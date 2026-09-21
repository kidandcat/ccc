package main

import (
	"context"
	"encoding/json"
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

	for _, want := range []string{"1. ", "2. ", "3. "} {
		if !strings.Contains(body, want) {
			t.Errorf("the card is missing numbered row %q:\n%s", want, body)
		}
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
	// Labels are the list numbers, not emails — Telegram truncates long
	// addresses and the two Default buttons would otherwise look identical.
	wantButtons := [][]string{
		{"1 Relogin"},
		{"2 Relogin", "⭐ 2"},
		{"3 Relogin", "⭐ 3"},
		{"🔄 Refresh"},
	}
	if len(buttons) != len(wantButtons) {
		t.Fatalf("button rows = %d, want %d: %+v", len(buttons), len(wantButtons), buttons)
	}
	for i, wantRow := range wantButtons {
		if len(buttons[i]) != len(wantRow) {
			t.Fatalf("row %d has %d buttons, want %d: %+v", i, len(buttons[i]), len(wantRow), buttons[i])
		}
		for j, want := range wantRow {
			if buttons[i][j].Text != want {
				t.Errorf("button [%d][%d] = %q, want %q", i, j, buttons[i][j].Text, want)
			}
			if strings.Contains(buttons[i][j].Text, "@") {
				t.Errorf("button [%d][%d] still carries an email: %q", i, j, buttons[i][j].Text)
			}
		}
	}
	if got, want := buttons[0][0].CallbackData, "account:login:"+accountButtonTarget(cards[0].Profile); got != want {
		t.Errorf("account 1 relogin callback = %q, want %q", got, want)
	}
	if got, want := buttons[1][0].CallbackData, "account:login:"+accountButtonTarget(cards[1].Profile); got != want {
		t.Errorf("account 2 relogin callback = %q, want %q", got, want)
	}
	if got, want := buttons[1][1].CallbackData, "account:default:"+accountButtonTarget(cards[1].Profile); got != want {
		t.Errorf("account 2 default callback = %q, want %q", got, want)
	}
	if got, want := buttons[2][0].CallbackData, "account:login:"+accountButtonTarget(cards[2].Profile); got != want {
		t.Errorf("account 3 relogin callback = %q, want %q", got, want)
	}
}

func TestRenderAccountsNumbersSameEmailDifferentEngines(t *testing.T) {
	cards := []accountCard{
		{
			Profile:    Profile{Name: "jairo.caroaccino@agentero.com", Engine: engineClaude, Label: "jairo.caroaccino@agentero.com"},
			State:      accountLoggedOut,
			IsDefault:  true,
			Disclaimer: true,
		},
		{
			Profile: Profile{Name: "jairo.caroaccino@agentero.com/codex", Engine: engineCodex, Label: "jairo.caroaccino@agentero.com"},
			State:   accountOK,
		},
		{
			Profile:    Profile{Name: "jairo@agentero.com", Engine: engineClaude, Label: "jairo@agentero.com"},
			State:      accountOK,
			Disclaimer: true,
		},
	}
	body, buttons := renderAccounts(cards)
	if !strings.Contains(body, "1. <b>jairo.caroaccino@agentero.com</b> ⭐") {
		t.Errorf("row 1 should be numbered and starred:\n%s", body)
	}
	if !strings.Contains(body, "2. <b>jairo.caroaccino@agentero.com</b> —") {
		t.Errorf("row 2 should be numbered without stealing the default star:\n%s", body)
	}
	if !strings.Contains(body, "3. <b>jairo@agentero.com</b>") {
		t.Errorf("row 3 should be numbered:\n%s", body)
	}
	if len(buttons) != 4 {
		t.Fatalf("rows = %d, want 3 accounts + refresh: %+v", len(buttons), buttons)
	}
	if buttons[0][0].Text != "1 Relogin" || buttons[1][0].Text != "2 Relogin" || buttons[2][0].Text != "3 Relogin" {
		t.Errorf("relogin labels = %q %q %q", buttons[0][0].Text, buttons[1][0].Text, buttons[2][0].Text)
	}
	if buttons[1][1].Text != "⭐ 2" || buttons[2][1].Text != "⭐ 3" {
		t.Errorf("default labels = %q %q", buttons[1][1].Text, buttons[2][1].Text)
	}
	if got, want := buttons[0][0].CallbackData, "account:login:jairo.caroaccino@agentero.com/claude"; got != want {
		t.Errorf("1 relogin callback = %q, want %q", got, want)
	}
	if got, want := buttons[1][0].CallbackData, "account:login:jairo.caroaccino@agentero.com/codex"; got != want {
		t.Errorf("2 relogin callback = %q, want %q", got, want)
	}
	if got, want := buttons[1][1].CallbackData, "account:default:jairo.caroaccino@agentero.com/codex"; got != want {
		t.Errorf("2 default callback = %q, want %q", got, want)
	}
	if got, want := buttons[2][1].CallbackData, "account:default:jairo@agentero.com/claude"; got != want {
		t.Errorf("3 default callback = %q, want %q", got, want)
	}
	if got, want := buttons[2][0].CallbackData, "account:login:jairo@agentero.com/claude"; got != want {
		t.Errorf("3 relogin callback = %q, want %q", got, want)
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
		BotToken: "TESTTOKEN", ChatID: 42, DataDir: in.dataDir,
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
		BotToken: "TESTTOKEN", ChatID: 42, DataDir: in.dataDir,
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
		BotToken: "TESTTOKEN", ChatID: 42, DataDir: in.dataDir,
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

// /model is the owner command that rewrites the bootstrap configuration from
// Telegram, which is what makes a headless box possible.
func TestModelCommand(t *testing.T) {
	in, _, api := testInstance(t)
	if err := saveConfig(in.config()); err != nil {
		t.Fatal(err)
	}

	in.handleMessage(ownerMessage("/model"))
	listed := strings.Join(api.texts(""), "\n")
	if !strings.Contains(listed, "claude · <code>default</code>") {
		t.Errorf("/model with no argument should list each account's model, got %q", listed)
	}
	if strings.Contains(listed, "claude=default") {
		t.Errorf("/model must not hide accounts behind a global engine=slug list: %q", listed)
	}

	in.handleMessage(ownerMessage("/model sonnet"))
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

	in.handleMessage(ownerMessage("/model default"))
	if got := in.config().Model; got != "" {
		t.Errorf("/model default should clear it, got %q", got)
	}

	in.handleMessage(ownerMessage("/model grok grok-4"))
	if got := in.config().Models[engineGrok]; got != "grok-4" {
		t.Errorf("grok model = %q", got)
	}
	if got := in.config().Model; got != "" {
		t.Errorf("setting grok must not rewrite the Claude model, got %q", got)
	}

	// /model in the DM is instance-level; it does not override a worker.
	b, err := in.createBot("dev", "")
	if err != nil {
		t.Fatal(err)
	}
	in.handleMessage(ownerMessage("/model haiku"))
	var bot Bot
	in.db.First(&bot, b.ID)
	if bot.Model != "" {
		t.Errorf("worker model = %q, want empty (instance default)", bot.Model)
	}
	if got := in.config().Model; got != "haiku" {
		t.Errorf("instance model = %q, want haiku", got)
	}
}

func lastKeyboard(t *testing.T, api *fakeBotAPI) [][]InlineKeyboardButton {
	t.Helper()
	calls := api.since("sendMessage")
	if len(calls) == 0 {
		t.Fatal("no sendMessage")
	}
	raw := calls[len(calls)-1].Params.Get("reply_markup")
	if raw == "" {
		t.Fatal("last sendMessage has no reply_markup")
	}
	var markup struct {
		InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
	}
	if err := json.Unmarshal([]byte(raw), &markup); err != nil {
		t.Fatalf("reply_markup: %v (%s)", err, raw)
	}
	return markup.InlineKeyboard
}

func TestRenderModelStatusPerAccount(t *testing.T) {
	cfg := &Config{
		Profiles: map[string]*Profile{
			"you@example.com":          {Engine: engineClaude, Label: "you@example.com"},
			"kidandcat@gmail.com/grok": {Engine: engineGrok, Label: "kidandcat@gmail.com"},
			"openai/codex":             {Engine: engineCodex, Label: "openai"},
		},
		Models: map[string]string{
			engineClaude: "opus",
			engineGrok:   "grok-4.6",
			engineCodex:  "gpt-5.4",
		},
	}
	body, buttons := renderModelStatus(cfg, nil)
	for _, want := range []string{
		"grok · <code>grok-4.6</code>",
		"claude · <code>opus</code>",
		"codex · <code>gpt-5.4</code>",
		"kidandcat@gmail.com",
		"you@example.com",
		"openai",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("listing missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "claude=default") || strings.Contains(body, "instance:") {
		t.Errorf("listing still uses the collapsed engine map:\n%s", body)
	}
	if len(buttons) != 3 {
		t.Fatalf("%d account buttons, want 3", len(buttons))
	}
	var picks int
	for _, row := range buttons {
		for _, b := range row {
			if strings.HasPrefix(b.CallbackData, "model:pick:") {
				picks++
			}
		}
	}
	if picks != 3 {
		t.Errorf("%d pick buttons, want one per account", picks)
	}
}

func TestModelPickerSetsEngine(t *testing.T) {
	in, _, api := testInstance(t)
	in.setConfig(&Config{
		BotToken: "TESTTOKEN", ChatID: 42, DataDir: in.dataDir,
		Profiles: map[string]*Profile{
			"you@example.com":          {Engine: engineClaude, Label: "you@example.com"},
			"kidandcat@gmail.com/grok": {Engine: engineGrok, Label: "kidandcat@gmail.com", ConfigDir: t.TempDir()},
		},
		Models: map[string]string{engineGrok: "grok-4.6", engineClaude: "opus"},
	})
	if err := saveConfig(in.config()); err != nil {
		t.Fatal(err)
	}

	in.handleMessage(ownerMessage("/model"))
	listed := strings.Join(api.texts(""), "\n")
	if !strings.Contains(listed, "grok · <code>grok-4.6</code>") || !strings.Contains(listed, "claude · <code>opus</code>") {
		t.Errorf("expected one line per account, got %q", listed)
	}
	kb := lastKeyboard(t, api)
	var grokPick string
	for _, row := range kb {
		for _, b := range row {
			if strings.Contains(b.Text, "kidandcat@gmail.com") || strings.HasPrefix(b.CallbackData, "model:pick:kidandcat@gmail.com/grok") {
				grokPick = b.CallbackData
			}
		}
	}
	if grokPick == "" {
		t.Fatalf("no grok account button: %+v", kb)
	}

	cb := &CallbackQuery{ID: "m1", Data: grokPick}
	cb.From.ID = 42
	cb.Message = dmMessage(42, "")
	in.handleCallback(cb)

	picker := lastKeyboard(t, api)
	var setGrok, clearGrok bool
	for _, row := range picker {
		for _, b := range row {
			if b.CallbackData == "model:set:grok:grok-4.6" || strings.HasPrefix(b.CallbackData, "model:set:grok:") {
				setGrok = true
			}
			if b.CallbackData == "model:clear:grok" {
				clearGrok = true
			}
		}
	}
	if !setGrok {
		t.Errorf("picker has no grok slug buttons: %+v", picker)
	}
	if !clearGrok {
		t.Errorf("picker has no engine-default button: %+v", picker)
	}

	set := &CallbackQuery{ID: "m2", Data: "model:set:grok:grok-4.5"}
	set.From.ID = 42
	set.Message = dmMessage(42, "")
	in.handleCallback(set)
	if got := in.config().Models[engineGrok]; got != "grok-4.5" {
		t.Errorf("picker did not set grok, got %q", got)
	}
	if got := in.config().Models[engineClaude]; got != "opus" {
		t.Errorf("setting grok rewrote claude, got %q", got)
	}
}

func TestRenderModelPickerClaudeAliases(t *testing.T) {
	p := Profile{Name: "you@example.com", Engine: engineClaude, Label: "you@example.com"}
	cfg := &Config{Models: map[string]string{engineClaude: "opus"}}
	body, buttons := renderModelPicker(cfg, p)
	if !strings.Contains(body, "you@example.com") || !strings.Contains(body, "opus") {
		t.Errorf("picker body = %q", body)
	}
	found := map[string]bool{}
	for _, row := range buttons {
		for _, b := range row {
			found[b.CallbackData] = true
		}
	}
	for _, want := range []string{"model:set:claude:opus", "model:set:claude:sonnet", "model:set:claude:haiku", "model:clear:claude"} {
		if !found[want] {
			t.Errorf("missing %s in %+v", want, found)
		}
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
		{"openai codex", "openai", engineCodex, true},
		{"codex openai", "openai", engineCodex, true},
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
	p := in.config().Profiles["work/grok"]
	if p == nil {
		t.Fatalf("grok account was not created: %+v", in.config().Profiles)
	}
	if profileEngine(*p) != engineGrok {
		t.Errorf("engine = %q, want grok", p.Engine)
	}
	if p.Label != "work" {
		t.Errorf("label = %q, want the identity, not the composite key", p.Label)
	}
	if want := filepath.Join(in.dataDir, "accounts", engineGrok, "work"); p.ConfigDir != want {
		t.Errorf("grok home = %q, want %q", p.ConfigDir, want)
	}

	in.handleAccountCommand(dmMessage(42, ""), "add lab agy")
	agy := in.config().Profiles["lab/antigravity"]
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
			Usage: profileUsage{
				FiveHour: 3, FiveHourKnown: true,
				Windows: []usageWin{{Name: "week", Percent: 3}},
			},
		},
		{
			Profile: Profile{Name: "lab", Engine: engineAntigravity, Label: "lab"},
			State:   accountLoggedOut,
			Usage:   naProfileUsage("no public usage endpoint"),
		},
		{
			Profile: Profile{Name: "openai", Engine: engineCodex, Label: "openai"},
			State:   accountOK,
			Usage: profileUsage{
				FiveHour: 18, FiveHourKnown: true,
				SevenDay: 40, SevenDayKnown: true,
				Windows: []usageWin{
					{Name: "5h", Percent: 18},
					{Name: "7d", Percent: 40},
				},
			},
		},
	}
	body, _ := renderAccounts(cards)
	for _, want := range []string{
		"<b>Accounts</b>", "Claude Code", "Grok Build", "Antigravity", "Codex",
		"you@example.com", "work", "lab", "openai", "✅ logged in", "❌ not logged in",
		"5h 10%", "week 3%", "n/a (no public usage endpoint)", "5h 18% · 7d 40%",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("mixed card missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "bypass disclaimer") {
		t.Errorf("grok/agy/codex cards must not show the Claude disclaimer:\n%s", body)
	}
	if strings.Count(body, "usage:") != 4 {
		t.Errorf("every engine should show usage:\n%s", body)
	}
}

func TestRenderAccountsShowsResetCountdown(t *testing.T) {
	now := time.Now()
	cards := []accountCard{
		{
			Profile: Profile{Name: "you@example.com", Engine: engineClaude, Label: "you@example.com"},
			State:   accountOK,
			Usage: profileUsage{
				FiveHour: 62, FiveHourKnown: true, FiveHourResetAt: now.Add(80*time.Minute + 10*time.Second),
				SevenDay: 40, SevenDayKnown: true, SevenDayResetAt: now.Add(3*24*time.Hour + 2*time.Minute),
			},
			Disclaimer: true,
		},
		{
			Profile: Profile{Name: "work", Engine: engineGrok, Label: "work"},
			State:   accountOK,
			Usage: profileUsage{
				Windows: []usageWin{{Name: "week", Percent: 8, ResetAt: now.Add(5*24*time.Hour + time.Hour)}},
			},
		},
	}
	body, _ := renderAccounts(cards)
	for _, want := range []string{
		"usage: 5h 62% · reset 1h20m · 7d 40% · reset 3d",
		"usage: week 8% · reset 5d",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("card missing %q:\n%s", want, body)
		}
	}
}

// The same email on Claude and Codex is two accounts; the same email on the
// same engine is still rejected. Status cards show the email, not the
// composite key.
func TestAccountAddSameEmailDifferentEngine(t *testing.T) {
	in, _, api := testInstance(t)
	if err := saveConfig(in.config()); err != nil {
		t.Fatal(err)
	}
	stubPTY(t, in)

	in.handleAccountCommand(dmMessage(42, ""), "add jairo.caroaccino@agentero.com claude")
	in.handleAccountCommand(dmMessage(42, ""), "add jairo.caroaccino@agentero.com codex")

	cfg := in.config()
	claude := cfg.Profiles["jairo.caroaccino@agentero.com"]
	codex := cfg.Profiles["jairo.caroaccino@agentero.com/codex"]
	if claude == nil || profileEngine(*claude) != engineClaude {
		t.Fatalf("claude profile missing: %+v", cfg.Profiles)
	}
	if codex == nil || profileEngine(*codex) != engineCodex {
		t.Fatalf("codex profile missing: %+v", cfg.Profiles)
	}
	if codex.Label != "jairo.caroaccino@agentero.com" {
		t.Errorf("codex label = %q, want the email", codex.Label)
	}
	if want := filepath.Join(in.dataDir, "accounts", engineCodex, "jairo.caroaccino_at_agentero.com"); codex.ConfigDir != want {
		t.Errorf("codex home = %q, want %q", codex.ConfigDir, want)
	}
	if got := accountDisplay(Profile{Name: "jairo.caroaccino@agentero.com/codex", Engine: engineCodex, Label: "jairo.caroaccino@agentero.com"}); got != "jairo.caroaccino@agentero.com" {
		t.Errorf("display = %q, want the email", got)
	}

	// Duplicate engine is still a no-op.
	before := len(cfg.Profiles)
	in.handleAccountCommand(dmMessage(42, ""), "add jairo.caroaccino@agentero.com codex")
	if len(in.config().Profiles) != before {
		t.Errorf("same engine was added twice: %+v", in.config().Profiles)
	}
	joined := strings.Join(api.texts(""), "\n")
	if !strings.Contains(joined, "already exists") {
		t.Errorf("duplicate same-engine add was not rejected:\n%s", joined)
	}
	if !strings.Contains(joined, "jairo.caroaccino@agentero.com/codex") {
		t.Errorf("duplicate hint should name the composite login:\n%s", joined)
	}
}

func TestAccountLoginDisambiguation(t *testing.T) {
	in, _, api := testInstance(t)
	in.setConfig(&Config{
		BotToken: "TESTTOKEN", ChatID: 42, DataDir: in.dataDir,
		Profiles: map[string]*Profile{
			"jairo@example.com":       {Engine: engineClaude, Label: "jairo@example.com", ConfigDir: "/tmp/c"},
			"jairo@example.com/codex": {Engine: engineCodex, Label: "jairo@example.com", ConfigDir: "/tmp/x"},
		},
	})
	if err := saveConfig(in.config()); err != nil {
		t.Fatal(err)
	}
	stubPTY(t, in)

	in.handleAccountCommand(dmMessage(42, ""), "login jairo@example.com")
	bare := strings.Join(api.texts(""), "\n")
	if !strings.Contains(bare, "Several accounts") {
		t.Errorf("bare email should ask for the engine:\n%s", bare)
	}
	if !strings.Contains(bare, "jairo@example.com/codex") || !strings.Contains(bare, "jairo@example.com") {
		t.Errorf("disambiguation should list both addresses:\n%s", bare)
	}
	if strings.Contains(bare, "Starting") {
		t.Error("bare email must not start a login")
	}

	n := len(api.texts(""))
	in.handleAccountCommand(dmMessage(42, ""), "login jairo@example.com/codex")
	waitForLogin(t, in)
	slash := strings.Join(api.texts("")[n:], "\n")
	if !strings.Contains(slash, "Starting") || !strings.Contains(strings.ToLower(slash), "codex") {
		t.Errorf("email/codex should start the Codex login:\n%s", slash)
	}

	n = len(api.texts(""))
	in.handleAccountCommand(dmMessage(42, ""), "login jairo@example.com claude")
	waitForLogin(t, in)
	words := strings.Join(api.texts("")[n:], "\n")
	if !strings.Contains(words, "Starting") || !strings.Contains(strings.ToLower(words), "claude") {
		t.Errorf("email claude should start the Claude login:\n%s", words)
	}

	before := in.config().DefaultProfile
	in.accountSetDefault(42, 0, "jairo@example.com")
	if in.config().DefaultProfile != before {
		t.Error("default with a shared email must not pick an engine silently")
	}
}

func TestAccountLoginCallbackIncludesEngineWhenEmailIsShared(t *testing.T) {
	in, _, api := testInstance(t)
	in.setConfig(&Config{
		BotToken: "TESTTOKEN", ChatID: 42, DataDir: in.dataDir,
		DefaultProfile: "jairo.caroaccino@agentero.com",
		Profiles: map[string]*Profile{
			"jairo.caroaccino@agentero.com": {
				Engine: engineClaude, Label: "jairo.caroaccino@agentero.com", ConfigDir: t.TempDir(),
			},
			"jairo.caroaccino@agentero.com/codex": {
				Engine: engineCodex, Label: "jairo.caroaccino@agentero.com", ConfigDir: t.TempDir(),
			},
			"jairo@agentero.com": {
				Engine: engineClaude, Label: "jairo@agentero.com", ConfigDir: t.TempDir(),
			},
		},
	})
	if err := saveConfig(in.config()); err != nil {
		t.Fatal(err)
	}
	stubPTY(t, in)

	cards := in.collectAccountCards()
	_, buttons := renderAccounts(cards)
	if len(buttons) < 3 {
		t.Fatalf("buttons = %+v", buttons)
	}
	claudeLogin := buttons[0][0].CallbackData
	if claudeLogin != "account:login:jairo.caroaccino@agentero.com/claude" {
		t.Fatalf("1 Relogin callback = %q, want engine-qualified claude", claudeLogin)
	}

	cb := &CallbackQuery{ID: "login1", Data: claudeLogin}
	cb.From.ID = 42
	cb.Message = dmMessage(42, "")
	in.handleCallback(cb)
	waitForLogin(t, in)
	got := strings.Join(api.texts(""), "\n")
	if strings.Contains(got, "Several accounts") {
		t.Errorf("1 Relogin must not ask to specify the engine:\n%s", got)
	}
	if !strings.Contains(got, "Starting") || !strings.Contains(strings.ToLower(got), "claude") {
		t.Errorf("1 Relogin should start the Claude login:\n%s", got)
	}

	n := len(api.texts(""))
	codexDefault := buttons[1][1].CallbackData
	if !strings.HasPrefix(codexDefault, "account:default:jairo.caroaccino@agentero.com/codex") {
		t.Fatalf("2 Default callback = %q", codexDefault)
	}
	def := &CallbackQuery{ID: "def2", Data: codexDefault}
	def.From.ID = 42
	def.Message = dmMessage(42, "")
	in.handleCallback(def)
	if in.config().DefaultProfile != "jairo.caroaccino@agentero.com/codex" {
		t.Errorf("2 Default = %q, want the Codex key", in.config().DefaultProfile)
	}
	defMsg := strings.Join(api.texts("")[n:], "\n")
	if strings.Contains(defMsg, "Several accounts") {
		t.Errorf("2 Default must not ask to specify the engine:\n%s", defMsg)
	}

	// A stale button that still carried the bare Claude key must keep working.
	n = len(api.texts(""))
	stale := &CallbackQuery{ID: "stale", Data: "account:login:jairo.caroaccino@agentero.com"}
	stale.From.ID = 42
	stale.Message = dmMessage(42, "")
	in.handleCallback(stale)
	waitForLogin(t, in)
	staleGot := strings.Join(api.texts("")[n:], "\n")
	if strings.Contains(staleGot, "Several accounts") {
		t.Errorf("legacy bare-email Relogin must still pick Claude, not ask:\n%s", staleGot)
	}
}

func TestCancelLoginStripsBotMention(t *testing.T) {
	in, _, api := testInstance(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	waiter := &loginWaiter{
		chatID: -100777, topicID: 7, profile: "you@example.com",
		codes: make(chan string, 1), cancel: cancel,
	}
	in.login.waiting = waiter

	if !in.takeLoginCode(-100777, 7, "/cancel@jairo_vps_bot") {
		t.Fatal("/cancel@bot must be consumed as a cancel")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("the login context was not cancelled")
	}
	if !strings.Contains(strings.Join(api.texts(""), "\n"), "Login cancelled.") {
		t.Errorf("missing cancel confirmation: %v", api.texts(""))
	}
}

func TestCancelLoginFromAnotherTopic(t *testing.T) {
	in, _, _ := testInstance(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	in.login.waiting = &loginWaiter{
		chatID: -100777, topicID: 1, profile: "you@example.com",
		codes: make(chan string, 1), cancel: cancel,
	}
	if !in.takeLoginCode(-100777, 99, "/cancel") {
		t.Fatal("/cancel from another topic must still abort the login")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("the login context was not cancelled")
	}
}

func TestCancelWithNoLogin(t *testing.T) {
	in, _, api := testInstance(t)
	in.handleMessage(ownerMessage("/cancel"))
	if !strings.Contains(strings.Join(api.texts(""), "\n"), "Nothing to cancel.") {
		t.Errorf("got %v", api.texts(""))
	}
	in.handleMessage(ownerMessage("/cancel@jairo_vps_bot"))
	if strings.Count(strings.Join(api.texts(""), "\n"), "Nothing to cancel.") != 2 {
		t.Errorf("/cancel@bot with no login: %v", api.texts(""))
	}
}
