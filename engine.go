package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Engines a bot can run on. Claude Code is the default and the only engine
// that gets the ccc MCP server and the Claude account pool. Grok Build and
// Antigravity are first-class alternatives: same Telegram topic, same
// envelope, their own CLI and session flags.
const (
	engineClaude      = "claude"
	engineGrok        = "grok"
	engineAntigravity = "antigravity"
)

// streamKind selects how spawn consumes stdout. Claude and Grok share the
// Anthropic-shaped stream-json / streaming-messages-json protocol
// (consumeClaudeEvent). Antigravity emits its own NDJSON (consumeAgyEvent).
// streamText is the fallback when a CLI prints only the final answer.
type streamKind string

const (
	streamClaudeMessages streamKind = "claude-messages"
	streamAgy            streamKind = "agy"
	streamText           streamKind = "text"
)

// parseEngine canonicalises an engine name from Telegram, config, or a bot
// row. Empty means Claude so existing bots and an unset default_engine stay
// backward compatible. Aliases: grok-build → grok, agy → antigravity.
func parseEngine(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", engineClaude:
		return engineClaude, nil
	case engineGrok, "grok-build":
		return engineGrok, nil
	case engineAntigravity, "agy":
		return engineAntigravity, nil
	default:
		return "", fmt.Errorf("unknown engine %q (use claude, grok or antigravity)", s)
	}
}

// botEngine is the engine a bot will spawn. An empty or unknown row value
// is Claude: AutoMigrate backfills new columns, but a hand-edited database
// must not start failing turns.
func botEngine(b *Bot) string {
	if b == nil {
		return engineClaude
	}
	e, err := parseEngine(b.Engine)
	if err != nil {
		return engineClaude
	}
	return e
}

// defaultEngine is the engine assigned to a newly created bot (General text
// or /bot). Spawned children inherit the parent instead (createBotRow).
func defaultEngine(cfg *Config) string {
	if cfg == nil {
		return engineClaude
	}
	e, err := parseEngine(cfg.DefaultEngine)
	if err != nil {
		return engineClaude
	}
	return e
}

func engineLabel(engine string) string {
	e, err := parseEngine(engine)
	if err != nil {
		e = engineClaude
	}
	switch e {
	case engineGrok:
		return "Grok Build"
	case engineAntigravity:
		return "Antigravity"
	default:
		return "Claude Code"
	}
}

func engineLoginHint(engine string) string {
	switch engine {
	case engineGrok:
		return "install Grok Build (~/.grok/bin/grok) and run `grok login` (`grok login --device-auth` on a VM)"
	case engineAntigravity:
		return "install Antigravity CLI (~/.local/bin/agy) and authenticate with an interactive `agy` session first"
	default:
		return "use /account to add a Claude login"
	}
}

// ---------------------------------------------------------------------------
// Binaries
// ---------------------------------------------------------------------------

// grokBin is the Grok Build CLI. LookPath first, then the documented install
// path (~/.grok/bin/grok). The binary was not on the agent VM; the path is
// the one Hairok verified on macOS.
func grokBin() string {
	return lookPathOrHome("grok", filepath.Join(".grok", "bin", "grok"))
}

// agyBin is the Antigravity CLI. LookPath first, then ~/.local/bin/agy.
func agyBin() string {
	return lookPathOrHome("agy", filepath.Join(".local", "bin", "agy"))
}

func lookPathOrHome(name, homeRel string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	home, err := os.UserHomeDir()
	if err == nil && homeRel != "" {
		cand := filepath.Join(home, homeRel)
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return name
}

// engineBin is the executable spawn will exec for this engine. It is a
// LookPath result (or the bare name), not a guarantee the file exists —
// resolveEngineBin is what turns a missing CLI into a turn error.
func engineBin(engine string) string {
	switch engine {
	case engineGrok:
		return grokBin()
	case engineAntigravity:
		return agyBin()
	default:
		return claudeBin()
	}
}

// resolveEngineBin returns the binary to exec, or a login/install hint when
// it is not on PATH and not in the documented home path. Claude keeps the
// historical behaviour of returning the bare name and letting exec fail:
// doctor already reports a missing claude, and existing tests rely on that.
func resolveEngineBin(engine string) (string, error) {
	bin := engineBin(engine)
	if engine == engineClaude {
		return bin, nil
	}
	if _, err := os.Stat(bin); err == nil {
		return bin, nil
	}
	if p, err := exec.LookPath(bin); err == nil {
		return p, nil
	}
	switch engine {
	case engineGrok:
		return "", fmt.Errorf("grok CLI not found (%s)", engineLoginHint(engineGrok))
	case engineAntigravity:
		return "", fmt.Errorf("agy CLI not found (%s)", engineLoginHint(engineAntigravity))
	default:
		return "", fmt.Errorf("%s CLI not found", engine)
	}
}

// ---------------------------------------------------------------------------
// Arg builders
// ---------------------------------------------------------------------------

// grokTurnArgs is the headless flag set for Grok Build (`grok`).
//
// The binary was not on the agent VM. Flags below are the Mac-verified set
// Hairok supplied, cross-checked against the published Grok Build headless
// docs (https://docs.x.ai/build/cli/reference and the headless-mode guide).
//
//	--always-approve            unattended tools (alias --yolo / bypassPermissions).
//	--output-format streaming-messages-json
//	                            Anthropic Messages stream-json: system/init,
//	                            assistant, user, result. Maps onto consumeClaudeEvent.
//	                            (Native `streaming-json` is xAI-shaped and would
//	                            need a second parser.)
//	--system-prompt-override    replaces Grok's own prompt. `--system-prompt` is
//	                            documented as a Claude-compatible alias; the
//	                            native name is used so a rename of the alias
//	                            cannot silently drop isolation.
//	--model                     instance model; omitted to accept grok's default.
//	--session-id <uuid>         first turn; ccc mints the UUID (must be unused).
//	--resume <uuid>             later turns. `--session-id` does NOT resume.
//
// The prompt is NOT in this slice: `--single` takes it as its argument, so
// buildTurn appends `--single`, envelope after these flags.
//
// MCP is deliberately omitted. Grok has `grok mcp add|list|remove` and stores
// servers in ~/.grok/config.toml (or project .grok/config.toml) — there is no
// clean per-turn inline --mcp-config equivalent. A half-broken bridge would
// be worse than none; ccc MCP stays Claude-only.
func grokTurnArgs(model, systemPrompt, sessionID string, resume bool) []string {
	args := []string{
		"--always-approve",
		"--output-format", "streaming-messages-json",
	}
	if systemPrompt != "" {
		args = append(args, "--system-prompt-override", systemPrompt)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if resume {
		args = append(args, "--resume", sessionID)
	} else if sessionID != "" {
		args = append(args, "--session-id", sessionID)
	}
	return args
}

// agyTurnArgs is the headless flag set for Antigravity (`agy`).
//
// The binary was not on the agent VM. Flags below are the Mac-verified set
// Hairok supplied, cross-checked against the official headless docs
// (https://www.antigravity.google/docs/cli/headless/).
//
//	--output-format stream-json  NDJSON: init, step_update, result. Parsed by
//	                            consumeAgyEvent (this is NOT Claude stream-json).
//	--dangerously-skip-permissions
//	                            unattended tools (headless has no prompt).
//	--print-timeout 120m        official default is 5m, which kills a coding
//	                            turn. This is a ccc choice, not a Mac-verified
//	                            flag; drop it if an older agy rejects it.
//	--model                     instance model; omitted to accept agy's default.
//	                            agy exits non-zero on an unknown slug.
//	--conversation <id>         resume. First turn: omit, then persist the
//	                            conversation_id from init/result.
//
// The prompt is NOT in this slice: `--print` takes it as its argument, so
// buildTurn appends `--print`, prompt. agy has no system-prompt flag; the
// system prompt is prepended to that prompt (buildTurn).
//
// MCP is deliberately omitted. agy reads ~/.gemini/config/mcp_config.json (or
// a workspace file) — a global/workspace file, not a per-turn inline config.
// Writing that file from ccc would race every other agy use on the machine.
func agyTurnArgs(model, conversationID string, resume bool) []string {
	args := []string{
		"--output-format", "stream-json",
		"--dangerously-skip-permissions",
		"--print-timeout", "120m",
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if resume && conversationID != "" {
		args = append(args, "--conversation", conversationID)
	}
	return args
}

// turnSpec is everything spawn needs to exec one engine.
type turnSpec struct {
	Engine string
	Bin    string
	Args   []string
	Env    []string
	Stream streamKind
}

// buildTurn picks the binary, flags, env and stream parser for one turn.
// Claude's arg list is the existing claudeTurnArgs (unchanged). The prompt
// is always the last value, carried by each CLI's print/single flag.
func buildTurn(engine string, p Profile, cfg *Config, mcpCfg, sessionID, sysPrompt, envelope string, resume bool) (turnSpec, error) {
	engine, err := parseEngine(engine)
	if err != nil {
		return turnSpec{}, err
	}
	bin, err := resolveEngineBin(engine)
	if err != nil {
		return turnSpec{}, err
	}
	model := instanceModel(cfg)
	spec := turnSpec{Engine: engine, Bin: bin, Env: engineEnv(cfg, engine, p)}
	switch engine {
	case engineGrok:
		spec.Stream = streamClaudeMessages
		spec.Args = append(grokTurnArgs(model, sysPrompt, sessionID, resume), "--single", envelope)
	case engineAntigravity:
		spec.Stream = streamAgy
		// No --system-prompt on agy (verified against the official flag
		// table). The session identity still has to reach the model, so the
		// system prompt is prepended to the user prompt.
		prompt := envelope
		if strings.TrimSpace(sysPrompt) != "" {
			prompt = strings.TrimSpace(sysPrompt) + "\n\n" + envelope
		}
		spec.Args = append(agyTurnArgs(model, sessionID, resume), "--print", prompt)
	default:
		spec.Stream = streamClaudeMessages
		spec.Args = append(claudeTurnArgs(model, sysPrompt, mcpCfg, sessionID, resume), envelope)
	}
	return spec, nil
}

// ---------------------------------------------------------------------------
// Environment
// ---------------------------------------------------------------------------

// engineEnv is the child environment for a turn. Claude keeps botEnv
// (claudeEnv + env_passthrough, never a parent CLAUDE*/ANTHROPIC*). Grok and
// Antigravity get the same whitelist and passthrough, plus the prefixes those
// CLIs actually read for auth — and never a fabricated CLAUDE_CONFIG_DIR.
func engineEnv(cfg *Config, engine string, p Profile) []string {
	if engine == engineClaude {
		return botEnv(cfg, p)
	}
	env := baseScrubbedEnv()
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		name := kv[:eq]
		if engineAuthPrefix(name) {
			env = append(env, kv)
		}
	}
	if cfg == nil {
		return env
	}
	for _, name := range cfg.EnvPassthrough {
		name = strings.TrimSpace(name)
		if name == "" || strings.HasPrefix(name, "CLAUDE") || strings.HasPrefix(name, "ANTHROPIC") {
			continue
		}
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}

func engineAuthPrefix(name string) bool {
	for _, p := range []string{"XAI_", "GROK_", "GEMINI_", "GOOGLE_", "AGY_", "ANTIGRAVITY_"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// baseScrubbedEnv is the PATH/HOME/… whitelist claudeEnv uses, without
// CLAUDE_CONFIG_DIR. Shared so grok/agy see the same machine, not a fake
// Claude profile directory.
func baseScrubbedEnv() []string {
	keep := map[string]bool{}
	for _, k := range envWhitelist {
		keep[k] = true
	}
	var env []string
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		name := kv[:eq]
		if keep[name] {
			env = append(env, kv)
			continue
		}
		for _, prefix := range envWhitelistPrefixes {
			if strings.HasPrefix(name, prefix) {
				env = append(env, kv)
				break
			}
		}
	}
	return env
}

// ---------------------------------------------------------------------------
// Antigravity stream-json
// ---------------------------------------------------------------------------

// agyStreamEvent is the subset of agy's stream-json protocol ccc reads.
// Shape verified against https://www.antigravity.google/docs/cli/headless/
// (the binary was not on the agent VM).
//
//	{"event":"init","conversation_id":"…","init":{…}}
//	{"event":"step_update","step_update":{"step_type":"tool","tool_name":"…","tool_info":{…}}}
//	{"event":"result","result":{"conversation_id":"…","status":"SUCCESS","response":"…","usage":{…}}}
type agyStreamEvent struct {
	Event          string `json:"event"`
	ConversationID string `json:"conversation_id"`
	StepUpdate     struct {
		ConversationID string `json:"conversation_id"`
		StepType       string `json:"step_type"`
		State          string `json:"state"`
		ToolName       string `json:"tool_name"`
		TextDelta      string `json:"text_delta"`
		ToolInfo       struct {
			Name       string          `json:"name"`
			Parameters json.RawMessage `json:"parameters"`
		} `json:"tool_info"`
	} `json:"step_update"`
	Result struct {
		ConversationID string          `json:"conversation_id"`
		Status         string          `json:"status"`
		Response       string          `json:"response"`
		Error          string          `json:"error"`
		Usage          json.RawMessage `json:"usage"`
	} `json:"result"`
}

// consumeAgyEvent folds one NDJSON line into the turn result and the
// progress line. Returns true when the line was JSON with an event ccc
// understands, so spawn can tell a real stream from plain-text stdout.
func consumeAgyEvent(line []byte, res *streamResult, prog *progress) bool {
	var ev agyStreamEvent
	if err := json.Unmarshal(line, &ev); err != nil || ev.Event == "" {
		return false
	}
	if ev.ConversationID != "" {
		res.SessionID = ev.ConversationID
	}
	switch ev.Event {
	case "init":
		// conversation_id is already captured above.
	case "step_update":
		if ev.StepUpdate.ConversationID != "" {
			res.SessionID = ev.StepUpdate.ConversationID
		}
		switch ev.StepUpdate.StepType {
		case "tool":
			name := ev.StepUpdate.ToolName
			if ev.StepUpdate.ToolInfo.Name != "" {
				name = ev.StepUpdate.ToolInfo.Name
			}
			if name != "" {
				prog.set(summarizeTool(name, ev.StepUpdate.ToolInfo.Parameters))
			} else {
				prog.set("running a tool")
			}
		case "agent_response":
			if strings.TrimSpace(ev.StepUpdate.TextDelta) != "" {
				prog.set("writing a reply")
			}
		}
	case "result":
		if ev.Result.ConversationID != "" {
			res.SessionID = ev.Result.ConversationID
		}
		res.Subtype = ev.Result.Status
		res.UsageJSON = mergeUsage(ev.Result.Usage, 0)
		if strings.EqualFold(ev.Result.Status, "SUCCESS") {
			res.Text = ev.Result.Response
			res.IsError = false
		} else {
			res.IsError = true
			if ev.Result.Error != "" {
				res.Text = ev.Result.Error
			} else {
				res.Text = ev.Result.Response
			}
		}
	}
	return true
}
