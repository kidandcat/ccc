package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// engineHasMCP reports whether ccc injects its stdio MCP server into turns of
// this engine. Claude gets --mcp-config; Grok and Codex get a config.toml
// entry in the isolated account home (plus -c on Codex exec). Antigravity
// still has no per-turn MCP attach that would not race the user's ~/.gemini.
func engineHasMCP(engine string) bool {
	switch engine {
	case engineClaude, engineGrok, engineCodex:
		return true
	default:
		return false
	}
}

// ensureAccountMCP writes [mcp_servers.ccc] into the isolated engine home so
// Grok/Codex load `ccc mcp` on every turn. Identity is CCC_BOT_ID / CCC_TURN_ID
// in the child environment (appendTurnIdentity), not per-turn argv — a shared
// GROK_HOME must not be rewritten with the last turn's bot id.
func ensureAccountMCP(p Profile, engine string) {
	if !engineHasMCP(engine) || engine == engineClaude {
		return
	}
	if p.Implicit || strings.TrimSpace(p.ConfigDir) == "" {
		return
	}
	home := engineHome(p)
	if home == "" {
		return
	}
	path := filepath.Join(home, "config.toml")
	if err := upsertCCCMCPBlock(path, cccPath); err != nil {
		hookLog("mcp config %s: %v", path, err)
	}
}

func upsertCCCMCPBlock(path, command string) error {
	if command == "" {
		command = "ccc"
	}
	block := fmt.Sprintf("[mcp_servers.ccc]\ncommand = %s\nargs = [\"mcp\"]\nenabled = true\n", tomlQuote(command))
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	raw := stripTOMLTable(existing, "mcp_servers.ccc")
	raw = bytes.TrimSpace(raw)
	var buf bytes.Buffer
	if len(raw) > 0 {
		buf.Write(raw)
		buf.WriteString("\n\n")
	}
	buf.WriteString(block)
	next := buf.Bytes()
	if bytes.Equal(bytes.TrimSpace(existing), bytes.TrimSpace(next)) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// WriteFile truncates in place. Two sessions launching on the same
	// account would let one CLI read a half-written config.toml.
	return writeFileAtomic(path, next, 0o600)
}

// stripTOMLTable drops [table] and [table.*] child tables.
func stripTOMLTable(raw []byte, table string) []byte {
	lines := bytes.Split(raw, []byte("\n"))
	out := make([][]byte, 0, len(lines))
	skipping := false
	for _, line := range lines {
		trim := bytes.TrimSpace(line)
		if len(trim) > 0 && trim[0] == '[' && trim[len(trim)-1] == ']' {
			inner := string(trim[1 : len(trim)-1])
			if inner == table || strings.HasPrefix(inner, table+".") {
				skipping = true
				continue
			}
			skipping = false
		}
		if skipping {
			continue
		}
		out = append(out, line)
	}
	return bytes.Join(out, []byte("\n"))
}

func tomlQuote(s string) string {
	return strconv.Quote(s)
}

// insertCodexMCPArgs puts per-turn -c overrides after `exec` so this bot's
// --bot/--turn reach Codex even if config.toml is shared across sessions.
func insertCodexMCPArgs(args []string, botID, turnID int64) []string {
	extra := []string{
		"-c", fmt.Sprintf("mcp_servers.ccc.command=%s", tomlQuote(cccPath)),
		"-c", fmt.Sprintf(`mcp_servers.ccc.args=["mcp","--bot","%d","--turn","%d"]`, botID, turnID),
	}
	out := make([]string, 0, len(args)+len(extra))
	inserted := false
	for _, a := range args {
		out = append(out, a)
		if !inserted && a == "exec" {
			out = append(out, extra...)
			inserted = true
		}
	}
	if !inserted {
		return append(extra, args...)
	}
	return out
}
