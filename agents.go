package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// This file implements the "background agent" backend that replaces the old
// tmux-based session model. Sessions are now real Claude Code background agents
// managed by the local `claude` daemon, so they show up in `claude agents`
// (the fleet view). ccc becomes a Telegram mirror of that fleet.
//
// Primitives:
//   - dispatchAgent: `claude --bg ...`      → start a new bg session, return its id
//   - resumeAgent:   `claude --resume ...`   → send a follow-up message (stop+resume)
//   - stopAgent:     `claude stop <id>`      → stop a bg session (keeps conversation)
//   - listAgents:    `claude agents --json`  → snapshot of the fleet + state
//   - attach is done directly from the terminal with `claude attach <id>`

// AgentInfo mirrors one entry of `claude agents --json --all`.
type AgentInfo struct {
	PID       int    `json:"pid"`
	ID        string `json:"id"`        // short daemon id (changes on every resume)
	Cwd       string `json:"cwd"`
	Kind      string `json:"kind"`      // "background"
	StartedAt int64  `json:"startedAt"` // unix millis
	SessionID string `json:"sessionId"` // full conversation UUID
	Name      string `json:"name"`      // display name (-n/--name)
	Status    string `json:"status"`    // "idle" | "busy"
	State     string `json:"state"`     // "working" | "done" | ... | ""
}

// jobState mirrors ~/.claude/jobs/<shortID>/state.json
type jobState struct {
	State        string `json:"state"`
	Detail       string `json:"detail"`
	Tempo        string `json:"tempo"`
	SessionID    string `json:"sessionId"`
	LinkScanPath string `json:"linkScanPath"`
	Intent       string `json:"intent"`
	Tokens       int    `json:"tokens"`
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)
var attachRe = regexp.MustCompile(`attach\s+([0-9a-f]{6,})`)

func claudeBin() string {
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	// common install location
	cand := filepath.Join(home, ".local", "bin", "claude")
	if _, err := os.Stat(cand); err == nil {
		return cand
	}
	return "claude"
}

// listAgents returns the current fleet snapshot. includeDone controls --all.
func listAgents(includeDone bool) ([]AgentInfo, error) {
	args := []string{"agents", "--json"}
	if includeDone {
		args = append(args, "--all")
	}
	cmd := exec.Command(claudeBin(), args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("claude agents --json failed: %w", err)
	}
	var agents []AgentInfo
	if err := json.Unmarshal(out, &agents); err != nil {
		return nil, fmt.Errorf("cannot parse agents json: %w", err)
	}
	return agents, nil
}

// agentByID finds an agent by its short daemon id.
func agentByID(agents []AgentInfo, shortID string) (*AgentInfo, bool) {
	for i := range agents {
		if agents[i].ID == shortID {
			return &agents[i], true
		}
	}
	return nil, false
}

// agentBySessionID finds an agent by its full conversation UUID.
func agentBySessionID(agents []AgentInfo, sessionID string) (*AgentInfo, bool) {
	if sessionID == "" {
		return nil, false
	}
	for i := range agents {
		if agents[i].SessionID == sessionID {
			return &agents[i], true
		}
	}
	return nil, false
}

// parseBgShortID extracts the short id printed by `claude --bg`.
// Banner looks like: "backgrounded · <id> · <name>\n  claude attach <id> ...".
func parseBgShortID(out string) string {
	clean := ansiRe.ReplaceAllString(out, "")
	if m := attachRe.FindStringSubmatch(clean); len(m) == 2 {
		return m[1]
	}
	// Fallback: "backgrounded · <id> · name"
	for _, line := range strings.Split(clean, "\n") {
		if strings.Contains(line, "backgrounded") {
			parts := strings.Split(line, "·")
			if len(parts) >= 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
}

// ccSettingsJSON builds the inline --settings payload that installs a
// PreToolUse hook for AskUserQuestion, scoped to ccc's agents only (so we never
// touch the user's global ~/.claude/settings.json). When Claude is about to ask
// a question, the hook (`ccc hook-question`) renders the options as Telegram
// buttons and blocks until the user taps one — see hooks.go.
func ccSettingsJSON() string {
	cmd := cccPath + " hook-question"
	b, _ := json.Marshal(map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []any{
				map[string]any{
					"matcher": "AskUserQuestion",
					"hooks": []any{
						map[string]any{"type": "command", "command": cmd},
					},
				},
			},
		},
	})
	return string(b)
}

// dispatchArgs builds the argv for a `claude --bg` invocation.
func dispatchArgs(name, resumeID, prompt string) []string {
	args := []string{"--bg", "--dangerously-skip-permissions", "--settings", ccSettingsJSON()}
	if name != "" {
		args = append(args, "--name", name)
	}
	if resumeID != "" {
		args = append(args, "--resume", resumeID)
	}
	args = append(args, prompt)
	return args
}

// dispatchAgent starts a new background Claude session in workDir with an
// initial prompt. Returns the short daemon id.
func dispatchAgent(name, workDir, prompt string) (string, error) {
	return runDispatch(name, workDir, "", prompt)
}

// resumeAgent sends a follow-up message to an existing conversation. Because
// background agents cannot be injected live, this stops the current bg job (a
// still-resident idle agent still counts as "running") and relaunches with
// --resume carrying the new user message. Returns the NEW short daemon id.
//
// stopShortID is the current live bg job to stop (empty if already settled).
// resumeID MUST be the stable conversation UUID, not the short id: after the bg
// job is stopped, `--resume <shortId>` fails with "source session not found",
// whereas `--resume <uuid>` reloads the conversation from disk.
//
// It waits for the previous session to fully stop before resuming: dispatching
// --resume while the old worker is still alive makes claude report "<id> is
// currently running as a background agent" and the worker crashes/respawns.
func resumeAgent(stopShortID, resumeID, name, workDir, prompt string) (string, error) {
	if stopShortID != "" {
		stopAgent(stopShortID)
		waitAgentStopped(stopShortID, 8*time.Second)
	}
	return runDispatch(name, workDir, resumeID, prompt)
}

// waitAgentStopped blocks until the given short id is no longer actively
// running (gone from the fleet, or settled to a terminal state), so a
// subsequent --resume does not race the still-alive worker.
func waitAgentStopped(shortID string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for {
		agents, err := listAgents(true)
		if err == nil {
			a, ok := agentByID(agents, shortID)
			if !ok {
				return // gone entirely
			}
			st := strings.ToLower(a.State)
			if a.Status != "busy" && st != "working" {
				return // settled (done/failed/idle)
			}
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func runDispatch(name, workDir, resumeID, prompt string) (string, error) {
	cmd := exec.Command(claudeBin(), dispatchArgs(name, resumeID, prompt)...)
	if workDir != "" {
		cmd.Dir = workDir
	}
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("claude --bg failed: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	shortID := parseBgShortID(string(out))
	if shortID == "" {
		return "", fmt.Errorf("could not parse background session id from: %s", strings.TrimSpace(ansiRe.ReplaceAllString(string(out), "")))
	}
	return shortID, nil
}

// stopAgent stops a background session (its conversation is kept).
func stopAgent(shortID string) error {
	if shortID == "" {
		return nil
	}
	return exec.Command(claudeBin(), "stop", shortID).Run()
}

// resolveSessionUUID resolves the full conversation UUID for a short id by
// polling the fleet snapshot (the job state file is not populated immediately).
func resolveSessionUUID(shortID string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for {
		if agents, err := listAgents(true); err == nil {
			if a, ok := agentByID(agents, shortID); ok && a.SessionID != "" {
				return a.SessionID
			}
		}
		if time.Now().After(deadline) {
			return ""
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// readJobState reads ~/.claude/jobs/<shortID>/state.json (best effort).
func readJobState(shortID string) *jobState {
	if shortID == "" {
		return nil
	}
	home, _ := os.UserHomeDir()
	data, err := os.ReadFile(filepath.Join(home, ".claude", "jobs", shortID, "state.json"))
	if err != nil {
		return nil
	}
	var js jobState
	if json.Unmarshal(data, &js) != nil {
		return nil
	}
	// Derive sessionId from linkScanPath if the explicit field is empty.
	if js.SessionID == "" && js.LinkScanPath != "" {
		base := filepath.Base(js.LinkScanPath)
		js.SessionID = strings.TrimSuffix(base, ".jsonl")
	}
	return &js
}

// transcriptPathForUUID locates the JSONL transcript for a conversation UUID.
func transcriptPathForUUID(uuid string) string {
	if uuid == "" {
		return ""
	}
	home, _ := os.UserHomeDir()
	matches, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", uuid+".jsonl"))
	if len(matches) > 0 {
		return matches[0]
	}
	return ""
}

var cccMarkerRe = regexp.MustCompile(`ccc-session:t(\d+)`)

// transcriptTopicMarker scans a conversation transcript for the ccc marker
// (see cccMarker) and returns the Telegram TopicID it encodes, or 0 if none.
// The marker survives resume because it lives in the conversation's message
// history, so this re-links a resumed agent (new UUID/short) to its session.
func transcriptTopicMarker(transcriptPath string) int64 {
	if transcriptPath == "" {
		return 0
	}
	data, err := os.ReadFile(transcriptPath)
	if err != nil {
		return 0
	}
	// Use the LAST marker: if a topic was deleted and the conversation later
	// re-tagged with a new topic id, the most recent one is authoritative.
	ms := cccMarkerRe.FindAllSubmatch(data, -1)
	if len(ms) == 0 {
		return 0
	}
	var id int64
	fmt.Sscanf(string(ms[len(ms)-1][1]), "%d", &id)
	return id
}

// classifyState maps daemon state/status fields to a coarse ccc status.
// Returns one of: "working", "needs_input", "done", "failed".
func classifyState(state, status string) string {
	l := strings.ToLower(state)
	switch l {
	case "working":
		return "working"
	case "done":
		return "done"
	case "failed", "error":
		return "failed"
	case "":
		if status == "idle" {
			return "done"
		}
		return "working"
	}
	if strings.Contains(l, "input") || strings.Contains(l, "block") ||
		strings.Contains(l, "wait") || strings.Contains(l, "question") {
		return "needs_input"
	}
	return "working"
}
