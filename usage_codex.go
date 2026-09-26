package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// Codex in ccc is a ChatGPT-subscription login (isolated CODEX_HOME), not a
// platform API key. The CLI itself reads quota from
//
//	GET https://chatgpt.com/backend-api/wham/usage
//
// (Authorization: Bearer <access_token>, ChatGPT-Account-Id: <account_id>).
// App-server's account/rateLimits/read is the same data over JSON-RPC; we
// call the HTTP route Claude-style so /status does not have to spawn
// `codex app-server`. Verified against Codex CLI 0.154.0 and live Pro Lite.

var codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"

type codexAuthFile struct {
	AuthMode     string       `json:"auth_mode"`
	OpenAIAPIKey *string      `json:"OPENAI_API_KEY"`
	Tokens       *codexTokens `json:"tokens"`
	LastRefresh  string       `json:"last_refresh"`
}

type codexTokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
}

type codexUsagePayload struct {
	PlanType  string          `json:"plan_type"`
	RateLimit *codexRateLimit `json:"rate_limit"`
	Credits   *codexCredits   `json:"credits"`
}

type codexRateLimit struct {
	PrimaryWindow   *codexWindow `json:"primary_window"`
	SecondaryWindow *codexWindow `json:"secondary_window"`
}

type codexWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int     `json:"limit_window_seconds"`
	ResetAfterSeconds  int     `json:"reset_after_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

type codexCredits struct {
	HasCredits bool     `json:"has_credits"`
	Unlimited  bool     `json:"unlimited"`
	Balance    *float64 `json:"balance"`
}

func fetchCodexUsage(p Profile) (profileUsage, error) {
	auth, _, err := loadCodexAuth(p)
	if err != nil {
		return unknownProfileUsage(), err
	}
	if reason := codexUsageNA(auth); reason != "" {
		return naProfileUsage(reason), nil
	}
	tok, err := codexAccessToken(p, auth)
	if err != nil {
		return unknownProfileUsage(), err
	}
	acct := ""
	if auth.Tokens != nil {
		acct = auth.Tokens.AccountID
	}
	u, status, err := codexUsageOnce(tok, acct)
	if err != nil {
		return unknownProfileUsage(), err
	}
	if status != http.StatusOK {
		return unknownProfileUsage(), fmt.Errorf("codex usage: HTTP %d", status)
	}
	return u, nil
}

func codexUsageNA(auth codexAuthFile) string {
	mode := strings.ToLower(strings.TrimSpace(auth.AuthMode))
	if auth.Tokens != nil && strings.TrimSpace(auth.Tokens.AccessToken) != "" {
		return ""
	}
	if mode == "apikey" || mode == "api_key" || (auth.OpenAIAPIKey != nil && strings.TrimSpace(*auth.OpenAIAPIKey) != "") {
		return "API key, not ChatGPT quota"
	}
	return ""
}

func codexUsageOnce(tok, accountID string) (profileUsage, int, error) {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	if accountID != "" {
		h.Set("ChatGPT-Account-Id", accountID)
	}
	raw, status, err := httpJSON(http.MethodGet, codexUsageURL, "Bearer "+tok, h, nil)
	if err != nil {
		return unknownProfileUsage(), status, err
	}
	if status != http.StatusOK {
		return unknownProfileUsage(), status, nil
	}
	var payload codexUsagePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return unknownProfileUsage(), status, err
	}
	return profileUsageFromCodex(payload), status, nil
}

func profileUsageFromCodex(p codexUsagePayload) profileUsage {
	u := unknownProfileUsage()
	if p.RateLimit == nil {
		return u
	}
	applyCodexWindow(&u, p.RateLimit.PrimaryWindow)
	applyCodexWindow(&u, p.RateLimit.SecondaryWindow)
	return u
}

func applyCodexWindow(u *profileUsage, w *codexWindow) {
	if w == nil {
		return
	}
	name := usageWindowName(w.LimitWindowSeconds)
	n := percentFromFloat(w.UsedPercent)
	reset := time.Time{}
	if w.ResetAt > 0 {
		reset = time.Unix(w.ResetAt, 0)
	} else if w.ResetAfterSeconds > 0 {
		// API countdown, frozen as an absolute time so the 5 min cache stays honest.
		reset = time.Now().Add(time.Duration(w.ResetAfterSeconds) * time.Second)
	}
	u.Windows = append(u.Windows, usageWin{Name: name, Percent: n, ResetAt: reset})
	if !u.FiveHourKnown {
		u.FiveHour = n
		u.FiveHourKnown = true
		u.FiveHourResetAt = reset
	}
	if name == "7d" || w.LimitWindowSeconds >= 2*86400 {
		u.SevenDay = n
		u.SevenDayKnown = true
		u.SevenDayResetAt = reset
	}
}

// codexAccessToken returns the stored ChatGPT access token only when it is
// still valid. Usage telemetry does not refresh or rewrite auth.json.
func codexAccessToken(p Profile, auth codexAuthFile) (string, error) {
	if auth.Tokens == nil || strings.TrimSpace(auth.Tokens.AccessToken) == "" {
		return "", fmt.Errorf("no codex chatgpt credentials for %s", accountDisplay(p))
	}
	if tokenExpired(jwtExpiry(auth.Tokens.AccessToken), time.Now()) {
		return "", errUsageTokenStale
	}
	return auth.Tokens.AccessToken, nil
}

func loadCodexAuth(p Profile) (codexAuthFile, []byte, error) {
	raw, err := os.ReadFile(codexAuthJSON(p))
	if err != nil {
		return codexAuthFile{}, nil, err
	}
	var auth codexAuthFile
	if err := json.Unmarshal(raw, &auth); err != nil {
		return codexAuthFile{}, raw, fmt.Errorf("codex auth.json: %w", err)
	}
	return auth, raw, nil
}
