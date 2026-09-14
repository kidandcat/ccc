package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubPTY makes every PTY flow fail immediately, so a test can exercise
// /account add without spawning claude.
func stubPTY(t *testing.T, in *instance) {
	t.Helper()
	in.pty = func(string, []string, string, ...string) (*ptySession, error) {
		return nil, errors.New("no pty in tests")
	}
	t.Cleanup(func() { waitForLogin(t, in) })
}

// waitForLogin waits for the background login goroutine to release the slot, so
// it cannot outlive the test's database and fake API.
func waitForLogin(t *testing.T, in *instance) {
	t.Helper()
	for i := 0; i < 300; i++ {
		in.login.mu.Lock()
		pending := in.login.waiting
		in.login.mu.Unlock()
		if pending == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("a login flow is still pending after the test")
}

func TestRenderAccountsCard(t *testing.T) {
	cards := []accountCard{
		{
			// A profile that predates emails as keys is shown by the email its
			// label carries, never by the key or the directory behind it.
			Profile:    Profile{Name: "work", ConfigDir: "/data/profiles/work", Label: "jairo@example.com"},
			State:      accountOK,
			Account:    "jairo@example.com",
			Usage:      profileUsage{FiveHour: 12, SevenDay: 40, FiveHourKnown: true, SevenDayKnown: true},
			Bots:       []string{"deployer", "watcher"},
			IsDefault:  true,
			Disclaimer: true,
		},
		{
			Profile: Profile{Name: "personal", ConfigDir: "/data/profiles/personal"},
			State:   accountNeedsLogin,
			Usage:   profileUsage{FiveHour: unknownUtilization, SevenDay: unknownUtilization},
		},
		{
			Profile:    Profile{Name: "spare"},
			State:      accountLoggedOut,
			Usage:      profileUsage{FiveHour: 0, SevenDay: 0, FiveHourKnown: true, SevenDayKnown: true},
			Disclaimer: true,
		},
	}
	body, buttons := renderAccounts(cards)

	for _, want := range []string{
		"<b>jairo@example.com</b>", "⭐", "✅ logged in", "5h 12% · 7d 40%",
		"deployer, watcher", "personal", "⚠️ needs login", "5h ? · 7d ?",
		"spare", "❌ not logged in", "running: nothing", "bypass disclaimer not accepted",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the card is missing %q:\n%s", want, body)
		}
	}
	// The email replaces the key rather than being repeated under it, and the
	// config dir is never part of the owner's view.
	if strings.Count(body, "jairo@example.com") != 1 {
		t.Errorf("the account email is not shown exactly once:\n%s", body)
	}
	for _, leak := range []string{"/data/profiles/work", "<b>work</b>"} {
		if strings.Contains(body, leak) {
			t.Errorf("the card leaks %q:\n%s", leak, body)
		}
	}
	// The default account must not offer a "make default" button, and the
	// disclaimer warning belongs only to the profile that lacks it.
	if strings.Count(body, "bypass disclaimer not accepted") != 1 {
		t.Errorf("disclaimer warning shown for the wrong number of accounts:\n%s", body)
	}

	var relogin, makeDefault int
	for _, row := range buttons {
		for _, b := range row {
			switch {
			case strings.HasPrefix(b.CallbackData, "account:login:"):
				relogin++
			case strings.HasPrefix(b.CallbackData, "account:default:"):
				makeDefault++
			}
		}
	}
	if relogin != 3 {
		t.Errorf("%d relogin buttons, want one per account", relogin)
	}
	if makeDefault != 2 {
		t.Errorf("%d default buttons, want one per non-default account", makeDefault)
	}
}

func TestRenderAccountsWithNoProfiles(t *testing.T) {
	body, buttons := renderAccounts(nil)
	if !strings.Contains(body, "/account add &lt;identity&gt; &lt;engine&gt;") {
		t.Errorf("an empty account list should say how to add one: %q", body)
	}
	if len(buttons) != 0 {
		t.Errorf("no buttons expected with no accounts, got %d rows", len(buttons))
	}
}

// /account add takes the email of the Claude account and nothing else: the
// config dir is derived from it, so nothing the owner types becomes a path.
func TestAccountAddByEmail(t *testing.T) {
	in, _, api := testInstance(t)
	if err := saveConfig(in.config()); err != nil {
		t.Fatal(err)
	}
	stubPTY(t, in)

	in.handleAccountCommand(dmMessage(42, ""), "add  Jairo@Agentero.com ")

	p := in.config().Profiles["jairo@agentero.com"]
	if p == nil {
		t.Fatalf("the account was not created: %+v", in.config().Profiles)
	}
	if want := filepath.Join(in.dataDir, "profiles", "jairo_at_agentero.com"); p.ConfigDir != want {
		t.Errorf("config dir = %q, want %q", p.ConfigDir, want)
	}
	if p.Label != "jairo@agentero.com" {
		t.Errorf("label = %q, want the email", p.Label)
	}
	if profileEngine(*p) != engineClaude {
		t.Errorf("engine = %q, want claude (legacy add without engine)", p.Engine)
	}
	if in.config().DefaultProfile != "jairo@agentero.com" {
		t.Errorf("the first account did not become the default: %q", in.config().DefaultProfile)
	}
	joined := strings.Join(api.texts(""), "\n")
	if !strings.Contains(joined, "jairo@agentero.com") {
		t.Errorf("the reply does not name the account:\n%s", joined)
	}
	if strings.Contains(joined, "jairo_at_agentero.com") {
		t.Errorf("the reply leaks the directory behind the account:\n%s", joined)
	}
	// Adding it again is a no-op that points at the login instead.
	in.handleAccountCommand(dmMessage(42, ""), "add jairo@agentero.com")
	if len(in.config().Profiles) != 1 {
		t.Errorf("the same account was added twice: %+v", in.config().Profiles)
	}
}

// Anything that is not an email gets an explanation, never a bare Usage line:
// the owner who typed an address and was answered with "Usage: /account add
// <name>" is exactly the case this replaces.
func TestAccountAddRejectsWhatIsNotAnEmail(t *testing.T) {
	in, _, api := testInstance(t)
	for _, bad := range []string{"../escape", "with space", "work", strings.Repeat("x", 40)} {
		in.handleAccountCommand(dmMessage(42, ""), "add "+bad)
	}
	if cfg := in.config(); len(cfg.Profiles) != 0 {
		t.Errorf("a non-email created a profile: %+v", cfg.Profiles)
	}
	for _, got := range api.texts("") {
		// A second token that is not an engine is an engine error; a single
		// non-email is still the Claude-email explanation.
		if !strings.Contains(got, "@example.com") && !strings.Contains(got, "unknown engine") {
			t.Errorf("a non-email was not explained: %q", got)
		}
		if strings.Contains(got, "Usage:") {
			t.Errorf("an argument that was given back a bare Usage line: %q", got)
		}
	}
	// Only a genuinely empty argument is a usage error.
	in.handleAccountCommand(dmMessage(42, ""), "add")
	if last := api.texts("")[len(api.texts(""))-1]; !strings.Contains(last, "Usage: /account add") {
		t.Errorf("an empty argument should say how to use it: %q", last)
	}
}

// The account a login produces is the one `claude auth status` reports, not the
// one the owner typed — and learning it re-keys the profile.
func TestLoginStoresTheAccountClaudeReports(t *testing.T) {
	in, _, api := testInstance(t)
	in.setConfig(&Config{
		BotToken: "TESTTOKEN", ChatID: 42, GroupID: -100777, DataDir: in.dataDir,
		Profiles:       map[string]*Profile{"work": {ConfigDir: "/tmp/work"}},
		DefaultProfile: "work",
	})
	if err := saveConfig(in.config()); err != nil {
		t.Fatal(err)
	}

	if got := in.reconcileLoginEmail(42, 0, "work", "jairo@agentero.com"); got != "jairo@agentero.com" {
		t.Errorf("the profile key after the login = %q, want the email", got)
	}
	cfg := in.config()
	if cfg.Profiles["work"] != nil {
		t.Error("the legacy key survived the migration")
	}
	p := cfg.Profiles["jairo@agentero.com"]
	if p == nil || p.ConfigDir != "/tmp/work" {
		t.Fatalf("the profile was not re-keyed onto its email: %+v", cfg.Profiles)
	}
	if cfg.DefaultProfile != "jairo@agentero.com" {
		t.Errorf("the default still points at the legacy key: %q", cfg.DefaultProfile)
	}

	// A mismatch between what was typed and what was logged in is reported.
	in.reconcileLoginEmail(42, 0, "jairo@agentero.com", "other@agentero.com")
	joined := strings.Join(api.texts(""), "\n")
	if !strings.Contains(joined, "You typed") || !strings.Contains(joined, "other@agentero.com") {
		t.Errorf("the mismatch was not reported:\n%s", joined)
	}
	if in.config().Profiles["other@agentero.com"] == nil {
		t.Errorf("the account was not stored under the address it logged in as: %+v", in.config().Profiles)
	}
}

// The machine's pre-existing ~/.claude account has no config entry at all; it
// gets one as soon as its email is known, so it stops showing as "default".
func TestImplicitProfileIsNamedByItsEmail(t *testing.T) {
	in, _, _ := testInstance(t)
	if err := saveConfig(in.config()); err != nil {
		t.Fatal(err)
	}
	if got := in.rememberProfileEmail(defaultProfileName, "jairo.caroaccino@agentero.com"); got != "jairo.caroaccino@agentero.com" {
		t.Fatalf("the implicit profile was not named by its email: %q", got)
	}
	p := in.config().Profiles["jairo.caroaccino@agentero.com"]
	if p == nil {
		t.Fatalf("no entry was registered: %+v", in.config().Profiles)
	}
	if p.ConfigDir != "" {
		t.Errorf("config dir = %q, want empty so claude keeps its own layout", p.ConfigDir)
	}
	// It is addressable by its email and by the name it used to answer to.
	for _, name := range []string{"jairo.caroaccino@agentero.com", "JAIRO.CAROACCINO@agentero.com"} {
		if _, ok := profileByName(in.config(), name); !ok {
			t.Errorf("the account is not addressable as %q", name)
		}
	}
	if got := accountDisplay(listProfiles(in.config())[0]); got != "jairo.caroaccino@agentero.com" {
		t.Errorf("display name = %q, want the email", got)
	}
}

func TestProfileDirNameIsFilesystemSafe(t *testing.T) {
	for in, want := range map[string]string{
		"jairo@agentero.com": "jairo_at_agentero.com",
		"a.b+c@x.co.uk":      "a.b_c_at_x.co.uk",
		"../escape@x.com":    "escape_at_x.com",
		"":                   "account",
	} {
		if got := profileDirName(in); got != want {
			t.Errorf("profileDirName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAccountDefaultAndUnknownName(t *testing.T) {
	in, _, api := testInstance(t)
	in.setConfig(&Config{
		BotToken: "TESTTOKEN", ChatID: 42, GroupID: -100777, DataDir: in.dataDir,
		Profiles: map[string]*Profile{"a": {ConfigDir: "/tmp/a"}, "b": {ConfigDir: "/tmp/b"}},
	})
	if err := saveConfig(in.config()); err != nil {
		t.Fatal(err)
	}

	in.accountSetDefault(42, 0, "nope")
	if strings.Contains(strings.Join(api.texts(""), "\n"), "⭐ Default account") {
		t.Error("an unknown account was made default")
	}

	in.accountSetDefault(42, 0, "b")
	if got := in.config().DefaultProfile; got != "b" {
		t.Errorf("default profile = %q, want b", got)
	}
}

// An account carrying a running turn cannot be pulled out from under it.
func TestAccountRemoveRefusesWhileATurnIsRunning(t *testing.T) {
	in, _, _ := testInstance(t)
	in.setConfig(&Config{
		BotToken: "TESTTOKEN", ChatID: 42, GroupID: -100777, DataDir: in.dataDir,
		Profiles: map[string]*Profile{"a": {ConfigDir: "/tmp/a"}},
	})
	if err := saveConfig(in.config()); err != nil {
		t.Fatal(err)
	}
	b, err := in.createBot("busy", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := in.db.Create(&Turn{BotID: b.ID, Profile: "a", Source: sourceUser, Status: turnRunning}).Error; err != nil {
		t.Fatal(err)
	}

	if got := in.accountRemove("a"); !strings.Contains(got, "busy") {
		t.Errorf("removal was not refused: %q", got)
	}
	if _, still := in.config().Profiles["a"]; !still {
		t.Error("the profile was removed even though a turn was running on it")
	}

	// Once the turn ends, it can go.
	in.db.Model(&Turn{}).Where("bot_id = ?", b.ID).Update("status", turnDone)
	if got := in.accountRemove("a"); !strings.Contains(got, "Removed") {
		t.Errorf("removal failed after the turn finished: %q", got)
	}
	if _, still := in.config().Profiles["a"]; still {
		t.Error("the profile survived its removal")
	}
}

func TestBusyBotsByProfile(t *testing.T) {
	in, _, _ := testInstance(t)
	one, err := in.createBot("one", "")
	if err != nil {
		t.Fatal(err)
	}
	two, err := in.createBot("two", "")
	if err != nil {
		t.Fatal(err)
	}
	in.db.Create(&Turn{BotID: one.ID, Profile: "work", Source: sourceUser, Status: turnRunning})
	in.db.Create(&Turn{BotID: two.ID, Profile: "work", Source: sourceUser, Status: turnRunning})
	in.db.Create(&Turn{BotID: two.ID, Profile: "personal", Source: sourceUser, Status: turnDone})

	busy := in.busyBotsByProfile()
	if len(busy["work"]) != 2 {
		t.Errorf("work = %v, want both bots", busy["work"])
	}
	if len(busy["personal"]) != 0 {
		t.Errorf("a finished turn still counts as busy: %v", busy["personal"])
	}

	// The same source feeds profile selection's tie-break.
	if got := runningTurnsByProfileDB(in.db)["work"]; got != 2 {
		t.Errorf("runningTurnsByProfile[work] = %d, want 2", got)
	}
}

// /model and /setgroup are the two owner commands that rewrite the bootstrap
// configuration from Telegram, which is what makes a headless box possible.
func TestModelCommand(t *testing.T) {
	in, _, api := testInstance(t)
	if err := saveConfig(in.config()); err != nil {
		t.Fatal(err)
	}

	in.handleMessage(ownerMessage(0, "/model"))
	if !strings.Contains(strings.Join(api.texts(""), "\n"), "claude default") {
		t.Error("/model with no argument should show the current model")
	}

	in.handleMessage(ownerMessage(0, "/model sonnet"))
	if got := in.config().Model; got != "sonnet" {
		t.Errorf("model = %q, want sonnet", got)
	}
	reloaded, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Model != "sonnet" {
		t.Errorf("the model was not persisted: %q", reloaded.Model)
	}

	in.handleMessage(ownerMessage(0, "/model default"))
	if got := in.config().Model; got != "" {
		t.Errorf("/model default should clear it, got %q", got)
	}
}

func TestSetGroupCommand(t *testing.T) {
	in, _, api := testInstance(t)
	in.setConfig(&Config{BotToken: "TESTTOKEN", ChatID: 42, DataDir: in.dataDir})
	if err := saveConfig(in.config()); err != nil {
		t.Fatal(err)
	}

	// In a DM it is refused: there is nothing to bind to.
	in.handleMessage(dmMessage(42, "/setgroup"))
	if in.config().GroupID != 0 {
		t.Error("/setgroup in a DM must not set a group")
	}

	msg := ownerMessage(0, "/setgroup")
	msg.Chat.ID = -100999
	in.handleMessage(msg)

	if got := in.config().GroupID; got != -100999 {
		t.Errorf("group = %d, want -100999", got)
	}
	reloaded, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.GroupID != -100999 {
		t.Errorf("the group was not persisted: %d", reloaded.GroupID)
	}
	if !strings.Contains(strings.Join(api.texts(""), "\n"), "-100999") {
		t.Error("/setgroup did not confirm the new group")
	}
}

func TestParseAccountAdd(t *testing.T) {
	cases := []struct {
		in, identity, engine string
		ok                   bool
	}{
		{"you@example.com", "you@example.com", engineClaude, true},
		{"you@example.com claude", "you@example.com", engineClaude, true},
		{"claude you@example.com", "you@example.com", engineClaude, true},
		{"work grok", "work", engineGrok, true},
		{"grok work", "work", engineGrok, true},
		{"work grok-build", "work", engineGrok, true},
		{"lab agy", "lab", engineAntigravity, true},
		{"antigravity lab", "lab", engineAntigravity, true},
		{"lab antigravity", "lab", engineAntigravity, true},
		{"", "", "", false},
		{"grok", "", "", false},
		{"work not-a-cli", "", "", false},
		{"a b c", "", "", false},
	}
	for _, c := range cases {
		id, eng, err := parseAccountAdd(c.in)
		if c.ok {
			if err != nil || id != c.identity || eng != c.engine {
				t.Errorf("parseAccountAdd(%q) = (%q,%q,%v), want (%q,%q,nil)", c.in, id, eng, err, c.identity, c.engine)
			}
			continue
		}
		if err == nil {
			t.Errorf("parseAccountAdd(%q) = (%q,%q), want error", c.in, id, eng)
		}
	}
}

func TestAccountAddTakesEngine(t *testing.T) {
	in, _, api := testInstance(t)
	if err := saveConfig(in.config()); err != nil {
		t.Fatal(err)
	}
	stubPTY(t, in)

	in.handleAccountCommand(dmMessage(42, ""), "add work grok")
	p := in.config().Profiles["work"]
	if p == nil {
		t.Fatalf("grok account was not created: %+v", in.config().Profiles)
	}
	if profileEngine(*p) != engineGrok {
		t.Errorf("engine = %q, want grok", p.Engine)
	}
	if want := filepath.Join(in.dataDir, "accounts", engineGrok, "work"); p.ConfigDir != want {
		t.Errorf("grok home = %q, want %q", p.ConfigDir, want)
	}

	in.handleAccountCommand(dmMessage(42, ""), "add lab agy")
	agy := in.config().Profiles["lab"]
	if agy == nil || profileEngine(*agy) != engineAntigravity {
		t.Fatalf("agy account = %+v", in.config().Profiles)
	}
	if want := filepath.Join(in.dataDir, "accounts", engineAntigravity, "lab"); agy.ConfigDir != want {
		t.Errorf("agy home = %q, want %q", agy.ConfigDir, want)
	}

	in.handleAccountCommand(dmMessage(42, ""), "add you@example.com claude")
	cl := in.config().Profiles["you@example.com"]
	if cl == nil || profileEngine(*cl) != engineClaude {
		t.Fatalf("claude account = %+v", in.config().Profiles)
	}

	if len(in.config().Profiles) != 3 {
		t.Errorf("mixed pool size = %d, want 3: %+v", len(in.config().Profiles), in.config().Profiles)
	}
	joined := strings.Join(api.texts(""), "\n")
	if !strings.Contains(joined, "Grok Build") || !strings.Contains(joined, "Antigravity") {
		t.Errorf("add replies should name the engine:\n%s", joined)
	}
}

func TestRenderAccountsShowsEngineAndMixedHealth(t *testing.T) {
	cards := []accountCard{
		{
			Profile:    Profile{Name: "you@example.com", Engine: engineClaude, Label: "you@example.com"},
			State:      accountOK,
			Usage:      profileUsage{FiveHour: 10, SevenDay: 20, FiveHourKnown: true, SevenDayKnown: true},
			IsDefault:  true,
			Disclaimer: true,
		},
		{
			Profile: Profile{Name: "work", Engine: engineGrok, Label: "work"},
			State:   accountOK,
		},
		{
			Profile: Profile{Name: "lab", Engine: engineAntigravity, Label: "lab"},
			State:   accountLoggedOut,
		},
	}
	body, _ := renderAccounts(cards)
	for _, want := range []string{
		"<b>Accounts</b>", "Claude Code", "Grok Build", "Antigravity",
		"you@example.com", "work", "lab", "✅ logged in", "❌ not logged in",
		"5h 10%",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("mixed card missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "bypass disclaimer") {
		t.Errorf("grok/agy cards must not show the Claude disclaimer:\n%s", body)
	}
	if strings.Count(body, "usage:") != 1 {
		t.Errorf("usage should be Claude-only:\n%s", body)
	}
}
