package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newFixtureProfile writes a profile config dir with the given .claude.json and
// settings.json contents ("" = do not create the file).
func newFixtureProfile(t *testing.T, name, claudeJSON, settingsJSON string) Profile {
	t.Helper()
	dir := t.TempDir()
	if claudeJSON != "" {
		if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(claudeJSON), 0600); err != nil {
			t.Fatalf("write .claude.json: %v", err)
		}
	}
	if settingsJSON != "" {
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(settingsJSON), 0600); err != nil {
			t.Fatalf("write settings.json: %v", err)
		}
	}
	return Profile{Name: name, ConfigDir: dir}
}

// usageFixture is the shape Claude Code writes; only the fields ccc reads are
// filled in, plus noise to prove unknown fields are ignored.
const usageFixture = `{
  "hasCompletedOnboarding": true,
  "cachedUsageUtilization": {
    "fetchedAtMs": 1788010567241,
    "utilization": {
      "five_hour": {"utilization": 7, "resets_at": "2026-08-29T17:10:00.160901+00:00"},
      "seven_day": {"utilization": 42, "resets_at": "2026-09-03T15:00:00.160920+00:00"},
      "seven_day_opus": null
    }
  }
}`

func TestReadProfileUsage(t *testing.T) {
	t.Run("full cache", func(t *testing.T) {
		p := newFixtureProfile(t, "a", usageFixture, "")
		u := readProfileUsage(p)
		if u.FiveHour != 7 || !u.FiveHourKnown {
			t.Errorf("five_hour = %d (known=%v), want 7 known", u.FiveHour, u.FiveHourKnown)
		}
		if u.SevenDay != 42 || !u.SevenDayKnown {
			t.Errorf("seven_day = %d (known=%v), want 42 known", u.SevenDay, u.SevenDayKnown)
		}
		if u.FiveHourResetAt.IsZero() {
			t.Error("five_hour resets_at not parsed")
		}
	})

	t.Run("null utilization is unknown", func(t *testing.T) {
		p := newFixtureProfile(t, "b", `{"cachedUsageUtilization":{"utilization":{"five_hour":null,"seven_day":{"utilization":null}}}}`, "")
		u := readProfileUsage(p)
		if u.FiveHour != unknownUtilization || u.FiveHourKnown {
			t.Errorf("five_hour = %d (known=%v), want %d unknown", u.FiveHour, u.FiveHourKnown, unknownUtilization)
		}
	})

	t.Run("missing file is unknown", func(t *testing.T) {
		u := readProfileUsage(Profile{Name: "c", ConfigDir: filepath.Join(t.TempDir(), "nope")})
		if u.FiveHour != unknownUtilization || u.SevenDay != unknownUtilization {
			t.Errorf("got %+v, want both %d", u, unknownUtilization)
		}
	})

	t.Run("malformed json is unknown", func(t *testing.T) {
		p := newFixtureProfile(t, "d", `{not json`, "")
		if u := readProfileUsage(p); u.FiveHourKnown || u.FiveHour != unknownUtilization {
			t.Errorf("got %+v, want unknown", u)
		}
	})

	t.Run("out-of-range percent is clamped", func(t *testing.T) {
		p := newFixtureProfile(t, "e", `{"cachedUsageUtilization":{"utilization":{"five_hour":{"utilization":250},"seven_day":{"utilization":-3}}}}`, "")
		u := readProfileUsage(p)
		if u.FiveHour != 100 || u.SevenDay != 0 {
			t.Errorf("got 5h=%d 7d=%d, want 100 and 0", u.FiveHour, u.SevenDay)
		}
	})

	t.Run("float percents from Claude Code 2.1.x", func(t *testing.T) {
		p := newFixtureProfile(t, "f", `{"cachedUsageUtilization":{"utilization":{"five_hour":{"utilization":7.0},"seven_day":{"utilization":42.4}}}}`, "")
		u := readProfileUsage(p)
		if u.FiveHour != 7 || !u.FiveHourKnown {
			t.Errorf("five_hour = %d (known=%v), want 7 known", u.FiveHour, u.FiveHourKnown)
		}
		if u.SevenDay != 42 || !u.SevenDayKnown {
			t.Errorf("seven_day = %d (known=%v), want 42 known", u.SevenDay, u.SevenDayKnown)
		}
	})
}

func TestChooseProfile(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tests := []struct {
		name  string
		stats []profileStat
		want  string
	}{
		{
			name:  "no profiles",
			stats: nil,
			want:  "",
		},
		{
			name: "lowest five-hour utilization wins",
			stats: []profileStat{
				{Name: "work", FiveHour: 80, SevenDay: 10},
				{Name: "work2", FiveHour: 12, SevenDay: 90},
			},
			want: "work2",
		},
		{
			name: "tie on utilization breaks on fewer working agents",
			stats: []profileStat{
				{Name: "a", FiveHour: 30, WorkingAgents: 4},
				{Name: "b", FiveHour: 30, WorkingAgents: 1},
			},
			want: "b",
		},
		{
			name: "full tie breaks on name",
			stats: []profileStat{
				{Name: "zeta", FiveHour: 30, WorkingAgents: 2},
				{Name: "alpha", FiveHour: 30, WorkingAgents: 2},
			},
			want: "alpha",
		},
		{
			name: "unknown utilization loses to a known-idle profile",
			stats: []profileStat{
				{Name: "known", FiveHour: 5},
				{Name: "unknown", FiveHour: unknownUtilization},
			},
			want: "known",
		},
		{
			name: "unknown utilization beats a known-busy profile",
			stats: []profileStat{
				{Name: "busy", FiveHour: 95},
				{Name: "unknown", FiveHour: unknownUtilization},
			},
			want: "unknown",
		},
		{
			name: "a cooling-down profile is skipped even when idler",
			stats: []profileStat{
				{Name: "limited", FiveHour: 1, CooledUntil: now.Add(10 * time.Minute)},
				{Name: "open", FiveHour: 70},
			},
			want: "open",
		},
		{
			name: "an expired cooldown does not exclude",
			stats: []profileStat{
				{Name: "expired", FiveHour: 1, CooledUntil: now.Add(-time.Minute)},
				{Name: "open", FiveHour: 70},
			},
			want: "expired",
		},
		{
			name: "when every profile is cooling down, take the one freeing up first",
			stats: []profileStat{
				{Name: "late", FiveHour: 1, CooledUntil: now.Add(2 * time.Hour)},
				{Name: "soon", FiveHour: 99, CooledUntil: now.Add(5 * time.Minute)},
			},
			want: "soon",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := chooseProfile(tt.stats, now); got != tt.want {
				t.Errorf("chooseProfile = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestChooseProfileIsDeterministic(t *testing.T) {
	now := time.Now()
	stats := []profileStat{
		{Name: "c", FiveHour: 10},
		{Name: "a", FiveHour: 10},
		{Name: "b", FiveHour: 10},
	}
	for i := 0; i < 20; i++ {
		if got := chooseProfile(stats, now); got != "a" {
			t.Fatalf("iteration %d: chooseProfile = %q, want %q", i, got, "a")
		}
	}
}

func TestProfileCooldown(t *testing.T) {
	cooldownMu.Lock()
	cooldowns = map[string]time.Time{}
	cooldownMu.Unlock()

	now := time.Now()
	// A profile with no cached resets_at falls back to the fixed cooldown.
	p := newFixtureProfile(t, "cool", `{}`, "")
	noteProfileLimit(p, now)
	until := profileCooledUntil("cool", now)
	if until.IsZero() {
		t.Fatal("profile should be on cooldown")
	}
	if d := until.Sub(now); d < defaultLimitCooldown-time.Second || d > defaultLimitCooldown+time.Second {
		t.Errorf("cooldown = %v, want ~%v", d, defaultLimitCooldown)
	}
	// Once it has elapsed the entry is forgotten.
	if got := profileCooledUntil("cool", now.Add(defaultLimitCooldown+time.Minute)); !got.IsZero() {
		t.Errorf("expired cooldown = %v, want zero", got)
	}
	if got := profileCooledUntil("never-limited", now); !got.IsZero() {
		t.Errorf("unknown profile cooldown = %v, want zero", got)
	}
}

func TestBypassAccepted(t *testing.T) {
	t.Run("settings.json flag", func(t *testing.T) {
		p := newFixtureProfile(t, "a", "", `{"skipDangerousModePermissionPrompt":true}`)
		if accepted, known := bypassAccepted(p); !accepted || !known {
			t.Errorf("got accepted=%v known=%v, want true true", accepted, known)
		}
	})
	t.Run("legacy .claude.json flag", func(t *testing.T) {
		p := newFixtureProfile(t, "b", `{"bypassPermissionsModeAccepted":true}`, `{}`)
		if accepted, known := bypassAccepted(p); !accepted || !known {
			t.Errorf("got accepted=%v known=%v, want true true", accepted, known)
		}
	})
	t.Run("present but false", func(t *testing.T) {
		p := newFixtureProfile(t, "c", `{}`, `{"skipDangerousModePermissionPrompt":false}`)
		accepted, known := bypassAccepted(p)
		if accepted || !known {
			t.Errorf("got accepted=%v known=%v, want false true", accepted, known)
		}
	})
	t.Run("fresh dir is unknown, not refused", func(t *testing.T) {
		p := Profile{Name: "d", ConfigDir: filepath.Join(t.TempDir(), "fresh")}
		accepted, known := bypassAccepted(p)
		if accepted || known {
			t.Errorf("got accepted=%v known=%v, want false false", accepted, known)
		}
	})
}

// TestAcceptBypassDisclaimer pins the direct write that replaced the PTY
// disclaimer driver (DESIGN §14.23): it must create the file when there is
// none, keep every other setting, be safe to run twice, and leave the file
// 0600 — a settings.json is next to the credentials of a real account.
func TestAcceptBypassDisclaimer(t *testing.T) {
	// settingsOf reads a profile's settings.json back as a map.
	settingsOf := func(t *testing.T, p Profile) map[string]any {
		t.Helper()
		data, err := os.ReadFile(profileSettings(p))
		if err != nil {
			t.Fatalf("read settings.json: %v", err)
		}
		var out map[string]any
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("settings.json is not valid JSON (%s): %v", data, err)
		}
		return out
	}
	mustBe0600 := func(t *testing.T, p Profile) {
		t.Helper()
		info, err := os.Stat(profileSettings(p))
		if err != nil {
			t.Fatalf("stat settings.json: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("settings.json mode = %v, want 0600", perm)
		}
	}

	t.Run("fresh config dir", func(t *testing.T) {
		// The dir does not exist yet either: a profile can be accepted before
		// claude has ever run under it.
		p := Profile{Name: "fresh", ConfigDir: filepath.Join(t.TempDir(), "cfg")}
		if err := acceptBypassDisclaimer(p); err != nil {
			t.Fatalf("acceptBypassDisclaimer: %v", err)
		}
		if got := settingsOf(t, p)[bypassSettingKey]; got != true {
			t.Errorf("%s = %v, want true", bypassSettingKey, got)
		}
		if accepted, known := bypassAccepted(p); !accepted || !known {
			t.Errorf("detector says accepted=%v known=%v, want true true", accepted, known)
		}
		mustBe0600(t, p)
	})

	t.Run("existing settings are preserved", func(t *testing.T) {
		p := newFixtureProfile(t, "keep", "", `{"model":"opus","env":{"FOO":"bar"},"skipDangerousModePermissionPrompt":false}`)
		if err := acceptBypassDisclaimer(p); err != nil {
			t.Fatalf("acceptBypassDisclaimer: %v", err)
		}
		got := settingsOf(t, p)
		if got["model"] != "opus" {
			t.Errorf("model = %v, want opus (other keys must survive)", got["model"])
		}
		env, ok := got["env"].(map[string]any)
		if !ok || env["FOO"] != "bar" {
			t.Errorf("env = %v, want {FOO: bar}", got["env"])
		}
		if got[bypassSettingKey] != true {
			t.Errorf("%s = %v, want true (an explicit false must be overwritten)", bypassSettingKey, got[bypassSettingKey])
		}
		mustBe0600(t, p)
	})

	t.Run("idempotent", func(t *testing.T) {
		p := Profile{Name: "twice", ConfigDir: t.TempDir()}
		if err := acceptBypassDisclaimer(p); err != nil {
			t.Fatalf("first call: %v", err)
		}
		first, err := os.ReadFile(profileSettings(p))
		if err != nil {
			t.Fatal(err)
		}
		if err := acceptBypassDisclaimer(p); err != nil {
			t.Fatalf("second call: %v", err)
		}
		second, err := os.ReadFile(profileSettings(p))
		if err != nil {
			t.Fatal(err)
		}
		if string(first) != string(second) {
			t.Errorf("a second call rewrote the file:\n%s\nvs\n%s", first, second)
		}
	})

	t.Run("empty file is not an error", func(t *testing.T) {
		p := newFixtureProfile(t, "empty", "", " \n")
		if err := acceptBypassDisclaimer(p); err != nil {
			t.Fatalf("acceptBypassDisclaimer: %v", err)
		}
		if got := settingsOf(t, p)[bypassSettingKey]; got != true {
			t.Errorf("%s = %v, want true", bypassSettingKey, got)
		}
	})

	t.Run("malformed settings are refused, not overwritten", func(t *testing.T) {
		p := newFixtureProfile(t, "broken", "", "{not json")
		if err := acceptBypassDisclaimer(p); err == nil {
			t.Fatal("a settings.json that cannot be parsed must not be replaced")
		}
		data, err := os.ReadFile(profileSettings(p))
		if err != nil || string(data) != "{not json" {
			t.Errorf("the file was touched: %q (%v)", data, err)
		}
	})

	t.Run("the implicit profile writes into ~/.claude", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("CLAUDE_CONFIG_DIR", "")
		initProfiles()
		defer initProfiles()

		// An empty config_dir means "claude's own default dir", so the file must
		// land in ~/.claude and nowhere else.
		if err := acceptBypassDisclaimer(Profile{Name: "implicit"}); err != nil {
			t.Fatalf("acceptBypassDisclaimer: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
		if err != nil {
			t.Fatalf("read ~/.claude/settings.json: %v", err)
		}
		if !strings.Contains(string(data), bypassSettingKey) {
			t.Errorf("~/.claude/settings.json does not record the acceptance: %s", data)
		}
	})
}

func TestGrokAndAgyLoginHealth(t *testing.T) {
	dir := t.TempDir()
	grok := Profile{Name: "work", Engine: engineGrok, ConfigDir: filepath.Join(dir, "grok")}
	if in, _, err := grokLoggedIn(grok); in || err != nil {
		t.Fatalf("missing auth.json should be logged out: in=%v err=%v", in, err)
	}
	if err := os.MkdirAll(engineHome(grok), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(grokAuthJSON(grok), []byte(`{"email":"me@x.ai"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	in, acct, err := grokLoggedIn(grok)
	if err != nil || !in || acct != "me@x.ai" {
		t.Fatalf("grokLoggedIn = %v %q %v", in, acct, err)
	}

	agy := Profile{Name: "lab", Engine: engineAntigravity, ConfigDir: filepath.Join(dir, "agy")}
	if in, _, err := agyLoggedIn(agy); in || err != nil {
		t.Fatalf("missing token should be logged out: in=%v err=%v", in, err)
	}
	token := agyOAuthToken(agy)
	if err := os.MkdirAll(filepath.Dir(token), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(token, []byte(`{"email":"lab@google.com"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	in, acct, err = agyLoggedIn(agy)
	if err != nil || !in || acct != "lab@google.com" {
		t.Fatalf("agyLoggedIn = %v %q %v", in, acct, err)
	}

	mixed := &Config{Profiles: map[string]*Profile{
		"you@example.com": {Engine: engineClaude, ConfigDir: filepath.Join(dir, "c")},
		"work":            {Engine: engineGrok, ConfigDir: grok.ConfigDir},
		"lab":             {Engine: engineAntigravity, ConfigDir: agy.ConfigDir},
	}}
	if got := listProfilesForEngine(mixed, engineGrok); len(got) != 1 || got[0].Name != "work" {
		t.Fatalf("listProfilesForEngine(grok) = %+v", got)
	}
	if got := configuredEngines(mixed); len(got) != 3 {
		t.Fatalf("configuredEngines = %v", got)
	}
}

func TestListProfilesAndDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Run("no profiles yields the implicit one", func(t *testing.T) {
		all := listProfiles(&Config{})
		if len(all) != 1 || all[0].Name != defaultProfileName || !all[0].Implicit {
			t.Fatalf("got %+v, want a single implicit profile", all)
		}
	})
	t.Run("nil config yields the implicit one", func(t *testing.T) {
		if all := listProfiles(nil); len(all) != 1 || all[0].Name != defaultProfileName {
			t.Fatalf("got %+v, want the implicit profile", all)
		}
	})
	t.Run("configured profiles are sorted", func(t *testing.T) {
		cfg := &Config{Profiles: map[string]*Profile{
			"work2": {ConfigDir: "/tmp/b"},
			"work":  {ConfigDir: "/tmp/a"},
		}}
		all := listProfiles(cfg)
		if len(all) != 2 || all[0].Name != "work" || all[1].Name != "work2" {
			t.Fatalf("got %+v, want [work work2]", all)
		}
	})
	t.Run("default_profile selects, else first by name", func(t *testing.T) {
		cfg := &Config{Profiles: map[string]*Profile{
			"work":  {ConfigDir: "/tmp/a"},
			"work2": {ConfigDir: "/tmp/b"},
		}}
		if got := defaultProfile(cfg).Name; got != "work" {
			t.Errorf("default without default_profile = %q, want work", got)
		}
		cfg.DefaultProfile = "work2"
		if got := defaultProfile(cfg).Name; got != "work2" {
			t.Errorf("default_profile = %q, want work2", got)
		}
		// A dangling default_profile must not break dispatch.
		cfg.DefaultProfile = "gone"
		if got := defaultProfile(cfg).Name; got != "work" {
			t.Errorf("dangling default_profile = %q, want fallback work", got)
		}
	})
}

func TestAccountKeysAndSharedEmailLookup(t *testing.T) {
	if got := normalizeAccountKey("Jairo@X.com", engineClaude); got != "jairo@x.com" {
		t.Errorf("claude key = %q", got)
	}
	if got := normalizeAccountKey("Jairo@X.com", engineCodex); got != "jairo@x.com/codex" {
		t.Errorf("codex key = %q", got)
	}
	if got := normalizeAccountKey("work", engineGrok); got != "work/grok" {
		t.Errorf("grok key = %q", got)
	}

	cfg := &Config{Profiles: map[string]*Profile{
		"jairo@x.com":       {Engine: engineClaude, Label: "jairo@x.com", ConfigDir: "/tmp/c"},
		"jairo@x.com/codex": {Engine: engineCodex, Label: "jairo@x.com", ConfigDir: "/tmp/x"},
		"work/grok":         {Engine: engineGrok, Label: "work", ConfigDir: "/tmp/g"},
	}}
	if _, ok := profileByName(cfg, "jairo@x.com"); ok {
		t.Fatal("bare email must not silently pick one of several engines")
	}
	if p, ok := profileByName(cfg, "jairo@x.com/codex"); !ok || profileEngine(p) != engineCodex {
		t.Fatalf("email/codex = %+v ok=%v", p, ok)
	}
	if p, ok := profileByName(cfg, "jairo@x.com codex"); !ok || profileEngine(p) != engineCodex {
		t.Fatalf("email codex = %+v ok=%v", p, ok)
	}
	if p, ok := profileByName(cfg, "jairo@x.com claude"); !ok || profileEngine(p) != engineClaude {
		t.Fatalf("email claude = %+v ok=%v", p, ok)
	}
	if p, ok := profileByKey(cfg, "jairo@x.com"); !ok || profileEngine(p) != engineClaude {
		t.Fatalf("exact Claude key = %+v ok=%v", p, ok)
	}
	if p, ok := profileByName(cfg, "work"); !ok || profileEngine(p) != engineGrok {
		t.Fatalf("unique short name = %+v ok=%v", p, ok)
	}
	if p, exists := profileByIdentityEngine(cfg, "jairo@x.com", engineCodex); !exists || p.Name != "jairo@x.com/codex" {
		t.Fatalf("identity+codex = %+v exists=%v", p, exists)
	}
	if _, exists := profileByIdentityEngine(cfg, "jairo@x.com", engineGrok); exists {
		t.Fatal("email is not a grok account")
	}

	legacy := &Config{Profiles: map[string]*Profile{
		"jairo@x.com": {Engine: engineCodex, Label: "jairo@x.com", ConfigDir: "/tmp/legacy"},
	}}
	if got := accountMapKey(legacy, "jairo@x.com", engineClaude); got != "jairo@x.com/claude" {
		t.Errorf("claude key when email is taken = %q", got)
	}
	if isAccountEmail("jairo@x.com/codex") {
		t.Error("a composite key must not parse as an email")
	}
}

func TestClaudeEnvScrubsInheritedState(t *testing.T) {
	// The leak this guards against: ccc started from inside a Claude Code
	// session inherits these, and a child `claude` then authenticates and
	// behaves as that parent session instead of as the profile.
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("ANTHROPIC_BASE_URL", "https://leaked.example")
	t.Setenv("ANTHROPIC_API_KEY", "sk-should-not-leak")
	t.Setenv("CLAUDE_CODE_OAUTH_SCOPES", "leaked")
	t.Setenv("CLAUDE_CODE_MESSAGING_URL", "leaked")
	t.Setenv("CLAUDE_CODE_SDK_VERSION", "leaked")
	t.Setenv("SOME_RANDOM_VAR", "leaked")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOME", "/home/test")
	t.Setenv("LC_ALL", "en_US.UTF-8")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/501")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.sock")

	env := claudeEnv(Profile{Name: "work2", ConfigDir: "/tmp/claude-b"})
	got := map[string]string{}
	for _, kv := range env {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				got[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	for _, banned := range []string{
		"CLAUDECODE", "ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY",
		"CLAUDE_CODE_OAUTH_SCOPES", "CLAUDE_CODE_MESSAGING_URL",
		"CLAUDE_CODE_SDK_VERSION", "SOME_RANDOM_VAR",
	} {
		if v, ok := got[banned]; ok {
			t.Errorf("%s leaked into the child env as %q", banned, v)
		}
	}
	for k, want := range map[string]string{
		"PATH": "/usr/bin:/bin", "HOME": "/home/test",
		"LC_ALL": "en_US.UTF-8", "XDG_RUNTIME_DIR": "/run/user/501",
		"SSH_AUTH_SOCK": "/tmp/agent.sock",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	if got["CLAUDE_CONFIG_DIR"] != "/tmp/claude-b" {
		t.Errorf("CLAUDE_CONFIG_DIR = %q, want /tmp/claude-b", got["CLAUDE_CONFIG_DIR"])
	}
}

func TestClaudeEnvOmitsConfigDirForImplicitProfile(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("HOME", "/home/test")
	initProfiles()
	p := implicitProfile()
	if !p.Implicit {
		t.Fatalf("expected an implicit profile, got %+v", p)
	}
	for _, kv := range claudeEnv(p) {
		if len(kv) >= 18 && kv[:18] == "CLAUDE_CONFIG_DIR=" {
			t.Errorf("implicit profile must not pin CLAUDE_CONFIG_DIR, got %q", kv)
		}
	}

	// When ccc itself was started with a dir, children must inherit it.
	t.Setenv("CLAUDE_CONFIG_DIR", "/custom/dir")
	initProfiles()
	p = implicitProfile()
	if p.Implicit || p.ConfigDir != "/custom/dir" {
		t.Fatalf("got %+v, want an explicit /custom/dir profile", p)
	}
	found := false
	for _, kv := range claudeEnv(p) {
		if kv == "CLAUDE_CONFIG_DIR=/custom/dir" {
			found = true
		}
	}
	if !found {
		t.Error("inherited CLAUDE_CONFIG_DIR was not passed through")
	}
	// Leave the package-level capture as the test process found it.
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	initProfiles()
}

func TestProfilePathsAreScopedToTheConfigDir(t *testing.T) {
	p := Profile{Name: "work2", ConfigDir: "/tmp/claude-b"}
	cases := map[string]string{
		profileProjectsDir(p): "/tmp/claude-b/projects",
		profileClaudeJSON(p):  "/tmp/claude-b/.claude.json",
		profileSettings(p):    "/tmp/claude-b/settings.json",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

// TestProfileClaudeJSONForImplicitProfile pins the one path that is NOT inside
// the config dir: with CLAUDE_CONFIG_DIR unset, .claude.json sits beside
// ~/.claude rather than in it.
func TestProfileClaudeJSONForImplicitProfile(t *testing.T) {
	t.Setenv("HOME", "/home/test")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	initProfiles()
	defer initProfiles()

	imp := implicitProfile()
	if got, want := profileClaudeJSON(imp), "/home/test/.claude.json"; got != want {
		t.Errorf("implicit .claude.json = %q, want %q", got, want)
	}
	if got, want := profileSettings(imp), "/home/test/.claude/settings.json"; got != want {
		t.Errorf("implicit settings.json = %q, want %q", got, want)
	}
	// An explicitly configured dir keeps its own copy, even when it is ~/.claude.
	explicit := Profile{Name: "same", ConfigDir: "/home/test/.claude"}
	if got, want := profileClaudeJSON(explicit), "/home/test/.claude/.claude.json"; got != want {
		t.Errorf("explicit .claude.json = %q, want %q", got, want)
	}
}

// TestEmptyConfigDirMeansClaudeDefault pins the distinction between an empty
// config_dir ("leave CLAUDE_CONFIG_DIR unset") and one spelled out as
// ~/.claude: setting the variable makes claude start a fresh <dir>/.claude.json
// instead of using ~/.claude.json, so `ccc profile add` must never pin it.
func TestEmptyConfigDirMeansClaudeDefault(t *testing.T) {
	t.Setenv("HOME", "/home/test")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	initProfiles()
	defer initProfiles()

	cfg := &Config{Profiles: map[string]*Profile{
		"default": {Label: "claude's default config dir"},
		"work2":   {ConfigDir: "/tmp/claude-b"},
	}}
	all := listProfiles(cfg)
	if len(all) != 2 {
		t.Fatalf("got %d profiles, want 2", len(all))
	}
	def, other := all[0], all[1]
	if def.Name != "default" || !def.Implicit {
		t.Fatalf("default = %+v, want an implicit profile", def)
	}
	if def.ConfigDir != "/home/test/.claude" {
		t.Errorf("default dir = %q, want /home/test/.claude", def.ConfigDir)
	}
	if profileClaudeJSON(def) != "/home/test/.claude.json" {
		t.Errorf("default .claude.json = %q, want the legacy location", profileClaudeJSON(def))
	}
	for _, kv := range claudeEnv(def) {
		if strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") {
			t.Errorf("empty config_dir must not pin CLAUDE_CONFIG_DIR, got %q", kv)
		}
	}
	if other.Implicit {
		t.Errorf("work2 = %+v, want an explicit profile", other)
	}
}

// TestIsStaleTokenError pins the mapping from Claude Code's org-flavoured
// wording to the action that actually fixes it (a fresh login).
func TestIsStaleTokenError(t *testing.T) {
	blocked := "org disabled OAuth — use API key or ask admin · Your organization has " +
		"disabled Claude subscription access for Claude Code · Use an Anthropic API key instead"
	if !isStaleTokenError(blocked) {
		t.Error("the observed blocked-job text should be recognised as a stale token")
	}
	if isStaleTokenError("waiting for your answer") {
		t.Error("an ordinary block must not be reported as a stale token")
	}
	if isStaleTokenError("") {
		t.Error("empty reason must not be reported as a stale token")
	}
}
