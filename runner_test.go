package main

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestClaudeTurnArgsIsolatesTheBot(t *testing.T) {
	args := claudeTurnArgs("sonnet", "SYS", `{"mcpServers":{}}`, "11111111-2222-4333-8444-555555555555", false)
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"-p", "--output-format stream-json", "--verbose",
		"--permission-mode bypassPermissions", "--disable-slash-commands",
		"--strict-mcp-config", "--system-prompt SYS", "--model sonnet",
		"--session-id 11111111-2222-4333-8444-555555555555",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in: %s", want, joined)
		}
	}
	// --setting-sources must be present with an EMPTY value: that is the only
	// combination verified to load no CLAUDE.md at all.
	found := false
	for i, a := range args {
		if a == "--setting-sources" {
			found = true
			if i+1 >= len(args) || args[i+1] != "" {
				t.Errorf("--setting-sources must be followed by an empty value, got %q", args[i+1:])
			}
		}
	}
	if !found {
		t.Error("--setting-sources is missing; the bot would load the owner's CLAUDE.md")
	}
	for _, forbidden := range []string{"--bare", "--safe-mode", "--append-system-prompt"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("%s must never be used", forbidden)
		}
	}
}

func TestClaudeTurnArgsResumeAndDefaultModel(t *testing.T) {
	args := claudeTurnArgs("", "SYS", "{}", "sess-1", true)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--resume sess-1") {
		t.Errorf("resume flag missing: %s", joined)
	}
	if strings.Contains(joined, "--session-id") {
		t.Error("a resume must not also pass --session-id")
	}
	if strings.Contains(joined, "--model") {
		t.Error("no model configured should mean no --model flag")
	}
}

func TestNewUUIDIsAValidSessionID(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		u := newUUID()
		if !re.MatchString(u) {
			t.Fatalf("newUUID produced %q, which claude --session-id would reject", u)
		}
		if seen[u] {
			t.Fatalf("newUUID repeated %q", u)
		}
		seen[u] = true
	}
}

func TestClassifyFailure(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{staleTokenMsg, errAuthStale},
		{"Error: Not logged in. Please run claude login", errAuthStale},
		{"Claude AI usage limit reached|1788000000", errRateLimited},
		{"API Error: 429 rate_limit_error", errRateLimited},
		{"No conversation found with session ID abc", errSessionLost},
		{`Session "e014ee51-aaaa-4bbb-8ccc-ddddeeeeffff" not found locally, restoring conversation from remote...
Error: Failed to restore session from remote: fetching session record: session get failed: 404 Not Found`, errSessionLost},
		{"not found locally", errSessionLost},
		{"Failed to restore session from remote", errSessionLost},
		{"session get failed: 404 Not Found", errSessionLost},
		{"fetch failed: ECONNRESET", errTransient},
		{"API Error: 503 upstream overloaded", errTransient},
		{"TypeError: undefined is not a function", errFatal},
		{"HTTP 404 Not Found", errFatal},
	}
	for _, c := range cases {
		if got := classifyFailure(c.text, 1); got != c.want {
			t.Errorf("classifyFailure(%q) = %q, want %q", truncate(c.text, 40), got, c.want)
		}
	}
}

func TestSummarizeToolNeverLeaksPayloads(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"Bash", `{"command":"go test ./...","description":"Run the test suite"}`, "run the test suite"},
		{"Bash", `{"command":"go build ./..."}`, "running go build ./..."},
		{"Read", `{"file_path":"/srv/app/poller.go"}`, "reading poller.go"},
		{"Edit", `{"file_path":"/srv/app/runner.go"}`, "editing runner.go"},
		{"Grep", `{"pattern":"TODO"}`, "searching for TODO"},
		{"mcp__ccc__ask_owner", `{"question":"deploy?"}`, "asking you a question"},
		{"mcp__ccc__remember", `{"key":"deploy-target","text":"secret stuff"}`, "remembering deploy-target"},
		{"WeirdTool", `{}`, "running WeirdTool"},
	}
	for _, c := range cases {
		got := summarizeTool(c.name, json.RawMessage(c.input))
		if got != c.want {
			t.Errorf("summarizeTool(%s) = %q, want %q", c.name, got, c.want)
		}
	}
	if got := summarizeTool("mcp__ccc__remember", json.RawMessage(`{"key":"k","text":"SECRET"}`)); strings.Contains(got, "SECRET") {
		t.Errorf("tool payload leaked into the progress line: %q", got)
	}
}

func TestSummarizeToolHandlesUnparseableInput(t *testing.T) {
	if got := summarizeTool("Read", json.RawMessage(`not json`)); got == "" {
		t.Error("a broken payload should still produce a label")
	}
}

func TestConsumeEventCollectsTheResult(t *testing.T) {
	r := &Runner{}
	res := &streamResult{}
	lines := []string{
		`{"type":"system","subtype":"init","session_id":"abc"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]}}`,
		`garbage that is not json`,
		`{"type":"result","subtype":"success","is_error":false,"result":"all good","usage":{"input_tokens":10}}`,
	}
	for _, l := range lines {
		r.consumeEvent([]byte(l), res, nil)
	}
	if res.Text != "all good" || res.Subtype != "success" || res.IsError {
		t.Fatalf("unexpected result: %+v", res)
	}
	if !strings.Contains(res.UsageJSON, "input_tokens") {
		t.Errorf("usage was not captured: %q", res.UsageJSON)
	}
	if !res.ok() {
		t.Error("a clean result should be ok()")
	}
}

func TestStreamResultFailureText(t *testing.T) {
	res := &streamResult{IsError: true, Text: "model refused", stderr: "boom", exitCode: 1}
	got := res.failureText()
	if !strings.Contains(got, "boom") || !strings.Contains(got, "model refused") {
		t.Errorf("failureText = %q", got)
	}
	empty := &streamResult{exitCode: 3}
	if !strings.Contains(empty.failureText(), "exited 3") {
		t.Errorf("a silent failure should still describe itself: %q", empty.failureText())
	}
}

func TestBotEnvPassthroughNeverLeaksClaudeVars(t *testing.T) {
	t.Setenv("CCC_TEST_TOKEN", "s3cret")
	t.Setenv("ANTHROPIC_API_KEY", "must-not-pass")
	cfg := &Config{EnvPassthrough: []string{"CCC_TEST_TOKEN", "ANTHROPIC_API_KEY", "CLAUDE_CODE_SOMETHING", ""}}
	env := botEnv(cfg, implicitProfile())
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "CCC_TEST_TOKEN=s3cret") {
		t.Error("an allowed variable was not passed through")
	}
	if strings.Contains(joined, "ANTHROPIC_API_KEY") || strings.Contains(joined, "CLAUDE_CODE_SOMETHING") {
		t.Errorf("a CLAUDE*/ANTHROPIC* variable was passed through:\n%s", joined)
	}
}

func TestMCPConfigPointsAtThisBinary(t *testing.T) {
	dir := t.TempDir()
	r := newRunner(nil, &Config{DataDir: dir}, nil)
	var spec struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(r.mcpConfigJSON(3, 9)), &spec); err != nil {
		t.Fatalf("mcp config is not valid JSON: %v", err)
	}
	srv, ok := spec.MCPServers["ccc"]
	if !ok {
		t.Fatal("no ccc server in the mcp config")
	}
	if srv.Type != "stdio" || srv.Command != cccPath {
		t.Errorf("server = %+v, want a stdio server running this binary", srv)
	}
	if strings.Join(srv.Args, " ") != "mcp --bot 3 --turn 9" {
		t.Errorf("args = %v", srv.Args)
	}
	if srv.Env["CCC_DB"] != dbPath(&Config{DataDir: dir}) {
		t.Errorf("CCC_DB = %q", srv.Env["CCC_DB"])
	}
}

// The queue folds everything waiting into the next turn, so a burst of
// messages costs one `claude -p` run, not one per message.
func TestQueuedInputsAreFoldedIntoOneTurn(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("queuer", "")
	if err != nil {
		t.Fatal(err)
	}
	for i, txt := range []string{"first", "second", "third"} {
		row := &Turn{BotID: b.ID, Source: sourceUser, Input: txt, Status: turnQueued, TriggerMessageID: int64(10 + i)}
		if err := in.db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}

	head, input, triggers, ok := foldQueue(in.db, b.ID)
	if !ok {
		t.Fatal("foldQueue found nothing to run")
	}
	if input != "first\n\nsecond\n\nthird" {
		t.Errorf("folded input = %q", input)
	}
	if len(triggers) != 3 {
		t.Errorf("triggers = %v, want one per message so each gets a ✅", triggers)
	}
	var merged int64
	in.db.Model(&Turn{}).Where("bot_id = ? AND stop_reason LIKE ?", b.ID, "merged%").Count(&merged)
	if merged != 2 {
		t.Errorf("%d turns marked merged, want 2", merged)
	}
	var stillQueued int64
	in.db.Model(&Turn{}).Where("bot_id = ? AND status = ? AND id <> ?", b.ID, turnQueued, head.ID).Count(&stillQueued)
	if stillQueued != 0 {
		t.Errorf("%d turns left queued behind the carrier", stillQueued)
	}

	if _, _, _, ok := foldQueue(in.db, b.ID); !ok {
		t.Error("the carrier turn is still queued until it runs")
	}
}

func TestDisabledBotDoesNotRun(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("paused", "")
	if err != nil {
		t.Fatal(err)
	}
	r := newRunner(in.db, in.cfg, nil)
	if err := in.db.Create(&Turn{BotID: b.ID, Source: sourceUser, Input: "go", Status: turnQueued}).Error; err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{botDisabled, botWaiting} {
		setBotStatus(in.db, b.ID, status)
		if r.runNext(b.ID) {
			t.Errorf("a %s bot must not run turns", status)
		}
	}
}

func TestStopDropsTheQueue(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("stopper", "")
	if err != nil {
		t.Fatal(err)
	}
	r := newRunner(in.db, in.cfg, nil)
	for i := 0; i < 3; i++ {
		if err := r.db.Create(&Turn{BotID: b.ID, Source: sourceUser, Input: "x", Status: turnQueued}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if r.Stop(b.ID) {
		t.Error("Stop should report false when nothing is running")
	}
	var n int64
	r.db.Model(&Turn{}).Where("bot_id = ? AND status = ?", b.ID, turnQueued).Count(&n)
	if n != 0 {
		t.Errorf("%d turns still queued after /stop", n)
	}
}

func TestRenderProgress(t *testing.T) {
	got := renderProgress("running go test", 95*time.Second)
	if !strings.Contains(got, "running go test") || !strings.Contains(got, "1m35s") {
		t.Errorf("progress line = %q", got)
	}
	if !strings.Contains(renderProgress("", time.Second), "working") {
		t.Error("an empty activity should still render something")
	}
	if got := renderProgress("<script>", time.Second); strings.Contains(got, "<script>") {
		t.Errorf("progress line is not HTML-escaped: %q", got)
	}
}

func TestHumanDuration(t *testing.T) {
	cases := map[time.Duration]string{
		5 * time.Second:    "5s",
		90 * time.Second:   "1m30s",
		3 * time.Hour:      "3h00m",
		3670 * time.Second: "1h01m",
	}
	for d, want := range cases {
		if got := humanDuration(d); got != want {
			t.Errorf("humanDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestSplitCommand(t *testing.T) {
	cases := []struct{ in, cmd, rest string }{
		{"/role", "/role", ""},
		{"/role ships things", "/role", "ships things"},
		{"/role@ccc_bot ships things", "/role", "ships things"},
		{"/STOP", "/stop", ""},
	}
	for _, c := range cases {
		cmd, rest := splitCommand(c.in)
		if cmd != c.cmd || rest != c.rest {
			t.Errorf("splitCommand(%q) = (%q,%q), want (%q,%q)", c.in, cmd, rest, c.cmd, c.rest)
		}
	}
}

func TestBotNameFromText(t *testing.T) {
	if got := botNameFromText("fix the login bug\nand write a test"); got != "fix the login bug" {
		t.Errorf("name = %q", got)
	}
	long := strings.Repeat("word ", 30)
	if got := botNameFromText(long); len(got) > 40 {
		t.Errorf("name is %d chars: %q", len(got), got)
	}
}

// Failover (DESIGN §3.4) only works if the profile picker actually takes the
// accounts a turn already burned out of the running.
func TestPickProfileExcludingSkipsTriedAndLoggedOutAccounts(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		DataDir: dir, // no ChatID: markNeedsLogin must not try to message anyone
		Profiles: map[string]*Profile{
			"alpha": {ConfigDir: filepath.Join(dir, "alpha")},
			"beta":  {ConfigDir: filepath.Join(dir, "beta")},
		},
	}
	r := newRunner(nil, cfg, nil)

	first, ok := r.pickProfileExcluding(nil)
	if !ok || first.Name != "alpha" {
		t.Fatalf("first pick = %q (ok=%v), want the deterministic alpha", first.Name, ok)
	}

	second, ok := r.pickProfileExcluding(map[string]bool{"alpha": true})
	if !ok || second.Name != "beta" {
		t.Fatalf("after alpha failed, pick = %q (ok=%v), want beta", second.Name, ok)
	}

	// A profile marked needs_login is skipped like an excluded one.
	r.markNeedsLogin(second)
	third, ok := r.pickProfileExcluding(nil)
	if !ok || third.Name != "alpha" {
		t.Fatalf("pick with beta logged out = %q (ok=%v), want alpha", third.Name, ok)
	}

	// When everything is excluded the turn still gets an account rather than
	// being dropped: a stale needs_login flag must not deadlock the queue.
	last, ok := r.pickProfileExcluding(nil)
	if !ok {
		t.Fatal("no profile returned at all")
	}
	if last.Name == "" {
		t.Error("returned an unnamed profile")
	}

	// With every profile already tried there is nothing left to fail over to.
	if _, ok := r.pickProfileExcluding(map[string]bool{"alpha": true, "beta": true}); ok {
		t.Error("expected no profile when every account has already been tried")
	}
}

func TestPickAccountStaysInsideEngine(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		DataDir: dir,
		Profiles: map[string]*Profile{
			"you@example.com": {Engine: engineClaude, ConfigDir: filepath.Join(dir, "claude")},
			"work":            {Engine: engineGrok, ConfigDir: filepath.Join(dir, "grok-a")},
			"personal":        {Engine: engineGrok, ConfigDir: filepath.Join(dir, "grok-b")},
			"lab":             {Engine: engineAntigravity, ConfigDir: filepath.Join(dir, "agy")},
		},
	}
	r := newRunner(nil, cfg, nil)

	claude, ok := r.pickAccount(engineClaude, nil)
	if !ok || profileEngine(claude) != engineClaude || claude.Name != "you@example.com" {
		t.Fatalf("claude pick = %+v ok=%v", claude, ok)
	}
	grok, ok := r.pickAccount(engineGrok, nil)
	if !ok || profileEngine(grok) != engineGrok {
		t.Fatalf("grok pick = %+v ok=%v", grok, ok)
	}
	if grok.Name != "personal" && grok.Name != "work" {
		t.Fatalf("grok pick left the grok pool: %q", grok.Name)
	}
	next, ok := r.pickAccount(engineGrok, map[string]bool{grok.Name: true})
	if !ok || profileEngine(next) != engineGrok || next.Name == grok.Name {
		t.Fatalf("grok failover = %+v ok=%v (from %q)", next, ok, grok.Name)
	}
	if _, ok := r.pickAccount(engineGrok, map[string]bool{"work": true, "personal": true}); ok {
		t.Error("grok failover must not jump to Claude or Antigravity")
	}
	agy, ok := r.pickAccount(engineAntigravity, nil)
	if !ok || agy.Name != "lab" {
		t.Fatalf("agy pick = %+v ok=%v", agy, ok)
	}
}

// ---------------------------------------------------------------------------
// Input debounce (DESIGN §14.18)
// ---------------------------------------------------------------------------

// queue puts one input in a bot's queue WITHOUT starting its loop, so a test
// can exercise the queue without spawning a real `claude`.
func queue(t *testing.T, db *gorm.DB, botID int64, source, text string) {
	t.Helper()
	if err := db.Create(&Turn{BotID: botID, Source: source, Input: text, Status: turnQueued}).Error; err != nil {
		t.Fatalf("queue: %v", err)
	}
}

// queueAsync is queue for a goroutine, where t.Fatalf is not allowed.
func queueAsync(db *gorm.DB, botID int64, source, text string) {
	if err := db.Create(&Turn{BotID: botID, Source: source, Input: text, Status: turnQueued}).Error; err != nil {
		hookLog("test queue: %v", err)
	}
}

// Two messages typed a moment apart become ONE turn: the runner waits out the
// debounce window before it folds the queue, so a burst of chat costs one
// `claude -p` run instead of one per line.
func TestDebounceCoalescesABurstIntoOneTurn(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("typer", "")
	if err != nil {
		t.Fatal(err)
	}
	setDebounce(t, in, 400)
	// The rows are created directly rather than through Enqueue: Enqueue also
	// starts the bot's loop, which would spawn a real `claude` process. What is
	// under test is the wait, and the wait reads the queue.
	r := newRunner(in.db, in.cfg, nil)
	t.Cleanup(r.Close)

	queue(t, in.db, b.ID, sourceUser, "first half of the thought")
	go func() {
		time.Sleep(150 * time.Millisecond)
		queueAsync(in.db, b.ID, sourceUser, "and the correction")
	}()

	start := time.Now()
	r.settleQueue(b.ID)
	waited := time.Since(start)
	if waited < 400*time.Millisecond {
		t.Errorf("settled after %v; the window must be measured from the LAST message", waited)
	}

	_, input, _, ok := foldQueue(in.db, b.ID)
	if !ok {
		t.Fatal("nothing to run after the debounce window")
	}
	if input != "first half of the thought\n\nand the correction" {
		t.Errorf("folded input = %q, want both messages in one turn", input)
	}
}

// One message still runs, after the window and no longer.
func TestDebounceReleasesASingleMessage(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("lonely", "")
	if err != nil {
		t.Fatal(err)
	}
	setDebounce(t, in, 300)
	r := newRunner(in.db, in.cfg, nil)
	t.Cleanup(r.Close)
	queue(t, in.db, b.ID, sourceUser, "just this")

	start := time.Now()
	r.settleQueue(b.ID)
	waited := time.Since(start)
	if waited < 250*time.Millisecond || waited > 2*time.Second {
		t.Errorf("settled after %v, want roughly the 300ms window", waited)
	}
	if _, _, _, ok := foldQueue(in.db, b.ID); !ok {
		t.Error("the message must run once the window has passed")
	}
}

// Machine-made inputs are not a burst of typing: a watch, a schedule or another
// bot delivers one thing and it runs at once.
func TestDebounceDoesNotDelayMachineInputs(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("watched", "")
	if err != nil {
		t.Fatal(err)
	}
	setDebounce(t, in, 5000)
	r := newRunner(in.db, in.cfg, nil)
	t.Cleanup(r.Close)
	queue(t, in.db, b.ID, sourceWatch, "the build went red")

	start := time.Now()
	r.settleQueue(b.ID)
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("a watch waited %v for a debounce window it should skip", waited)
	}
}

func TestDebounceCanBeTurnedOff(t *testing.T) {
	in, _, _ := testInstance(t)
	r := newRunner(in.db, in.cfg, nil)
	t.Cleanup(r.Close)
	if got := r.debounceDuration(); got != defaultDebounceMS*time.Millisecond {
		t.Errorf("default debounce = %v, want %dms", got, defaultDebounceMS)
	}
	setDebounce(t, in, 0)
	if got := r.debounceDuration(); got != 0 {
		t.Errorf("debounce_ms = 0 must disable the wait, got %v", got)
	}
	setDebounce(t, in, 999999)
	if got := r.debounceDuration(); got != maxDebounceMS*time.Millisecond {
		t.Errorf("an absurd debounce_ms must be capped, got %v", got)
	}
}

// The plain-turn flags (memory compaction) carry the same isolation as a bot's
// turn, and no MCP server at all.
func TestClaudePlainArgsCarryNoTools(t *testing.T) {
	args := strings.Join(claudePlainArgs("haiku", "0d9d9e5a-3d1c-4f1e-8a77-4c2f2b1d9b11"), " ")
	for _, want := range []string{
		"-p", "--output-format text", "--session-id 0d9d9e5a", "--setting-sources  ",
		"--disable-slash-commands", "--strict-mcp-config", `--mcp-config {"mcpServers":{}}`, "--model haiku",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("plain-turn args are missing %q: %s", want, args)
		}
	}
	for _, unwanted := range []string{"--resume", "bypassPermissions", "stream-json", "ccc mcp"} {
		if strings.Contains(args, unwanted) {
			t.Errorf("plain-turn args should not carry %q: %s", unwanted, args)
		}
	}
	if strings.Contains(strings.Join(claudePlainArgs("", "x"), " "), "--model") {
		t.Error("no compaction model means claude's own default, not an empty --model")
	}
}

// setDebounce points the instance's config at a debounce value; the knob is a
// config.json key, not a settings-table row.
func setDebounce(t *testing.T, in *instance, ms int) {
	t.Helper()
	in.mu.Lock()
	in.cfg.DebounceMS = &ms
	in.mu.Unlock()
}
