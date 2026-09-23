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
		{"codex", engineCodex, false},
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
	writeFakeCLI(t, dir, "codex", "")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := &Config{Model: "sonnet"}
	p := implicitProfile()

	claude, err := buildTurn(engineClaude, p, cfg, `{"mcpServers":{}}`, "sid-1", "SYS", "hello", "sonnet", false)
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

	grok, err := buildTurn(engineGrok, p, cfg, "MUST-NOT-APPEAR", "sid-2", "SYS", "hello", "", false)
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
	if strings.Contains(joined, "sonnet") {
		t.Error("grok must not inherit the Claude instance model")
	}
	if filepath.Base(grok.Bin) != "grok" {
		t.Errorf("grok bin = %q", grok.Bin)
	}

	agy, err := buildTurn(engineAntigravity, p, cfg, "", "", "SYS", "hello", "", false)
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

	resumed, err := buildTurn(engineAntigravity, p, cfg, "", "conv-1", "SYS", "hello", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(resumed.Args, " "), "--conversation conv-1") {
		t.Errorf("agy resume args = %v", resumed.Args)
	}

	alias, err := buildTurn("grok-build", p, cfg, "", "sid", "SYS", "hi", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if alias.Engine != engineGrok || !strings.Contains(strings.Join(alias.Args, " "), "--resume sid") {
		t.Errorf("grok-build alias did not dispatch: %+v", alias)
	}

	codex, err := buildTurn(engineCodex, p, cfg, "MUST-NOT-APPEAR", "", "SYS", "hello", "gpt-5.4", false)
	if err != nil {
		t.Fatal(err)
	}
	joined = strings.Join(codex.Args, " ")
	if codex.Engine != engineCodex || codex.Stream != streamCodex {
		t.Errorf("codex spec = %+v", codex)
	}
	for _, want := range []string{"exec", "--json", "--sandbox danger-full-access", "--model gpt-5.4", "hello"} {
		if !strings.Contains(joined, want) {
			t.Errorf("codex args missing %q: %v", want, codex.Args)
		}
	}
	if strings.Contains(joined, "resume") {
		t.Error("first codex turn must not resume")
	}
	if strings.Contains(joined, "MUST-NOT-APPEAR") {
		t.Error("codex received the Claude MCP config")
	}

	codexResume, err := buildTurn(engineCodex, p, cfg, "", "thread-1", "SYS", "hello", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(codexResume.Args, " "), "resume thread-1") {
		t.Errorf("codex resume args = %v", codexResume.Args)
	}
}

func TestBuildTurnClaudeArgsMatchClaudeTurnArgs(t *testing.T) {
	// The dispatch layer must not drift from the verified Claude flag set.
	want := append(claudeTurnArgs("sonnet", "SYS", `{"mcpServers":{}}`, "sid", false), "hello")
	got, err := buildTurn(engineClaude, implicitProfile(), &Config{Model: "sonnet"}, `{"mcpServers":{}}`, "sid", "SYS", "hello", "sonnet", false)
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
	if _, err := resolveEngineBin(engineCodex); err == nil {
		t.Error("expected a missing codex binary to be an error")
	}
}

func TestAppendTurnIdentity(t *testing.T) {
	cfg := &Config{DataDir: t.TempDir()}
	got := strings.Join(appendTurnIdentity(nil, cfg, 7, 9), "\n")
	for _, want := range []string{"CCC_BOT_ID=7", "CCC_TURN_ID=9", "CCC_CONFIG=", "CCC_DB="} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if !strings.Contains(got, dbPath(cfg)) {
		t.Errorf("CCC_DB does not point at the instance db:\n%s", got)
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

	grokP := Profile{Name: "work", Engine: engineGrok, ConfigDir: "/tmp/grok-home"}
	t.Setenv("GROK_HOME", "/tmp/parent-grok")
	grokIso := strings.Join(engineEnv(cfg, engineGrok, grokP), "\n")
	if !strings.Contains(grokIso, "GROK_HOME=/tmp/grok-home") {
		t.Errorf("registered grok account must pin GROK_HOME:\n%s", grokIso)
	}
	if strings.Contains(grokIso, "/tmp/parent-grok") {
		t.Error("parent GROK_HOME must not leak into a registered grok account")
	}

	agyP := Profile{Name: "lab", Engine: engineAntigravity, ConfigDir: "/tmp/agy-home"}
	t.Setenv("GEMINI_HOME", "/tmp/real-gemini")
	agyIso := strings.Join(engineEnv(cfg, engineAntigravity, agyP), "\n")
	if !strings.Contains(agyIso, "HOME=/tmp/agy-home") ||
		!strings.Contains(agyIso, "GEMINI_HOME=/tmp/agy-home/.gemini") ||
		!strings.Contains(agyIso, "GEMINI_FORCE_FILE_STORAGE=true") {
		t.Errorf("registered agy account must isolate HOME/GEMINI_HOME:\n%s", agyIso)
	}
	if strings.Contains(agyIso, "/tmp/real-gemini") {
		t.Error("parent GEMINI_HOME must not leak into a registered agy account")
	}
	if strings.Contains(agyIso, "CLAUDE_CONFIG_DIR") {
		t.Error("agy isolation must not invent a Claude config dir")
	}

	codexP := Profile{Name: "openai", Engine: engineCodex, ConfigDir: "/tmp/codex-home"}
	t.Setenv("CODEX_HOME", "/tmp/parent-codex")
	codexIso := strings.Join(engineEnv(cfg, engineCodex, codexP), "\n")
	if !strings.Contains(codexIso, "CODEX_HOME=/tmp/codex-home") {
		t.Errorf("registered codex account must pin CODEX_HOME:\n%s", codexIso)
	}
	if strings.Contains(codexIso, "/tmp/parent-codex") {
		t.Error("parent CODEX_HOME must not leak into a registered codex account")
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

func TestConsumeCodexEventCollectsThreadAndMessage(t *testing.T) {
	res := &streamResult{}
	lines := []string{
		`{"type":"thread.started","thread_id":"thread-xyz"}`,
		`{"type":"item.started","item":{"type":"command_execution","command":"go test"}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"all green"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":3}}`,
	}
	for _, l := range lines {
		if !consumeCodexEvent([]byte(l), res, nil) {
			t.Errorf("codex line should parse: %s", l)
		}
	}
	if res.SessionID != "thread-xyz" || res.Text != "all green" || res.IsError {
		t.Fatalf("unexpected result: %+v", res)
	}

	rpc := &streamResult{}
	consumeCodexEvent([]byte(`{"method":"thread/started","params":{"thread_id":"from-params"}}`), rpc, nil)
	if rpc.SessionID != "from-params" {
		t.Errorf("rpc thread id = %q", rpc.SessionID)
	}
	errRes := &streamResult{}
	consumeCodexEvent([]byte(`{"type":"error","error":{"message":"please run `+"`codex login`"+`"}}`), errRes, nil)
	if !errRes.IsError || !strings.Contains(errRes.Text, "codex login") {
		t.Errorf("error event = %+v", errRes)
	}
}

func TestResolveModelPrecedence(t *testing.T) {
	cfg := &Config{Model: "sonnet", Models: map[string]string{engineGrok: "grok-4", engineClaude: "opus"}}
	if got := resolveModel(cfg, engineClaude, ""); got != "opus" {
		t.Errorf("claude instance = %q, want opus (Models wins over legacy Model)", got)
	}
	if got := resolveModel(cfg, engineClaude, "haiku"); got != "haiku" {
		t.Errorf("bot override = %q, want haiku", got)
	}
	if got := resolveModel(cfg, engineGrok, ""); got != "grok-4" {
		t.Errorf("grok = %q", got)
	}
	if got := resolveModel(cfg, engineCodex, ""); got != "" {
		t.Errorf("unset engine must be empty so the CLI picks, got %q", got)
	}
	if got := resolveModel(&Config{Model: "sonnet"}, engineGrok, ""); got != "" {
		t.Errorf("legacy Model must not leak onto grok, got %q", got)
	}
}

func TestProfileEffectiveModelUsesCLIWhenUnset(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"model":"fable[1m]"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := Profile{Name: "you@example.com", Engine: engineClaude, ConfigDir: dir}
	if got := profileEffectiveModel(&Config{}, p); got != "fable[1m]" {
		t.Errorf("cli stored = %q, want fable[1m]", got)
	}
	cfg := &Config{Models: map[string]string{engineClaude: "opus"}}
	if got := profileEffectiveModel(cfg, p); got != "opus" {
		t.Errorf("ccc override = %q, want opus", got)
	}
	grok := Profile{Name: "me/grok", Engine: engineGrok, ConfigDir: t.TempDir()}
	if got := profileEffectiveModel(&Config{}, grok); got != "" {
		t.Errorf("unset grok must stay empty, got %q", got)
	}
}

func TestGrokCachedModelIDs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "models_cache.json"), []byte(`{"models":{"grok-4.6":{},"grok-4.5":{}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := grokCachedModelIDs(Profile{Engine: engineGrok, ConfigDir: dir})
	if len(got) != 2 || got[0] != "grok-4.5" || got[1] != "grok-4.6" {
		t.Errorf("got %v, want sorted grok-4.5, grok-4.6", got)
	}
	if got := grokCachedModelIDs(Profile{Engine: engineGrok, ConfigDir: t.TempDir()}); len(got) != 0 {
		t.Errorf("missing cache = %v", got)
	}
}

func TestCodexTurnArgsMintAndResume(t *testing.T) {
	first := strings.Join(codexTurnArgs("gpt-5.4", "", "do the thing", false), " ")
	for _, want := range []string{
		"exec", "--json", "--sandbox danger-full-access",
		"--dangerously-bypass-approvals-and-sandbox", "--skip-git-repo-check",
		"--model gpt-5.4", "do the thing",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("missing %q in: %s", want, first)
		}
	}
	if strings.Contains(first, "resume") {
		t.Error("a first turn must not pass resume")
	}
	resume := strings.Join(codexTurnArgs("", "thread-1", "continue", true), " ")
	if !strings.Contains(resume, "resume thread-1 continue") {
		t.Errorf("resume args = %s", resume)
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
	codex := &streamResult{}
	if !consumeTurnEvent(streamCodex, []byte(`{"type":"thread.started","thread_id":"t1"}`), codex, nil) {
		t.Fatal("codex stream line should parse")
	}
	if codex.SessionID != "t1" {
		t.Errorf("codex session = %q", codex.SessionID)
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

func TestDefaultEngineFromDefaultAccount(t *testing.T) {
	cfg := &Config{
		Profiles: map[string]*Profile{
			"you@example.com": {Engine: engineClaude, ConfigDir: "/tmp/c"},
			"work":            {Engine: engineGrok, ConfigDir: "/tmp/g"},
		},
		DefaultProfile: "work",
	}
	if defaultEngine(cfg) != engineGrok {
		t.Fatalf("new bots should inherit the default account's engine, got %q", defaultEngine(cfg))
	}
	cfg.DefaultEngine = engineClaude
	if defaultEngine(cfg) != engineClaude {
		t.Fatal("explicit default_engine still wins over the default account")
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

func TestRenderSystemPromptGrokGetsMCP(t *testing.T) {
	got := renderSystemPrompt(
		promptBot{Name: "coder", Role: "writes go", Cwd: "/tmp", Engine: engineGrok},
		"host", nil,
	)
	if !strings.Contains(got, "Grok Build") {
		t.Errorf("grok prompt should name the engine:\n%s", got)
	}
	if !strings.Contains(got, "ccc MCP tools") || !strings.Contains(got, "report_to_general") {
		t.Errorf("grok worker prompt should describe ccc MCP:\n%s", got)
	}
	if !strings.Contains(got, "search_tool") {
		t.Errorf("grok prompt should teach search_tool for MCP:\n%s", got)
	}
	if strings.Contains(got, "spawn_session") || strings.Contains(got, "ccc tell") || strings.Contains(got, "send_to_bot") {
		t.Errorf("grok worker must not spawn or page teammates:\n%s", got)
	}
	chief := renderSystemPrompt(
		promptBot{Name: "General", Cwd: "/tmp", Engine: engineGrok, Chief: true},
		"host", nil,
	)
	if !strings.Contains(chief, "spawn_session") || !strings.Contains(chief, "tell_session") {
		t.Errorf("grok chief prompt should describe dispatcher tools:\n%s", chief)
	}
	if strings.Contains(chief, "report_to_general") {
		t.Error("chief must not get report_to_general")
	}
	agy := renderSystemPrompt(promptBot{Name: "coder", Cwd: "/tmp", Engine: engineAntigravity}, "host", nil)
	if !strings.Contains(agy, "do NOT have the ccc MCP") || !strings.Contains(agy, "ccc routine") {
		t.Errorf("agy should still skip MCP and teach ccc routine:\n%s", agy)
	}
	claude := renderSystemPrompt(promptBot{Name: "coder", Cwd: "/tmp"}, "host", nil)
	if !strings.Contains(claude, "backend session") || !strings.Contains(claude, "remember/recall/forget") {
		t.Error("Claude prompt should describe a session and the ccc tools")
	}
}

func plantClaudeCreds(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"claudeAiOauth":{"accessToken":"test-access-token"}}`)
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func plantEngineAuth(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{"email":"a@b.c"}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func prependFakeBin(t *testing.T, name string) {
	t.Helper()
	dir := t.TempDir()
	writeFakeCLI(t, dir, name, "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
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

func TestCreateBotPicksEngineWithMostHeadroom(t *testing.T) {
	in, _, _ := testInstance(t)
	usageMemClear()
	t.Cleanup(usageMemClear)
	dir := t.TempDir()
	in.cfg.DefaultEngine = engineGrok
	in.cfg.DefaultProfile = "work"
	grokDir := filepath.Join(dir, "grok")
	claudeDir := filepath.Join(dir, "claude")
	plantEngineAuth(t, grokDir)
	plantClaudeCreds(t, claudeDir)
	prependFakeBin(t, "grok")
	in.cfg.Profiles = map[string]*Profile{
		"work":     {Engine: engineGrok, ConfigDir: grokDir},
		"personal": {Engine: engineClaude, ConfigDir: claudeDir},
	}
	for _, p := range listProfiles(in.cfg) {
		switch profileEngine(p) {
		case engineGrok:
			usageMemPut(p, profileUsage{FiveHour: 63, FiveHourKnown: true, SevenDay: 40, SevenDayKnown: true})
		case engineClaude:
			usageMemPut(p, profileUsage{FiveHour: 10, FiveHourKnown: true, SevenDay: 32, SevenDayKnown: true})
		}
	}
	b, err := in.createBot("from-headroom", "")
	if err != nil {
		t.Fatal(err)
	}
	if botEngine(b) != engineClaude {
		t.Errorf("engine = %q, want claude (10%% 5h vs grok 63%%)", b.Engine)
	}
}

func TestCreateBotUnknownUsageLosesToKnown(t *testing.T) {
	in, _, _ := testInstance(t)
	usageMemClear()
	t.Cleanup(usageMemClear)
	dir := t.TempDir()
	in.cfg.DefaultEngine = ""
	in.cfg.DefaultProfile = "you@example.com"
	claudeDir := filepath.Join(dir, "claude")
	codexDir := filepath.Join(dir, "codex")
	plantClaudeCreds(t, claudeDir)
	plantEngineAuth(t, codexDir)
	prependFakeBin(t, "codex")
	in.cfg.Profiles = map[string]*Profile{
		"you@example.com": {Engine: engineClaude, ConfigDir: claudeDir},
		"codex":           {Engine: engineCodex, ConfigDir: codexDir},
	}
	for _, p := range listProfiles(in.cfg) {
		if profileEngine(p) == engineClaude {
			usageMemPut(p, profileUsage{FiveHour: 42, FiveHourKnown: true, SevenDay: 42, SevenDayKnown: true})
		}
	}
	b, err := in.createBot("from-unused-codex", "")
	if err != nil {
		t.Fatal(err)
	}
	if botEngine(b) != engineClaude {
		t.Errorf("engine = %q, want claude (unknown usage must not beat a known 42%%)", b.Engine)
	}
}

func TestNextEngineByHeadroomCrossesEngines(t *testing.T) {
	usageMemClear()
	t.Cleanup(usageMemClear)
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, "claude")
	codexDir := filepath.Join(dir, "codex")
	plantClaudeCreds(t, claudeDir)
	plantEngineAuth(t, codexDir)
	prependFakeBin(t, "codex")
	cfg := &Config{
		Profiles: map[string]*Profile{
			"you@example.com": {Engine: engineClaude, ConfigDir: claudeDir},
			"codex":           {Engine: engineCodex, ConfigDir: codexDir},
		},
	}
	for _, p := range listProfiles(cfg) {
		switch profileEngine(p) {
		case engineClaude:
			usageMemPut(p, profileUsage{FiveHour: 90, FiveHourKnown: true})
		case engineCodex:
			usageMemPut(p, profileUsage{FiveHour: 0, FiveHourKnown: true})
		}
	}
	got, ok := nextEngineByHeadroom(cfg, nil, map[string]bool{engineClaude: true})
	if !ok || got != engineCodex {
		t.Fatalf("failover after claude = %q ok=%v, want codex", got, ok)
	}
	if _, ok := nextEngineByHeadroom(cfg, nil, map[string]bool{engineClaude: true, engineCodex: true}); ok {
		t.Fatal("no engines left must not invent a pool")
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
