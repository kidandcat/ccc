package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// usagefetch.go is how /account, /status and the doctor learn an account's
// rate/usage limits. Claude: GET /api/oauth/usage (5h/7d, resets_at). Grok
// Build: GET cli-chat-proxy.grok.com/v1/billing?format=credits (weekly
// SuperGrok pool, currentPeriod.end). Codex: GET chatgpt.com/backend-api/
// wham/usage (ChatGPT windows, reset_at / reset_after_seconds). Antigravity
// has no public usage endpoint and is shown as n/a. Snapshots live 5 minutes
// so chooseProfile can see numbers without hitting the (rate-limited) APIs
// on every turn.

const (
	usageFetchTTL   = 5 * time.Minute
	oauthUsageBeta  = "oauth-2025-04-20"
	keychainService = "Claude Code-credentials"
	tokenSkew       = 60 * time.Second
)

// errUsageTokenStale means the stored access token is missing or expired.
// Usage telemetry must not refresh or persist credentials to replace it.
// Callers degrade to the on-disk snapshot or an unknown reading.
var errUsageTokenStale = errors.New("oauth access token is stale")

var (
	oauthUsageURL = "https://api.anthropic.com/api/oauth/usage"
	usageHTTP     = &http.Client{Timeout: 5 * time.Second}

	usageMemMu sync.Mutex
	usageMem   = map[string]usageMemEntry{}
)

type usageMemEntry struct {
	usage   profileUsage
	fetched time.Time
}

func usageMemKey(p Profile) string {
	return profileEngine(p) + "\x00" + engineHome(p)
}

func usageMemGet(p Profile) (profileUsage, bool) {
	usageMemMu.Lock()
	defer usageMemMu.Unlock()
	e, ok := usageMem[usageMemKey(p)]
	if !ok || time.Since(e.fetched) > usageFetchTTL {
		return profileUsage{}, false
	}
	return e.usage, true
}

func usageCacheable(u profileUsage) bool {
	return u.FiveHourKnown || u.SevenDayKnown || len(u.Windows) > 0
}

func usageMemPut(p Profile, u profileUsage) {
	if !usageCacheable(u) {
		return
	}
	usageMemMu.Lock()
	usageMem[usageMemKey(p)] = usageMemEntry{usage: u, fetched: time.Now()}
	usageMemMu.Unlock()
}

func usageMemClear() {
	usageMemMu.Lock()
	usageMem = map[string]usageMemEntry{}
	usageMemMu.Unlock()
}

// refreshProfileUsage is the on-demand path (/account, /status, doctor,
// `ccc profile list`): return a fresh snapshot. Claude hits Anthropic
// GET /api/oauth/usage; Grok hits cli-chat-proxy billing; Codex hits
// ChatGPT GET /backend-api/wham/usage. Failures fall back to Claude's
// on-disk cache (other engines have none) so a blip does not blank the card.
//
// This path never refreshes or writes OAuth credentials. A stale access
// token degrades the cosmetic 5h/7d line. Refreshing here used to rotate
// the refresh token and overwrite the keychain item Claude Code itself
// uses; a CCC-only lock would not coordinate with that other process, so
// the write is gone rather than wrapped.
func refreshProfileUsage(p Profile) profileUsage {
	if profileEngine(p) == engineAntigravity {
		return naProfileUsage("no public usage endpoint")
	}
	if u, ok := usageMemGet(p); ok {
		return u
	}
	if u, err := fetchProfileUsage(p); err == nil && usageCacheable(u) {
		usageMemPut(p, u)
		return u
	} else if err == nil && u.Unavailable != "" {
		return u
	}
	return readProfileUsageFromFile(p)
}

func fetchProfileUsage(p Profile) (profileUsage, error) {
	switch profileEngine(p) {
	case engineGrok:
		return fetchGrokUsage(p)
	case engineCodex:
		return fetchCodexUsage(p)
	case engineAntigravity:
		return naProfileUsage("no public usage endpoint"), nil
	}
	return fetchClaudeUsage(p)
}

func fetchClaudeUsage(p Profile) (profileUsage, error) {
	tok, err := claudeAccessTokenReadOnly(p)
	if err != nil {
		return unknownProfileUsage(), err
	}
	req, err := http.NewRequest(http.MethodGet, oauthUsageURL, nil)
	if err != nil {
		return unknownProfileUsage(), err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("anthropic-beta", oauthUsageBeta)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ccc")
	resp, err := usageHTTP.Do(req)
	if err != nil {
		return unknownProfileUsage(), err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return unknownProfileUsage(), err
	}
	if resp.StatusCode != http.StatusOK {
		return unknownProfileUsage(), fmt.Errorf("oauth usage: HTTP %d", resp.StatusCode)
	}
	var payload oauthUsagePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return unknownProfileUsage(), err
	}
	return profileUsageFromPayload(payload), nil
}

// ---------------------------------------------------------------------------
// Credentials: file under the profile's config dir, else macOS keychain.
// ---------------------------------------------------------------------------

type claudeAiOauth struct {
	AccessToken           string   `json:"accessToken"`
	RefreshToken          string   `json:"refreshToken"`
	ExpiresAt             int64    `json:"expiresAt"`
	RefreshTokenExpiresAt int64    `json:"refreshTokenExpiresAt,omitempty"`
	Scopes                []string `json:"scopes"`
	SubscriptionType      string   `json:"subscriptionType,omitempty"`
	RateLimitTier         string   `json:"rateLimitTier,omitempty"`
	ClientID              string   `json:"clientId,omitempty"`
}

type claudeCredentialsFile struct {
	ClaudeAiOauth *claudeAiOauth `json:"claudeAiOauth"`
}

func (o *claudeAiOauth) expired(now time.Time) bool {
	if o == nil || o.AccessToken == "" {
		return true
	}
	if o.ExpiresAt == 0 {
		return false
	}
	return !now.Before(time.UnixMilli(o.ExpiresAt).Add(-tokenSkew))
}

// claudeAccessTokenReadOnly returns an access token only when one is already
// valid. It does not refresh and it does not write. It is the only token
// path usage telemetry, the doctor, and spawn may use.
func claudeAccessTokenReadOnly(p Profile) (string, error) {
	blob, err := loadClaudeOAuth(p)
	if err != nil {
		return "", err
	}
	if blob.oauth.expired(time.Now()) {
		return "", errUsageTokenStale
	}
	return blob.oauth.AccessToken, nil
}

// oauthBlob is one credentials document plus enough to write it back without
// dropping sibling keys (macOS keychain items also hold mcpOAuth).
type oauthBlob struct {
	oauth   *claudeAiOauth
	raw     []byte
	file    string
	service string
	account string
}

func (b oauthBlob) fromKeychain() bool { return b.service != "" }

func loadClaudeOAuth(p Profile) (oauthBlob, error) {
	var file, keychain oauthBlob
	if b, ok := readClaudeOAuthFile(p); ok {
		file = b
	}
	if b, ok := oauthKeychainReader(p); ok {
		keychain = b
	}
	if b, ok := pickOAuth(file, keychain, time.Now()); ok {
		return b, nil
	}
	return oauthBlob{}, fmt.Errorf("no claude oauth credentials for %s", accountDisplay(p))
}

// pickOAuth prefers a still-valid token. A leftover plaintext credentials.json
// can sit expired for months while the live token lives in the keychain.
func pickOAuth(file, keychain oauthBlob, now time.Time) (oauthBlob, bool) {
	switch {
	case file.oauth != nil && !file.oauth.expired(now):
		return file, true
	case keychain.oauth != nil && !keychain.oauth.expired(now):
		return keychain, true
	case keychain.oauth != nil:
		return keychain, true
	case file.oauth != nil:
		return file, true
	default:
		return oauthBlob{}, false
	}
}

func readClaudeOAuthFile(p Profile) (oauthBlob, bool) {
	home := claudeHome(p)
	for _, name := range []string{".credentials.json", "credentials.json"} {
		path := filepath.Join(home, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		oauth := oauthFromJSON(data)
		if oauth == nil {
			continue
		}
		return oauthBlob{oauth: oauth, raw: data, file: path}, true
	}
	return oauthBlob{}, false
}

func readClaudeOAuthKeychain(p Profile) (oauthBlob, bool) {
	svc := keychainServiceName(p)
	acct := keychainAccount()
	if acct == "" {
		return oauthBlob{}, false
	}
	data, err := securityOutput("-s", svc, "-a", acct)
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return oauthBlob{}, false
	}
	oauth := oauthFromJSON(data)
	if oauth == nil {
		return oauthBlob{}, false
	}
	return oauthBlob{oauth: oauth, raw: data, service: svc, account: acct}, true
}

func oauthFromJSON(data []byte) *claudeAiOauth {
	var creds claudeCredentialsFile
	if json.Unmarshal(data, &creds) != nil || creds.ClaudeAiOauth == nil || creds.ClaudeAiOauth.AccessToken == "" {
		return nil
	}
	return creds.ClaudeAiOauth
}

func patchOAuthJSON(raw []byte, oauth *claudeAiOauth) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	b, err := json.Marshal(oauth)
	if err != nil {
		return nil, err
	}
	m["claudeAiOauth"] = b
	return json.Marshal(m)
}

func keychainServiceName(p Profile) string {
	home, _ := os.UserHomeDir() // safe-ignore: empty home falls through to the profile dir below
	defaultDir := filepath.Join(home, ".claude")
	dir := claudeHome(p)
	if p.Implicit || dir == "" || filepath.Clean(dir) == filepath.Clean(defaultDir) {
		return keychainService
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	abs = filepath.Clean(abs)
	sum := sha256.Sum256([]byte(abs))
	return keychainService + "-" + hex.EncodeToString(sum[:])[:8]
}

func keychainAccount() string {
	if u := strings.TrimSpace(os.Getenv("USER")); u != "" {
		return u
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return ""
}

// oauthKeychainReader loads the macOS keychain item. Tests replace it; on
// other operating systems the default reads nothing.
var oauthKeychainReader = func(p Profile) (oauthBlob, bool) {
	if runtime.GOOS != "darwin" {
		return oauthBlob{}, false
	}
	return readClaudeOAuthKeychain(p)
}

// securityOutput runs `security find-generic-password … -w`. Overridable in tests.
var securityOutput = func(args ...string) ([]byte, error) {
	cmdArgs := append([]string{"find-generic-password"}, args...)
	cmdArgs = append(cmdArgs, "-w")
	return exec.Command("security", cmdArgs...).Output()
}

// securityAdd writes a generic password to the macOS keychain. Nothing in
// the usage, doctor, or spawn path may call it: refreshing a shared OAuth
// item races Claude Code. Tests replace the var and fail if it runs.
var securityAdd = func(service, account string, secret []byte) error {
	// -w as the last flag reads the password from stdin (twice: enter and
	// confirm). Putting the JSON on argv shows the refresh token in ps.
	cmd := exec.Command("security", "add-generic-password", "-U", "-a", account, "-s", service, "-w")
	pw := append(append([]byte{}, secret...), '\n')
	cmd.Stdin = bytes.NewReader(append(append([]byte{}, pw...), pw...))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("security add-generic-password: %w (%s)", err, truncate(string(out), 120))
	}
	return nil
}

// jwtExpiry reads exp from an unverified JWT payload. Signature checks belong
// to the issuer; usage telemetry only needs to know whether the access token
// is still usable. A stale token is not refreshed.
func jwtExpiry(tok string) time.Time {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload := parts[1]
	if m := len(payload) % 4; m != 0 {
		payload += strings.Repeat("=", 4-m)
	}
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return time.Time{}
	}
	var c struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &c) != nil || c.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(c.Exp, 0)
}

func tokenExpired(exp time.Time, now time.Time) bool {
	if exp.IsZero() {
		return false
	}
	return !now.Before(exp.Add(-tokenSkew))
}

func httpJSON(method, rawURL, auth string, extra http.Header, body []byte) ([]byte, int, error) {
	var rdr io.Reader
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, rawURL, rdr)
	if err != nil {
		return nil, 0, err
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ccc")
	for k, vs := range extra {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	resp, err := usageHTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return raw, resp.StatusCode, nil
}
