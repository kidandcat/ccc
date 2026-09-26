package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// Grok Build (the ccc grok engine) is a SuperGrok OAuth session, not an xAI
// API key. The CLI's /usage card reads:
//
//	GET https://cli-chat-proxy.grok.com/v1/billing?format=credits
//
// with the access token in $GROK_HOME/auth.json. That is the same weekly pool
// grok.com Settings → Usage shows. The xAI Management API prepaid-balance
// route is API-key billing and does not apply to this login.

var grokBillingURL = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"

type grokAuth struct {
	Key          string `json:"key"`
	AuthMode     string `json:"auth_mode"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    string `json:"expires_at"`
	OIDCIssuer   string `json:"oidc_issuer"`
	OIDCClientID string `json:"oidc_client_id"`
}

type grokBillingPayload struct {
	Config *grokBillingConfig `json:"config"`
	grokBillingConfig
}

type grokBillingConfig struct {
	CreditUsagePercent *float64      `json:"creditUsagePercent"`
	CurrentPeriod      grokPeriod    `json:"currentPeriod"`
	BillingPeriodEnd   string        `json:"billingPeriodEnd"`
	ProductUsage       []grokProduct `json:"productUsage"`
	PrepaidBalance     *grokMoneyVal `json:"prepaidBalance"`
}

type grokPeriod struct {
	Type  string `json:"type"`
	Start string `json:"start"`
	End   string `json:"end"`
}

type grokProduct struct {
	Product      string   `json:"product"`
	UsagePercent *float64 `json:"usagePercent"`
}

type grokMoneyVal struct {
	Val json.RawMessage `json:"val"`
}

func fetchGrokUsage(p Profile) (profileUsage, error) {
	tok, err := grokAccessToken(p)
	if err != nil {
		return unknownProfileUsage(), err
	}
	u, status, err := grokBillingOnce(tok)
	if err != nil {
		return unknownProfileUsage(), err
	}
	if status != http.StatusOK {
		return unknownProfileUsage(), fmt.Errorf("grok billing: HTTP %d", status)
	}
	return u, nil
}

func grokBillingOnce(tok string) (profileUsage, int, error) {
	raw, status, err := httpJSON(http.MethodGet, grokBillingURL, "Bearer "+tok, nil, nil)
	if err != nil {
		return unknownProfileUsage(), status, err
	}
	if status != http.StatusOK {
		return unknownProfileUsage(), status, nil
	}
	var payload grokBillingPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return unknownProfileUsage(), status, err
	}
	cfg := payload.grokBillingConfig
	if payload.Config != nil {
		cfg = *payload.Config
	}
	return profileUsageFromGrok(cfg), status, nil
}

func profileUsageFromGrok(cfg grokBillingConfig) profileUsage {
	u := unknownProfileUsage()
	pct := grokPercent(cfg)
	if pct == nil {
		return u
	}
	name := grokPeriodName(cfg.CurrentPeriod.Type)
	reset := parseResetAt(cfg.CurrentPeriod.End)
	if reset.IsZero() {
		reset = parseResetAt(cfg.BillingPeriodEnd)
	}
	n := percentFromFloat(*pct)
	u.Windows = []usageWin{{Name: name, Percent: n, ResetAt: reset}}
	u.FiveHour = n
	u.FiveHourKnown = true
	u.FiveHourResetAt = reset
	if name == "week" || name == "7d" || name == "month" {
		u.SevenDay = n
		u.SevenDayKnown = true
	}
	return u
}

func grokPercent(cfg grokBillingConfig) *float64 {
	if cfg.CreditUsagePercent != nil {
		return cfg.CreditUsagePercent
	}
	for _, p := range cfg.ProductUsage {
		if strings.EqualFold(p.Product, "GrokBuild") && p.UsagePercent != nil {
			return p.UsagePercent
		}
	}
	for _, p := range cfg.ProductUsage {
		if p.UsagePercent != nil {
			return p.UsagePercent
		}
	}
	return nil
}

func grokPeriodName(t string) string {
	t = strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(t)), "USAGE_PERIOD_TYPE_")
	switch t {
	case "WEEKLY":
		return "week"
	case "DAILY":
		return "day"
	case "MONTHLY":
		return "month"
	case "HOURLY":
		return "hour"
	case "":
		return "week"
	default:
		return strings.ToLower(t)
	}
}

// grokAccessToken returns the stored access token only when it is still
// valid. Usage telemetry does not refresh or rewrite auth.json.
func grokAccessToken(p Profile) (string, error) {
	_, auth, _, err := loadGrokAuth(p)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(auth.Key) == "" || tokenExpired(grokExpiry(auth), time.Now()) {
		return "", errUsageTokenStale
	}
	return auth.Key, nil
}

func grokExpiry(a grokAuth) time.Time {
	if exp := jwtExpiry(a.Key); !exp.IsZero() {
		return exp
	}
	return parseResetAt(a.ExpiresAt)
}

func loadGrokAuth(p Profile) (entryKey string, auth grokAuth, raw []byte, err error) {
	path := grokAuthJSON(p)
	raw, err = os.ReadFile(path)
	if err != nil {
		return "", grokAuth{}, nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", grokAuth{}, raw, fmt.Errorf("grok auth.json: %w", err)
	}
	var bestKey string
	var best grokAuth
	now := time.Now()
	for k, blob := range m {
		var a grokAuth
		if json.Unmarshal(blob, &a) != nil || strings.TrimSpace(a.Key) == "" {
			continue
		}
		if best.Key == "" {
			bestKey, best = k, a
		}
		if !tokenExpired(grokExpiry(a), now) {
			return k, a, raw, nil
		}
	}
	if best.Key == "" {
		return "", grokAuth{}, raw, fmt.Errorf("no grok oauth credentials for %s", accountDisplay(p))
	}
	return bestKey, best, raw, nil
}
