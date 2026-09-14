package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseEngineAliasesAndDefault(t *testing.T) {
	cases := []struct {
		in, want string
		fail     bool
	}{
		{"", engineClaude, false},
		{"claude", engineClaude, false},
		{"CLAUDE", engineClaude, false},
		{"grok", engineGrok, false},
		{"grok-build", engineGrok, false},
		{"agy", engineAntigravity, false},
		{"antigravity", engineAntigravity, false},
		{"carbon-copy-cloner", "", true},
		{"chatgpt", "", true},
	}
	for _, c := range cases {
		got, err := parseEngine(c.in)
		if c.fail {
			if err == nil {
				t.Errorf("parseEngine(%q) should fail", c.in)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("parseEngine(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	if botEngine(nil) != engineClaude || botEngine(&Bot{}) != engineClaude {
		t.Error("an empty bot must run on Claude")
	}
	if botEngine(&Bot{Engine: "nope"}) != engineClaude {
		t.Error("an unknown stored engine must fall back to Claude, not fail the turn")
	}
}

func TestGrokTurnArgsMintAndResume(t *testing.T) {
	args := grokTurnArgs("grok-build", "SYS", "11111111-2222-4333-8444-555555555555", false)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--always-approve",
		"--output-format streaming-messages-json",
		"--system-prompt-override SYS",
		"--model grok-build",
		"--session-id 11111111-2222-4333-8444-555555555555",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in: %s", want, joined)
		}
	}
	if strings.Contains(joined, "--resume") {
		t.Error("a first turn must not also pass --resume")
	}
	if strings.Contains(joined, "--mcp-config") || strings.Contains(joined, "CLAUDE_CONFIG_DIR") {
		t.Error("grok must not carry Claude MCP or a fake config dir")
	}

	resume := strings.Join(grokTurnArgs("", "", "sess-1", true), " ")
	if !strings.Contains(resume, "--resume sess-1") {
		t.Errorf("resume flag missing: %s", resume)
	}
	if strings.Contains(resume, "--session-id") {
		t.Error("a resume must not also pass --session-id")
	}
	if strings.Contains(resume, "--model") {
		t.Error("no model configured should mean no --model flag")
	}
}

func TestAgyTurnArgsMintAndResume(t *testing.T) {
	first := strings.Join(agyTurnArgs("gemini-3.5-flash-medium", "", false), " ")
	for _, want := range []string{
		"--output-format stream-json",
		"--dangerously-skip-permissions",
		"--print-timeout 120m",
		"--model gemini-3.5-flash-medium",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("missing %q in: %s", want, first)
		}
	}
	if strings.Contains(first, "--conversation") {
		t.Error("a first turn must not pass --conversation; agy mints the id")
	}

	resume := strings.Join(agyTurnArgs("", "conv-9", true), " ")
	if !strings.Contains(resume, "--conversation conv-9") {
		t.Errorf("resume flag missing: %s", resume)
	}
	if strings.Contains(resume, "--mcp-config") {
		t.Error("agy must not carry Claude MCP")
	}
}

func TestBuildTurnDispatchesEngines(t *testing.T) {
	dir := t.TempDir()
	writeFakeCLI(t, dir, "claude", "")
	writeFakeCLI(t, dir, "grok", "")
	writeFakeCLI(t, dir, "agy", "")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := &Config{Model: "sonnet"}
	p := implicitProfile()

	claude, err := buildTurn(engineClaude, p, cfg, `{"mcpServers":{}}`, "sid-1", "SYS", "hello", false)
	if err != nil {
		t.Fatal(err)
	}
	if claude.Engine != engineClaude || claude.Stream != streamClaudeMessages {
		t.Errorf("claude spec = %+v", claude)
	}
	if !strings.Contains(strings.Join(claude.Args, " "), "-p") ||
		!strings.Contains(strings.Join(claude.Args, " "), "--output-format stream-json") {
		t.Errorf("claude args lost the existing flag set: %v", claude.Args)
	}
	if claude.Args[len(claude.Args)-1] != "hello" {
		t.Errorf("claude prompt should stay a trailing positional, got %q", claude.Args[len(claude.Args)-1])
	}

	grok, err := buildTurn(engineGrok, p, cfg, "MUST-NOT-APPEAR", "sid-2", "SYS", "hello", false)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(grok.Args, " ")
	if grok.Engine != engineGrok || grok.Stream != streamClaudeMessages {
		t.Errorf("grok spec = %+v", grok)
	}
	if !strings.Contains(joined, "--single hello") || !strings.Contains(joined, "--always-approve") {
		t.Errorf("grok args = %v", grok.Args)
	}
	if strings.Contains(joined, "MUST-NOT-APPEAR") {
		t.Error("grok received the Claude MCP config")
	}
	if filepath.Base(grok.Bin) != "grok" {
		t.Errorf("grok bin = %q", grok.Bin)
	}

	agy, err := buildTurn(engineAntigravity, p, cfg, "", "", "SYS", "hello", false)
	if err != nil {
		t.Fatal(err)
	}
	joined = strings.Join(agy.Args, " ")
	if agy.Engine != engineAntigravity || agy.Stream != streamAgy {
		t.Errorf("agy spec = %+v", agy)
	}
	if !strings.Contains(joined, "--print") || !strings.Contains(joined, "hello") ||
		!strings.Contains(joined, "SYS") {
		t.Errorf("agy should prepend the system prompt and pass --print: %v", agy.Args)
	}
	if strings.Contains(joined, "--conversation") {
		t.Error("first agy turn must not resume")
	}

	resumed, err := buildTurn(engineAntigravity, p, cfg, "", "conv-1", "SYS", "hello", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(resumed.Args, " "), "--conversation conv-1") {
		t.Errorf("agy resume args = %v", resumed.Args)
	}

	alias, err := buildTurn("grok-build", p, cfg, "", "sid", "SYS", "hi", true)
	if err != nil {
		t.Fatal(err)
	}
	if alias.Engine != engineGrok || !strings.Contains(strings.Join(alias.Args, " "), "--resume sid") {
		t.Errorf("grok-build alias did not dispatch: %+v", alias)
	}
}

func TestBuildTurnClaudeArgsMatchClaudeTurnArgs(t *testing.T) {
	// The dispatch layer must not drift from the verified Claude flag set.
	want := append(claudeTurnArgs("sonnet", "SYS", `{"mcpServers":{}}`, "sid", false), "hello")
	got, err := buildTurn(engineClaude, implicitProfile(), &Config{Model: "sonnet"}, `{"mcpServers":{}}`, "sid", "SYS", "hello", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Args, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("buildTurn claude args drifted\n got %v\nwant %v", got.Args, want)
	}
}

func TestResolveEngineBinMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	if _, err := resolveEngineBin(engineGrok); err == nil {
		t.Error("expected a missing grok binary to be an error")
	}
	if _, err := resolveEngineBin(engineAntigravity); err == nil {
		t.Error("expected a missing agy binary to be an error")
	}
}

func TestEngineEnvNeverSetsClaudeConfigDir(t *testing.T) {
	t.Setenv("CCC_TEST_TOKEN", "s3cret")
	t.Setenv("XAI_API_KEY", "xai-key")
	t.Setenv("ANTHROPIC_API_KEY", "must-not-pass")
	cfg := &Config{EnvPassthrough: []string{"CCC_TEST_TOKEN", "ANTHROPIC_API_KEY"}}
	p := Profile{Name: "alpha", ConfigDir: "/tmp/claude-profile"}

	joined := strings.Join(engineEnv(cfg, engineGrok, p), "\n")
	if strings.Contains(joined, "CLAUDE_CONFIG_DIR") {
		t.Errorf("grok env invented a Claude config dir:\n%s", joined)
	}
	if !strings.Contains(joined, "CCC_TEST_TOKEN=s3cret") {
		t.Error("passthrough was not applied to grok")
	}
	if !strings.Contains(joined, "XAI_API_KEY=xai-key") {
		t.Error("Grok auth env was not passed through")
	}
	if strings.Contains(joined, "ANTHROPIC_API_KEY") {
		t.Error("ANTHROPIC_* must not reach a non-Claude engine via passthrough")
	}

	claude := strings.Join(engineEnv(cfg, engineClaude, p), "\n")
	if !strings.Contains(claude, "CLAUDE_CONFIG_DIR=/tmp/claude-profile") {
		t.Error("Claude must still get its profile config dir")
	}
}

func TestConsumeAgyEventCollectsResultAndSession(t *testing.T) {
	res := &streamResult{}
	lines := []string{
		`{"event":"init","conversation_id":"conv-abc","init":{"cwd":"/tmp"}}`,
		`{"event":"step_update","step_update":{"conversation_id":"conv-abc","step_type":"tool","tool_name":"run_command","tool_info":{"name":"run_command","parameters":{"CommandLine":"ls"}}}}`,
		`{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"done"}}`,
		`garbage`,
		`{"event":"result","result":{"conversation_id":"conv-abc","status":"SUCCESS","response":"all good","usage":{"input_tokens":10}}}`,
	}
	for _, l := range lines {
		consumeAgyEvent([]byte(l), res, nil)
	}
	if res.Text != "all good" || res.IsError || res.SessionID != "conv-abc" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if !strings.Contains(res.UsageJSON, "input_tokens") {
		t.Errorf("usage was not captured: %q", res.UsageJSON)
	}

	fail := &streamResult{}
	consumeAgyEvent([]byte(`{"event":"result","result":{"status":"ERROR","error":"authentication required"}}`), fail, nil)
	if !fail.IsError || fail.Text != "authentication required" {
		t.Errorf("error result = %+v", fail)
	}
}

func TestConsumeTurnEventDispatches(t *testing.T) {
	claude := &streamResult{}
	if !consumeTurnEvent(streamClaudeMessages, []byte(`{"type":"result","result":"hi","is_error":false}`), claude, nil) {
		t.Fatal("claude stream line should parse")
	}
	if claude.Text != "hi" {
		t.Errorf("claude text = %q", claude.Text)
	}
	agy := &streamResult{}
	if !consumeTurnEvent(streamAgy, []byte(`{"event":"result","result":{"status":"SUCCESS","response":"yo"}}`), agy, nil) {
		t.Fatal("agy stream line should parse")
	}
	if agy.Text != "yo" {
		t.Errorf("agy text = %q", agy.Text)
	}
}

func TestDefaultEngineFromConfig(t *testing.T) {
	if defaultEngine(nil) != engineClaude || defaultEngine(&Config{}) != engineClaude {
		t.Fatal("unset default_engine must be claude")
	}
	if defaultEngine(&Config{DefaultEngine: "agy"}) != engineAntigravity {
		t.Fatal("agy alias should become antigravity")
	}
	if defaultEngine(&Config{DefaultEngine: "nope"}) != engineClaude {
		t.Fatal("a typo in config must not break new bots")
	}
}

func TestConfigDefaultEngine(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := configCommand([]string{"set", "default_engine", "grok-build"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultEngine != engineGrok {
		t.Errorf("persisted %q, want grok", cfg.DefaultEngine)
	}
	got, err := configGet(cfg, "default_engine")
	if err != nil || got != engineGrok {
		t.Errorf("config get default_engine = %q, %v", got, err)
	}
	if err := configCommand([]string{"set", "default_engine", "claude"}); err != nil {
		t.Fatal(err)
	}
	cfg, _ = loadConfig()
	if cfg.DefaultEngine != "" {
		t.Errorf("claude should store as unset, got %q", cfg.DefaultEngine)
	}
	if err := configCommand([]string{"set", "default_engine", "not-a-cli"}); err == nil {
		t.Error("unknown engine should be rejected")
	}
}

func TestClassifyFailureGrokAndAgyAuth(t *testing.T) {
	if got := classifyFailure("authentication required", 1); got != errAuthStale {
		t.Errorf("agy auth = %q", got)
	}
	if got := classifyFailure("Error: please run `grok login`", 1); got != errAuthStale {
		t.Errorf("grok login = %q", got)
	}
}

func TestRenderSystemPromptOmitsMCPForGrok(t *testing.T) {
	got := renderSystemPrompt(
		promptBot{Name: "coder", Role: "writes go", Cwd: "/tmp", Engine: engineGrok},
		"host", nil, []string{"🚀"},
	)
	if strings.Contains(got, "ccc MCP tools:\n  remember") || strings.Contains(got, "ask_owner") && strings.Contains(got, "Prefer ask_owner") {
		t.Errorf("grok prompt still describes Claude MCP:\n%s", got)
	}
	if !strings.Contains(got, "Grok Build") || !strings.Contains(got, "do NOT have the ccc MCP") {
		t.Errorf("grok prompt should name the engine and skip MCP:\n%s", got)
	}
	claude := renderSystemPrompt(promptBot{Name: "coder", Role: "writes go", Cwd: "/tmp"}, "host", nil, nil)
	if !strings.Contains(claude, "a group of Claude bots") || !strings.Contains(claude, "remember/recall/forget") {
		t.Error("Claude prompt must stay the original text")
	}
}

func writeFakeCLI(t *testing.T, dir, name, script string) string {
	t.Helper()
	if script == "" {
		script = "#!/bin/sh\nexit 0\n"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

type recUI struct{}

func (recUI) Post(int64, string) (int64, error) { return 1, nil }
func (recUI) Edit(int64, int64, string) error   { return nil }
func (recUI) React(int64, string)               {}

func TestGrokEngineTurnWithFakeBinary(t *testing.T) {
	binDir := t.TempDir()
	argsFile := filepath.Join(binDir, "args.txt")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > '" + argsFile + "'\n" +
		"echo '{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"hi\"}]}}'\n" +
		"echo '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"hello from grok\",\"usage\":{\"input_tokens\":1}}'\n"
	writeFakeCLI(t, binDir, "grok", script)
	t.Setenv("PATH", binDir)
	t.Setenv("HOME", t.TempDir())

	in, _, _ := testInstance(t)
	zero := 0
	in.cfg.DebounceMS = &zero
	b, err := in.createBot("grokbot", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := in.db.Model(&Bot{}).Where("id = ?", b.ID).Update("engine", engineGrok).Error; err != nil {
		t.Fatal(err)
	}

	r := newRunner(in.db, in.cfg, recUI{})
	t.Cleanup(r.Close)
	if err := in.db.Create(&Turn{BotID: b.ID, Source: sourceUser, Input: "hi there", Status: turnQueued}).Error; err != nil {
		t.Fatal(err)
	}
	if !r.runNext(b.ID) {
		t.Fatal("the grok turn did not run")
	}
	var turn Turn
	if err := in.db.Where("bot_id = ? AND source = ?", b.ID, sourceUser).Order("id DESC").First(&turn).Error; err != nil {
		t.Fatal(err)
	}
	if turn.Status != turnDone || turn.Output != "hello from grok" {
		t.Fatalf("turn status=%s output=%q error=%s", turn.Status, turn.Output, turn.StopReason)
	}
	if turn.Profile != engineGrok {
		t.Errorf("profile = %q, want grok (not a Claude account)", turn.Profile)
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := string(raw)
	for _, want := range []string{"--single", "--always-approve", "streaming-messages-json"} {
		if !strings.Contains(args, want) {
			t.Errorf("fake grok args missing %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "--mcp-config") || strings.Contains(args, "CLAUDE_CONFIG_DIR") {
		t.Errorf("grok must not see Claude MCP or config dir:\n%s", args)
	}
	after, _ := botByID(in.db, b.ID)
	if after.SessionID == "" {
		t.Error("the first grok turn should have stored the minted session id")
	}
}

func TestAgyEngineTurnWithFakeBinary(t *testing.T) {
	binDir := t.TempDir()
	argsFile := filepath.Join(binDir, "args.txt")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > '" + argsFile + "'\n" +
		"echo '{\"event\":\"init\",\"conversation_id\":\"conv-xyz\"}'\n" +
		"echo '{\"event\":\"result\",\"result\":{\"conversation_id\":\"conv-xyz\",\"status\":\"SUCCESS\",\"response\":\"hello from agy\"}}'\n"
	writeFakeCLI(t, binDir, "agy", script)
	t.Setenv("PATH", binDir)
	t.Setenv("HOME", t.TempDir())

	in, _, _ := testInstance(t)
	zero := 0
	in.cfg.DebounceMS = &zero
	b, err := in.createBot("agybot", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := in.db.Model(&Bot{}).Where("id = ?", b.ID).Update("engine", engineAntigravity).Error; err != nil {
		t.Fatal(err)
	}

	r := newRunner(in.db, in.cfg, recUI{})
	t.Cleanup(r.Close)
	if err := in.db.Create(&Turn{BotID: b.ID, Source: sourceUser, Input: "hi", Status: turnQueued}).Error; err != nil {
		t.Fatal(err)
	}
	if !r.runNext(b.ID) {
		t.Fatal("the agy turn did not run")
	}
	var turn Turn
	if err := in.db.Where("bot_id = ?", b.ID).Order("id DESC").First(&turn).Error; err != nil {
		t.Fatal(err)
	}
	if turn.Status != turnDone || turn.Output != "hello from agy" {
		t.Fatalf("turn status=%s output=%q error=%s", turn.Status, turn.Output, turn.StopReason)
	}
	after, _ := botByID(in.db, b.ID)
	if after.SessionID != "conv-xyz" {
		t.Errorf("session_id = %q, want the conversation_id agy minted", after.SessionID)
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := string(raw)
	if !strings.Contains(args, "--print") || !strings.Contains(args, "stream-json") ||
		!strings.Contains(args, "--dangerously-skip-permissions") {
		t.Errorf("fake agy args:\n%s", args)
	}
	if strings.Contains(args, "--conversation") {
		t.Error("first agy turn must not resume")
	}

	// Second turn should resume that conversation.
	if err := os.Remove(argsFile); err != nil {
		t.Fatal(err)
	}
	if err := in.db.Create(&Turn{BotID: b.ID, Source: sourceUser, Input: "again", Status: turnQueued}).Error; err != nil {
		t.Fatal(err)
	}
	if !r.runNext(b.ID) {
		t.Fatal("the resumed agy turn did not run")
	}
	raw, err = os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "--conversation") || !strings.Contains(string(raw), "conv-xyz") {
		t.Errorf("second agy turn should resume conv-xyz:\n%s", raw)
	}
}

func TestPlainTextEngineFallbackStillDelivers(t *testing.T) {
	binDir := t.TempDir()
	writeFakeCLI(t, binDir, "grok", "#!/bin/sh\necho 'just the answer'\n")
	t.Setenv("PATH", binDir)
	t.Setenv("HOME", t.TempDir())

	in, _, _ := testInstance(t)
	zero := 0
	in.cfg.DebounceMS = &zero
	b, err := in.createBot("plain", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := in.db.Model(&Bot{}).Where("id = ?", b.ID).Update("engine", engineGrok).Error; err != nil {
		t.Fatal(err)
	}
	r := newRunner(in.db, in.cfg, recUI{})
	t.Cleanup(r.Close)
	if err := in.db.Create(&Turn{BotID: b.ID, Source: sourceUser, Input: "hi", Status: turnQueued}).Error; err != nil {
		t.Fatal(err)
	}
	if !r.runNext(b.ID) {
		t.Fatal("did not run")
	}
	var turn Turn
	in.db.Where("bot_id = ?", b.ID).Order("id DESC").First(&turn)
	if turn.Status != turnDone || turn.Output != "just the answer" {
		t.Fatalf("plain-text fallback failed: status=%s output=%q err=%s", turn.Status, turn.Output, turn.StopReason)
	}
}

func TestCreateBotUsesDefaultEngine(t *testing.T) {
	in, _, _ := testInstance(t)
	in.cfg.DefaultEngine = engineGrok
	b, err := in.createBot("from-default", "")
	if err != nil {
		t.Fatal(err)
	}
	if botEngine(b) != engineGrok {
		t.Errorf("engine = %q, want grok from default_engine", b.Engine)
	}
}

func TestSpawnedBotInheritsParentEngine(t *testing.T) {
	in, _, _ := testInstance(t)
	parent, err := in.createBot("lead", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := in.db.Model(&Bot{}).Where("id = ?", parent.ID).Update("engine", engineAntigravity).Error; err != nil {
		t.Fatal(err)
	}
	child, err := createBotRow(in.db, in.cfg, "helper", "helps", "", &parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if botEngine(child) != engineAntigravity {
		t.Errorf("child engine = %q, want the parent's antigravity", child.Engine)
	}
}

func TestSessionForAntigravityMintsNothing(t *testing.T) {
	r := &Runner{}
	id, resume := r.sessionFor(&Bot{Engine: engineAntigravity})
	if id != "" || resume {
		t.Errorf("first agy session = %q resume=%v; agy must mint the id", id, resume)
	}
	id, resume = r.sessionFor(&Bot{Engine: engineGrok})
	if id == "" || resume {
		t.Errorf("first grok session = %q resume=%v; ccc should mint a UUID", id, resume)
	}
	id, resume = r.sessionFor(&Bot{Engine: engineClaude, SessionID: "kept"})
	if id != "kept" || !resume {
		t.Errorf("claude resume broken: %q %v", id, resume)
	}
}
