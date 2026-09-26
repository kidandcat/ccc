package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const liveUsageJSON = `{
  "five_hour": {"utilization": 2.0, "resets_at": "2026-09-15T14:30:00.048430+00:00"},
  "seven_day": {"utilization": 14.0, "resets_at": "2026-09-17T18:00:00.048462+00:00"},
  "seven_day_opus": null,
  "limits": [
    {"kind": "session", "group": "session", "percent": 2, "resets_at": "2026-09-15T14:30:00.048430+00:00"},
    {"kind": "weekly_all", "group": "weekly", "percent": 14, "resets_at": "2026-09-17T18:00:00.048462+00:00"},
    {"kind": "weekly_scoped", "group": "weekly", "percent": 15, "resets_at": "2026-09-17T18:00:00.048797+00:00",
     "scope": {"model": {"display_name": "Fable"}}}
  ]
}`

func TestProfileUsageFromPayload(t *testing.T) {
	t.Run("float windows from the live API", func(t *testing.T) {
		var p oauthUsagePayload
		if err := json.Unmarshal([]byte(liveUsageJSON), &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		u := profileUsageFromPayload(p)
		if u.FiveHour != 2 || !u.FiveHourKnown {
			t.Errorf("5h = %d known=%v, want 2 known", u.FiveHour, u.FiveHourKnown)
		}
		if u.SevenDay != 14 || !u.SevenDayKnown {
			t.Errorf("7d = %d known=%v, want 14 known", u.SevenDay, u.SevenDayKnown)
		}
		if u.FiveHourResetAt.IsZero() {
			t.Error("5h resets_at not parsed")
		}
		if u.SevenDayResetAt.IsZero() {
			t.Error("7d resets_at not parsed")
		}
	})

	t.Run("limits array fills in when five_hour is null", func(t *testing.T) {
		raw := `{
		  "five_hour": null,
		  "seven_day": null,
		  "limits": [
		    {"kind": "session", "percent": 41, "resets_at": "2026-09-15T14:30:00Z"},
		    {"kind": "weekly_all", "percent": 9, "resets_at": "2026-09-17T18:00:00Z"}
		  ]
		}`
		var p oauthUsagePayload
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		u := profileUsageFromPayload(p)
		if u.FiveHour != 41 || !u.FiveHourKnown {
			t.Errorf("5h = %d known=%v, want 41 known", u.FiveHour, u.FiveHourKnown)
		}
		if u.SevenDay != 9 || !u.SevenDayKnown {
			t.Errorf("7d = %d known=%v, want 9 known", u.SevenDay, u.SevenDayKnown)
		}
		if u.FiveHourResetAt.IsZero() || u.SevenDayResetAt.IsZero() {
			t.Error("limits[].resets_at not parsed")
		}
	})

	t.Run("null five_hour resets_at is omitted, seven_day kept", func(t *testing.T) {
		// Live 2026-09-17: five_hour.resets_at JSON null, seven_day.resets_at set.
		raw := `{
		  "five_hour": {"utilization": 0.0, "resets_at": null},
		  "seven_day": {"utilization": 0.0, "resets_at": "2026-09-24T18:00:00+00:00"}
		}`
		var p oauthUsagePayload
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		u := profileUsageFromPayload(p)
		if !u.FiveHourResetAt.IsZero() {
			t.Errorf("5h reset = %v, want zero (API sent null)", u.FiveHourResetAt)
		}
		if u.SevenDayResetAt.IsZero() {
			t.Error("7d resets_at not parsed")
		}
		now := time.Date(2026, 9, 17, 18, 0, 0, 0, time.UTC)
		got := profileUsageLineAt(engineClaude, u, now)
		if got != "5h 0% · 7d 0% · reset 7d" {
			t.Errorf("line = %q", got)
		}
	})
}

func TestRefreshProfileUsageFetchesAPI(t *testing.T) {
	usageMemClear()
	t.Cleanup(usageMemClear)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/oauth/usage" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-ant-oat01-test" {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("anthropic-beta"); got != oauthUsageBeta {
			t.Errorf("beta = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveUsageJSON))
	}))
	t.Cleanup(srv.Close)

	oldURL, oldClient, oldSec := oauthUsageURL, usageHTTP, securityOutput
	oauthUsageURL = srv.URL + "/api/oauth/usage"
	usageHTTP = srv.Client()
	securityOutput = func(args ...string) ([]byte, error) { return nil, os.ErrNotExist }
	t.Cleanup(func() {
		oauthUsageURL = oldURL
		usageHTTP = oldClient
		securityOutput = oldSec
	})

	dir := t.TempDir()
	creds := `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-test","refreshToken":"sk-ant-ort01-test","expiresAt":` +
		farExpiryMS() + `,"scopes":["user:profile"]}}`
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(creds), 0600); err != nil {
		t.Fatal(err)
	}
	p := Profile{Name: "work", ConfigDir: dir}

	u := refreshProfileUsage(p)
	if u.FiveHour != 2 || u.SevenDay != 14 {
		t.Fatalf("got 5h=%d 7d=%d, want 2 and 14", u.FiveHour, u.SevenDay)
	}
	// Second call must be served from memory, not the server (which we now close).
	srv.Close()
	u2 := refreshProfileUsage(p)
	if u2.FiveHour != 2 || u2.SevenDay != 14 {
		t.Fatalf("cached 5h=%d 7d=%d", u2.FiveHour, u2.SevenDay)
	}
}

func TestPickOAuthPrefersLiveKeychainOverStaleFile(t *testing.T) {
	file := oauthBlob{oauth: &claudeAiOauth{AccessToken: "stale", ExpiresAt: 1}}
	keychain := oauthBlob{
		oauth:   &claudeAiOauth{AccessToken: "live", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()},
		service: keychainService,
		account: "jairo",
	}
	got, ok := pickOAuth(file, keychain, time.Now())
	if !ok || got.oauth.AccessToken != "live" || !got.fromKeychain() {
		t.Fatalf("got %+v ok=%v, want the live keychain token", got.oauth, ok)
	}
}

func TestPatchOAuthJSONKeepsSiblingKeys(t *testing.T) {
	raw := []byte(`{"mcpOAuth":{"slack":true},"claudeAiOauth":{"accessToken":"old","refreshToken":"r","expiresAt":1}}`)
	fresh := &claudeAiOauth{AccessToken: "new", RefreshToken: "r2", ExpiresAt: 9}
	out, err := patchOAuthJSON(raw, fresh)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if string(m["mcpOAuth"]) != `{"slack":true}` {
		t.Errorf("mcpOAuth lost: %s", m["mcpOAuth"])
	}
	var oauth claudeAiOauth
	if err := json.Unmarshal(m["claudeAiOauth"], &oauth); err != nil {
		t.Fatal(err)
	}
	if oauth.AccessToken != "new" {
		t.Errorf("access = %s", oauth.AccessToken)
	}
}

func TestUsageWindowName(t *testing.T) {
	cases := map[int]string{0: "win", 900: "15m", 18000: "5h", 604800: "7d"}
	for sec, want := range cases {
		if got := usageWindowName(sec); got != want {
			t.Errorf("usageWindowName(%d) = %q, want %q", sec, got, want)
		}
	}
}

func TestUsageLine(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	claude := profileUsage{FiveHour: 12, SevenDay: 40, FiveHourKnown: true, SevenDayKnown: true}
	if got := profileUsageLineAt(engineClaude, claude, now); got != "5h 12% · 7d 40%" {
		t.Errorf("claude = %q", got)
	}
	if got := profileUsageLine(engineClaude, unknownProfileUsage()); got != "5h ? · 7d ?" {
		t.Errorf("claude unknown = %q", got)
	}
	grok := profileUsage{Windows: []usageWin{{Name: "week", Percent: 3}}, FiveHour: 3, FiveHourKnown: true}
	if got := profileUsageLineAt(engineGrok, grok, now); got != "week 3%" {
		t.Errorf("grok = %q", got)
	}
	if got := profileUsageLine(engineAntigravity, naProfileUsage("no public usage endpoint")); got != "n/a (no public usage endpoint)" {
		t.Errorf("agy = %q", got)
	}
	if got := profileUsageLine(engineGrok, unknownProfileUsage()); got != "?" {
		t.Errorf("grok unknown = %q", got)
	}

	withReset := profileUsage{
		FiveHour: 62, FiveHourKnown: true, FiveHourResetAt: now.Add(80 * time.Minute),
		SevenDay: 40, SevenDayKnown: true, SevenDayResetAt: now.Add(3 * 24 * time.Hour),
	}
	if got := profileUsageLineAt(engineClaude, withReset, now); got != "5h 62% · reset 1h20m · 7d 40% · reset 3d" {
		t.Errorf("claude reset = %q", got)
	}
	grokReset := profileUsage{Windows: []usageWin{{Name: "week", Percent: 8, ResetAt: now.Add(5*24*time.Hour + 23*time.Hour)}}}
	if got := profileUsageLineAt(engineGrok, grokReset, now); got != "week 8% · reset 5d23h" {
		t.Errorf("grok reset = %q", got)
	}
	past := profileUsage{Windows: []usageWin{{Name: "5h", Percent: 18, ResetAt: now.Add(-time.Minute)}}}
	if got := profileUsageLineAt(engineCodex, past, now); got != "5h 18% · reset now" {
		t.Errorf("past reset = %q", got)
	}
}

func TestCompactResetDuration(t *testing.T) {
	cases := map[time.Duration]string{
		0:                            "now",
		-time.Second:                 "now",
		45 * time.Second:             "45s",
		20 * time.Minute:             "20m",
		80 * time.Minute:             "1h20m",
		2 * time.Hour:                "2h",
		3 * 24 * time.Hour:           "3d",
		3*24*time.Hour + 2*time.Hour: "3d2h",
	}
	for d, want := range cases {
		if got := compactResetDuration(d); got != want {
			t.Errorf("compactResetDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestProfileUsageFromGrokWeekly(t *testing.T) {
	pct := 3.0
	u := profileUsageFromGrok(grokBillingConfig{
		CreditUsagePercent: &pct,
		CurrentPeriod:      grokPeriod{Type: "USAGE_PERIOD_TYPE_WEEKLY", End: "2026-09-23T20:26:20.348972+00:00"},
	})
	if !u.FiveHourKnown || u.FiveHour != 3 {
		t.Fatalf("5h = %d known=%v", u.FiveHour, u.FiveHourKnown)
	}
	if len(u.Windows) != 1 || u.Windows[0].Name != "week" || u.Windows[0].Percent != 3 {
		t.Fatalf("windows = %+v", u.Windows)
	}
	if u.Windows[0].ResetAt.IsZero() {
		t.Fatal("currentPeriod.end not parsed")
	}
	now := time.Date(2026, 9, 17, 20, 26, 20, 0, time.UTC)
	if got := profileUsageLineAt(engineGrok, u, now); got != "week 3% · reset 6d" {
		t.Errorf("line = %q", got)
	}
}

func TestProfileUsageFromCodexWindows(t *testing.T) {
	u := profileUsageFromCodex(codexUsagePayload{RateLimit: &codexRateLimit{
		PrimaryWindow:   &codexWindow{UsedPercent: 18, LimitWindowSeconds: 18000, ResetAt: 1730947200},
		SecondaryWindow: &codexWindow{UsedPercent: 86, LimitWindowSeconds: 604800, ResetAt: 1730980800},
	}})
	now := time.Unix(1730940000, 0).UTC()
	if got := profileUsageLineAt(engineCodex, u, now); got != "5h 18% · reset 2h · 7d 86% · reset 11h20m" {
		t.Errorf("line = %q", got)
	}
	if u.FiveHour != 18 || u.SevenDay != 86 {
		t.Errorf("selection 5h=%d 7d=%d", u.FiveHour, u.SevenDay)
	}
	if u.SevenDayResetAt.IsZero() {
		t.Error("7d reset_at not copied")
	}

	weekly := profileUsageFromCodex(codexUsagePayload{RateLimit: &codexRateLimit{
		PrimaryWindow: &codexWindow{UsedPercent: 0, LimitWindowSeconds: 604800, ResetAt: 1790246266},
	}})
	if got := profileUsageLineAt(engineCodex, weekly, time.Unix(1790246266-3600, 0)); got != "7d 0% · reset 1h" {
		t.Errorf("weekly-only = %q", got)
	}

	relative := profileUsageFromCodex(codexUsagePayload{RateLimit: &codexRateLimit{
		PrimaryWindow: &codexWindow{UsedPercent: 4, LimitWindowSeconds: 18000, ResetAfterSeconds: 90 * 60},
	}})
	if len(relative.Windows) != 1 || relative.Windows[0].ResetAt.IsZero() {
		t.Fatalf("reset_after_seconds should freeze an absolute reset: %+v", relative.Windows)
	}
	left := time.Until(relative.Windows[0].ResetAt)
	if left < 85*time.Minute || left > 95*time.Minute {
		t.Errorf("reset_after_seconds 90m froze as %v left", left)
	}
}

func TestRefreshProfileUsageGrokFetchesBilling(t *testing.T) {
	usageMemClear()
	t.Cleanup(usageMemClear)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/billing" || r.URL.RawQuery != "format=credits" {
			t.Errorf("path = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer grok-token" {
			t.Errorf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"config":{"creditUsagePercent":3.0,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-09-23T20:26:20Z"}}}`))
	}))
	t.Cleanup(srv.Close)

	oldURL, oldClient := grokBillingURL, usageHTTP
	grokBillingURL = srv.URL + "/v1/billing?format=credits"
	usageHTTP = srv.Client()
	t.Cleanup(func() {
		grokBillingURL = oldURL
		usageHTTP = oldClient
	})

	dir := t.TempDir()
	auth := `{"https://auth.x.ai::test":{"key":"grok-token","auth_mode":"oidc","expires_at":"` +
		time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano) + `"}}`
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(auth), 0600); err != nil {
		t.Fatal(err)
	}
	p := Profile{Name: "work", Engine: engineGrok, ConfigDir: dir}
	u := refreshProfileUsage(p)
	if u.FiveHour != 3 || !u.FiveHourKnown {
		t.Fatalf("got 5h=%d known=%v", u.FiveHour, u.FiveHourKnown)
	}
	got := profileUsageLine(engineGrok, u)
	if !strings.Contains(got, "week 3%") || !strings.Contains(got, "reset ") {
		t.Fatalf("got %q", got)
	}
	srv.Close()
	u2 := refreshProfileUsage(p)
	if !strings.Contains(profileUsageLine(engineGrok, u2), "week 3%") {
		t.Fatalf("cached %q", profileUsageLine(engineGrok, u2))
	}
}

func TestRefreshProfileUsageCodexFetchesWham(t *testing.T) {
	usageMemClear()
	t.Cleanup(usageMemClear)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/wham/usage" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer codex-token" {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("ChatGPT-Account-Id"); got != "acct-1" {
			t.Errorf("account-id = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":18,"limit_window_seconds":18000,"reset_at":1730947200},"secondary_window":{"used_percent":40,"limit_window_seconds":604800,"reset_at":1730980800}}}`))
	}))
	t.Cleanup(srv.Close)

	oldURL, oldClient := codexUsageURL, usageHTTP
	codexUsageURL = srv.URL + "/backend-api/wham/usage"
	usageHTTP = srv.Client()
	t.Cleanup(func() {
		codexUsageURL = oldURL
		usageHTTP = oldClient
	})

	dir := t.TempDir()
	auth := `{"auth_mode":"chatgpt","tokens":{"access_token":"codex-token","refresh_token":"rt","account_id":"acct-1"}}`
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(auth), 0600); err != nil {
		t.Fatal(err)
	}
	p := Profile{Name: "openai", Engine: engineCodex, ConfigDir: dir}
	u := refreshProfileUsage(p)
	got := profileUsageLine(engineCodex, u)
	if !strings.Contains(got, "5h 18%") || !strings.Contains(got, "7d 40%") || !strings.Contains(got, "reset ") {
		t.Fatalf("got %q", got)
	}
}

func TestRefreshProfileUsageAntigravityIsNA(t *testing.T) {
	p := Profile{Name: "lab", Engine: engineAntigravity, ConfigDir: t.TempDir()}
	u := refreshProfileUsage(p)
	if u.Unavailable != "no public usage endpoint" {
		t.Fatalf("unavailable = %q", u.Unavailable)
	}
	if profileUsageLine(engineAntigravity, u) != "n/a (no public usage endpoint)" {
		t.Fatalf("line = %q", profileUsageLine(engineAntigravity, u))
	}
}

func TestCodexAPIKeyHasNoQuota(t *testing.T) {
	key := "sk-test"
	auth := codexAuthFile{AuthMode: "apikey", OpenAIAPIKey: &key}
	if got := codexUsageNA(auth); got != "API key, not ChatGPT quota" {
		t.Fatalf("got %q", got)
	}
}

func farExpiryMS() string {
	return jsonNumber(time.Now().Add(8 * time.Hour).UnixMilli())
}

// credWriteProbe fails the test if usage telemetry refreshes or persists
// credentials. securityAdd and writeFileAtomic are the two write hooks.
type credWriteProbe struct {
	adds   atomic.Int32
	writes atomic.Int32
	hits   atomic.Int32
}

func (p *credWriteProbe) install(t *testing.T) *httptest.Server {
	t.Helper()
	usageMemClear()
	prevAdd := securityAdd
	prevWrite := writeFileAtomic
	prevHTTP := usageHTTP
	prevUsage := oauthUsageURL
	prevGrok := grokBillingURL
	prevCodex := codexUsageURL
	prevReader := oauthKeychainReader
	prevSec := securityOutput
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.hits.Add(1)
		http.Error(w, "usage telemetry must not call the network for a stale token", http.StatusInternalServerError)
	}))
	securityAdd = func(service, account string, secret []byte) error {
		p.adds.Add(1)
		return fmt.Errorf("securityAdd %s/%s", service, account)
	}
	writeFileAtomic = func(path string, data []byte, perm os.FileMode) error {
		p.writes.Add(1)
		return fmt.Errorf("writeFileAtomic %s", path)
	}
	usageHTTP = srv.Client()
	oauthUsageURL = srv.URL + "/api/oauth/usage"
	grokBillingURL = srv.URL + "/v1/billing"
	codexUsageURL = srv.URL + "/backend-api/wham/usage"
	securityOutput = func(args ...string) ([]byte, error) { return nil, os.ErrNotExist }
	oauthKeychainReader = func(Profile) (oauthBlob, bool) { return oauthBlob{}, false }
	t.Cleanup(func() {
		securityAdd = prevAdd
		writeFileAtomic = prevWrite
		usageHTTP = prevHTTP
		oauthUsageURL = prevUsage
		grokBillingURL = prevGrok
		codexUsageURL = prevCodex
		oauthKeychainReader = prevReader
		securityOutput = prevSec
		srv.Close()
		usageMemClear()
	})
	return srv
}

func (p *credWriteProbe) assertQuiet(t *testing.T) {
	t.Helper()
	if n := p.adds.Load(); n != 0 {
		t.Errorf("securityAdd called %d times", n)
	}
	if n := p.writes.Load(); n != 0 {
		t.Errorf("writeFileAtomic called %d times", n)
	}
	if n := p.hits.Load(); n != 0 {
		t.Errorf("usage HTTP called %d times", n)
	}
}

func TestRefreshProfileUsageStaleClaudeFileDoesNotWrite(t *testing.T) {
	var probe credWriteProbe
	probe.install(t)
	dir := t.TempDir()
	creds := `{"claudeAiOauth":{"accessToken":"stale-access","refreshToken":"stale-refresh","expiresAt":1}}`
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(creds), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := `{"cachedUsageUtilization":{"utilization":{"five_hour":{"utilization":12},"seven_day":{"utilization":40}}}}`
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}
	p := Profile{Name: "work", ConfigDir: dir}
	t.Cleanup(func() {
		cooldownMu.Lock()
		delete(cooldowns, p.Name)
		cooldownMu.Unlock()
	})
	u := refreshProfileUsage(p)
	probe.assertQuiet(t)
	if u.FiveHour != 12 || !u.FiveHourKnown || u.SevenDay != 40 || !u.SevenDayKnown {
		t.Fatalf("stale token should degrade to the on-disk snapshot, got %+v", u)
	}
	noteProfileLimit(p, time.Now())
	probe.assertQuiet(t)
}

func TestRefreshProfileUsageStaleKeychainDoesNotWrite(t *testing.T) {
	var probe credWriteProbe
	probe.install(t)
	raw := []byte(`{"mcpOAuth":{"slack":true},"claudeAiOauth":{"accessToken":"stale-access","refreshToken":"stale-refresh","expiresAt":1}}`)
	oauthKeychainReader = func(Profile) (oauthBlob, bool) {
		return oauthBlob{
			oauth:   oauthFromJSON(raw),
			raw:     raw,
			service: keychainService,
			account: "tester",
		}, true
	}
	p := Profile{Name: "work", ConfigDir: t.TempDir()}
	u := refreshProfileUsage(p)
	probe.assertQuiet(t)
	if u.FiveHourKnown || u.SevenDayKnown {
		t.Fatalf("stale keychain token should degrade to unknown, got %+v", u)
	}
	if got := profileUsageLine(engineClaude, u); got != "5h ? · 7d ?" {
		t.Fatalf("line = %q", got)
	}
}

func TestRefreshProfileUsageStaleGrokAndCodexDoNotWrite(t *testing.T) {
	var probe credWriteProbe
	probe.install(t)

	t.Run("grok", func(t *testing.T) {
		dir := t.TempDir()
		auth := `{"https://auth.x.ai::test":{"key":"grok-token","auth_mode":"oidc","refresh_token":"rt","expires_at":"2000-01-01T00:00:00Z","oidc_client_id":"cid"}}`
		if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(auth), 0o600); err != nil {
			t.Fatal(err)
		}
		u := refreshProfileUsage(Profile{Name: "grok", Engine: engineGrok, ConfigDir: dir})
		if u.FiveHourKnown || len(u.Windows) > 0 {
			t.Fatalf("stale grok token should degrade, got %+v", u)
		}
	})
	t.Run("codex", func(t *testing.T) {
		dir := t.TempDir()
		tok := unsignedJWT(time.Now().Add(-time.Hour).Unix())
		auth := `{"auth_mode":"chatgpt","tokens":{"access_token":"` + tok + `","refresh_token":"rt","account_id":"acct-1"}}`
		if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(auth), 0o600); err != nil {
			t.Fatal(err)
		}
		u := refreshProfileUsage(Profile{Name: "codex", Engine: engineCodex, ConfigDir: dir})
		if u.FiveHourKnown || len(u.Windows) > 0 {
			t.Fatalf("stale codex token should degrade, got %+v", u)
		}
	})
	probe.assertQuiet(t)
}

func TestRefreshProfileUsageConcurrentStaleToken(t *testing.T) {
	var probe credWriteProbe
	probe.install(t)
	dir := t.TempDir()
	creds := `{"claudeAiOauth":{"accessToken":"stale-access","refreshToken":"stale-refresh","expiresAt":1}}`
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(creds), 0o600); err != nil {
		t.Fatal(err)
	}
	p := Profile{Name: "work", ConfigDir: dir}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = refreshProfileUsage(p)
		}()
	}
	wg.Wait()
	probe.assertQuiet(t)
}

func TestUsageMemConcurrent(t *testing.T) {
	usageMemClear()
	t.Cleanup(usageMemClear)
	p := Profile{Name: "race", ConfigDir: t.TempDir()}
	u := profileUsage{FiveHour: 3, FiveHourKnown: true}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			usageMemPut(p, u)
			if got, ok := usageMemGet(p); ok && got.FiveHour != 3 {
				t.Errorf("usageMemGet = %+v", got)
			}
		}()
	}
	wg.Wait()
}

func TestKeychainServiceName(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Fatal(err)
	}
	if got := keychainServiceName(implicitProfile()); got != keychainService {
		t.Fatalf("implicit = %q", got)
	}
	bare := Profile{ConfigDir: filepath.Join(home, ".claude")}
	if got := keychainServiceName(bare); got != keychainService {
		t.Fatalf("default ~/.claude = %q", got)
	}
	isolated := Profile{ConfigDir: t.TempDir()}
	got := keychainServiceName(isolated)
	if got == keychainService || !strings.HasPrefix(got, keychainService+"-") || len(got) != len(keychainService)+1+8 {
		t.Fatalf("isolated = %q", got)
	}
}

func TestUsageCallSitesDoNotRefreshCredentials(t *testing.T) {
	graph := funcCallGraph(t)
	forbidden := []string{
		"securityAdd", "saveClaudeOAuth", "refreshClaudeOAuth", "claudeAccessToken",
		"saveGrokAuth", "refreshGrokAuth", "grokRefreshAndSave",
		"saveCodexTokens", "refreshCodexAuth", "codexRefreshAndSave",
	}
	roots := []string{
		"refreshProfileUsage", "fetchProfileUsage", "fetchClaudeUsage", "fetchGrokUsage", "fetchCodexUsage",
		"claudeAccessTokenReadOnly", "pickSpawnEngine", "runDoctor", "noteProfileLimit",
		"collectProfileRows", "collectAccountCards", "doctorProfiles",
	}
	for _, root := range roots {
		if _, ok := graph.calls[root]; !ok {
			t.Errorf("call graph is missing %s", root)
			continue
		}
		for _, bad := range forbidden {
			if path := graph.reachPath(root, bad); path != nil {
				t.Errorf("%s can reach %s: %s", root, bad, strings.Join(path, " -> "))
			}
		}
	}
	noWrite := []string{
		"refreshProfileUsage", "fetchProfileUsage", "fetchClaudeUsage", "fetchGrokUsage", "fetchCodexUsage",
		"claudeAccessTokenReadOnly", "pickSpawnEngine", "noteProfileLimit",
	}
	for _, root := range noWrite {
		if path := graph.reachPath(root, "writeFileAtomic"); path != nil {
			t.Errorf("%s can reach writeFileAtomic: %s", root, strings.Join(path, " -> "))
		}
	}
	if path := graph.reachPath("pickSpawnEngine", "refreshProfileUsage"); path != nil {
		t.Errorf("pickSpawnEngine still refreshes usage: %s", strings.Join(path, " -> "))
	}
}

// callGraph is a name-level call graph. Only package functions are followed.
// Method calls are recorded, so a direct securityAdd/writeFileAtomic is seen,
// but a selector named Run is not treated as scheduler.Run: that collision
// walks from exec.Cmd.Run into the doctor and every turn spawn.
type callGraph struct {
	funcs map[string]bool
	calls map[string]map[string]bool
}

func funcCallGraph(t *testing.T) callGraph {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(info os.FileInfo) bool {
		name := info.Name()
		return strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg := pkgs["main"]
	if pkg == nil {
		t.Fatal("package main not parsed")
	}
	g := callGraph{funcs: map[string]bool{}, calls: map[string]map[string]bool{}}
	for _, file := range pkg.Files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Name == nil {
				continue
			}
			name := fn.Name.Name
			if fn.Recv == nil {
				g.funcs[name] = true
			}
			set := g.calls[name]
			if set == nil {
				set = map[string]bool{}
				g.calls[name] = set
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					set[fun.Name] = true
				case *ast.SelectorExpr:
					set[fun.Sel.Name] = true
				}
				return true
			})
		}
	}
	return g
}

func (g callGraph) reachPath(root, target string) []string {
	seen := map[string]bool{}
	var walk func(string) []string
	walk = func(name string) []string {
		if seen[name] {
			return nil
		}
		seen[name] = true
		for next := range g.calls[name] {
			if next == target {
				return []string{name, next}
			}
			if g.funcs[next] {
				if path := walk(next); path != nil {
					return append([]string{name}, path...)
				}
			}
		}
		return nil
	}
	return walk(root)
}

func unsignedJWT(exp int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp)))
	return header + "." + payload + ".x"
}

func jsonNumber(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
