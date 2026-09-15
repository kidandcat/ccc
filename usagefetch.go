package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// usagefetch.go is how /account and the doctor learn a Claude account's
// 5h/7d utilization. Claude Code 2.1.x still *reads* cachedUsageUtilization
// from .claude.json, but it often never writes it (the cache lives in memory
// and on GET /api/oauth/usage). ccc therefore fetches the same endpoint
// Claude Code's /status uses, with the profile's OAuth token, and keeps a
// 5-minute snapshot so chooseProfile can see numbers without hitting the
// (rate-limited) API on every turn.

const (
	usageFetchTTL   = 5 * time.Minute
	oauthUsageBeta  = "oauth-2025-04-20"
	oauthClientID   = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	keychainService = "Claude Code-credentials"
	tokenSkew       = 60 * time.Second
)

var (
	oauthUsageURL = "https://api.anthropic.com/api/oauth/usage"
	oauthTokenURL = "https://platform.claude.com/v1/oauth/token"
	usageHTTP     = &http.Client{Timeout: 5 * time.Second}

	usageMemMu sync.Mutex
	usageMem   = map[string]usageMemEntry{}
)

type usageMemEntry struct {
	usage   profileUsage
	fetched time.Time
}

func usageMemKey(p Profile) string {
	return profileEngine(p) + "\x00" + claudeHome(p)
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

func usageMemPut(p Profile, u profileUsage) {
	if !u.FiveHourKnown && !u.SevenDayKnown {
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

// refreshProfileUsage is the on-demand path (/account, doctor, `ccc profile
// list`): return a fresh snapshot, fetching from Anthropic when the memory
// cache is cold. Failures fall back to the on-disk cache so a blip does not
// blank the card.
func refreshProfileUsage(p Profile) profileUsage {
	if profileEngine(p) != engineClaude {
		return unknownProfileUsage()
	}
	if u, ok := usageMemGet(p); ok {
		return u
	}
	if u, err := fetchProfileUsage(p); err == nil && (u.FiveHourKnown || u.SevenDayKnown) {
		usageMemPut(p, u)
		return u
	}
	return readProfileUsageFromFile(p)
}

func fetchProfileUsage(p Profile) (profileUsage, error) {
	tok, err := claudeAccessToken(p)
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

func claudeAccessToken(p Profile) (string, error) {
	blob, err := loadClaudeOAuth(p)
	if err != nil {
		return "", err
	}
	if !blob.oauth.expired(time.Now()) {
		return blob.oauth.AccessToken, nil
	}
	if blob.oauth.RefreshToken == "" {
		return "", fmt.Errorf("oauth token expired and no refresh token")
	}
	fresh, err := refreshClaudeOAuth(blob.oauth)
	if err != nil {
		return "", err
	}
	if err := saveClaudeOAuth(blob, fresh); err != nil {
		// Still usable this call even if we could not persist the rotation.
		hookLog("could not persist refreshed oauth token: %v", err)
	}
	return fresh.AccessToken, nil
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
	if runtime.GOOS == "darwin" {
		if b, ok := readClaudeOAuthKeychain(p); ok {
			keychain = b
		}
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

func saveClaudeOAuth(blob oauthBlob, oauth *claudeAiOauth) error {
	raw, err := patchOAuthJSON(blob.raw, oauth)
	if err != nil {
		return err
	}
	if blob.fromKeychain() {
		return securityAdd(blob.service, blob.account, raw)
	}
	if blob.file == "" {
		return fmt.Errorf("no credentials path to write")
	}
	return writeFileAtomic(blob.file, raw, 0o600)
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

func refreshClaudeOAuth(oauth *claudeAiOauth) (*claudeAiOauth, error) {
	cid := strings.TrimSpace(oauth.ClientID)
	if cid == "" {
		cid = oauthClientID
	}
	body, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": oauth.RefreshToken,
		"client_id":     cid,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, oauthTokenURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-beta", oauthUsageBeta)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ccc")
	resp, err := usageHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oauth refresh: HTTP %d", resp.StatusCode)
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil {
		return nil, err
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("oauth refresh: empty access_token")
	}
	fresh := *oauth
	fresh.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		fresh.RefreshToken = tok.RefreshToken
	}
	if tok.ExpiresIn > 0 {
		fresh.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).UnixMilli()
	}
	if tok.Scope != "" {
		fresh.Scopes = strings.Fields(tok.Scope)
	}
	return &fresh, nil
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

// securityOutput runs `security find-generic-password … -w`. Overridable in tests.
var securityOutput = func(args ...string) ([]byte, error) {
	cmdArgs := append([]string{"find-generic-password"}, args...)
	cmdArgs = append(cmdArgs, "-w")
	return exec.Command("security", cmdArgs...).Output()
}

var securityAdd = func(service, account string, secret []byte) error {
	cmd := exec.Command("security", "add-generic-password", "-U", "-a", account, "-s", service, "-w", string(secret))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("security add-generic-password: %w (%s)", err, truncate(string(out), 120))
	}
	return nil
}
