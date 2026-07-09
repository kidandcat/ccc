package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestStripTitlePathPrefix verifies only path-shaped "<dir>: " prefixes are
// stripped from a topic title, and plain "word: rest" titles pass through.
func TestStripTitlePathPrefix(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"~/pcb: pcb board compaction", "pcb board compaction"},
		{"~: mándame imagen gateway", "mándame imagen gateway"},
		{".../a/b/c: x", "x"},
		{"/abs/path: y", "y"},
		{"fix: swipe decoder", "fix: swipe decoder"},
		{"plain", "plain"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := stripTitlePathPrefix(tt.in); got != tt.want {
			t.Errorf("stripTitlePathPrefix(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// writeSessionName writes a label file under $HOME/.claude/session-names.
func writeSessionName(t *testing.T, name, content string) {
	t.Helper()
	dir := sessionNamesDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir session-names: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestSessionLabel checks the manual/autolabel precedence and whitespace collapse.
func TestSessionLabel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if got := sessionLabel(""); got != "" {
		t.Errorf("sessionLabel(empty) = %q, want empty", got)
	}
	if got := sessionLabel("missing-uuid"); got != "" {
		t.Errorf("sessionLabel(missing) = %q, want empty", got)
	}

	// autolabel used when the manual file is absent.
	writeSessionName(t, "uuid-a.autolabel", "  heuristic  label \n")
	if got := sessionLabel("uuid-a"); got != "heuristic label" {
		t.Errorf("sessionLabel(autolabel) = %q, want %q", got, "heuristic label")
	}

	// manual label wins over autolabel.
	writeSessionName(t, "uuid-a", "manual\tlabel")
	if got := sessionLabel("uuid-a"); got != "manual label" {
		t.Errorf("sessionLabel(manual) = %q, want %q", got, "manual label")
	}

	// blank manual falls back to autolabel.
	writeSessionName(t, "uuid-b", "   \n")
	writeSessionName(t, "uuid-b.autolabel", "fallback")
	if got := sessionLabel("uuid-b"); got != "fallback" {
		t.Errorf("sessionLabel(blank manual) = %q, want %q", got, "fallback")
	}
}

// TestCarrySessionLabel checks labels are copied to a new UUID across a resume.
func TestCarrySessionLabel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := sessionNamesDir()

	writeSessionName(t, "old", "my label")
	writeSessionName(t, "old.autolabel", "auto")
	writeSessionName(t, "old.cwd", "/Users/x/proj")

	// No-op cases: none should create a "new" file.
	carrySessionLabel("", "new")
	carrySessionLabel("old", "")
	carrySessionLabel("old", "old")
	if _, err := os.Stat(filepath.Join(dir, "new")); err == nil {
		t.Error("no-op carrySessionLabel created a file")
	}

	carrySessionLabel("old", "new")
	for suffix, want := range map[string]string{"": "my label", ".autolabel": "auto", ".cwd": "/Users/x/proj"} {
		data, err := os.ReadFile(filepath.Join(dir, "new"+suffix))
		if err != nil {
			t.Errorf("new%s not created: %v", suffix, err)
			continue
		}
		if string(data) != want {
			t.Errorf("new%s = %q, want %q", suffix, string(data), want)
		}
	}

	// A blank source file is not carried.
	writeSessionName(t, "blank.cwd", "   ")
	carrySessionLabel("blank", "target")
	if _, err := os.Stat(filepath.Join(dir, "target.cwd")); err == nil {
		t.Error("blank source should not be carried")
	}
}

// TestTopicTitleFor checks title precedence: session label, then stripped fleet
// name, then cwd base, then short id.
func TestTopicTitleFor(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Session label wins (and survives the "<dir>: " garbage in Name).
	writeSessionName(t, "uuid-1", "pcb board compaction")
	a := &AgentInfo{SessionID: "uuid-1", Name: "~: está perfecto", Cwd: "/Users/x/pcb", ID: "abcd1234"}
	if got := topicTitleFor(a); got != "pcb board compaction" {
		t.Errorf("topicTitleFor(label) = %q, want %q", got, "pcb board compaction")
	}

	// No label → stripped fleet name.
	b := &AgentInfo{SessionID: "uuid-2", Name: "~/pcb: fix decoder", Cwd: "/Users/x/pcb", ID: "abcd1234"}
	if got := topicTitleFor(b); got != "fix decoder" {
		t.Errorf("topicTitleFor(stripped name) = %q, want %q", got, "fix decoder")
	}

	// No label, empty name → cwd base.
	c := &AgentInfo{SessionID: "uuid-3", Name: "", Cwd: "/Users/x/proj", ID: "abcd1234"}
	if got := topicTitleFor(c); got != "proj" {
		t.Errorf("topicTitleFor(cwd base) = %q, want %q", got, "proj")
	}

	// No label, empty name, empty cwd → short id.
	d := &AgentInfo{SessionID: "uuid-4", Name: "", Cwd: "", ID: "abcd1234"}
	if got := topicTitleFor(d); got != "abcd1234" {
		t.Errorf("topicTitleFor(id) = %q, want %q", got, "abcd1234")
	}
}

// TestAppendOldSessionID checks dedup and the most-recent-20 cap.
func TestAppendOldSessionID(t *testing.T) {
	info := &SessionInfo{}

	appendOldSessionID(info, "") // empty is a no-op
	if len(info.OldSessionIDs) != 0 {
		t.Fatalf("empty uuid appended: %v", info.OldSessionIDs)
	}

	appendOldSessionID(info, "a")
	appendOldSessionID(info, "a") // dedup
	appendOldSessionID(info, "b")
	if len(info.OldSessionIDs) != 2 || info.OldSessionIDs[0] != "a" || info.OldSessionIDs[1] != "b" {
		t.Fatalf("dedup failed: %v", info.OldSessionIDs)
	}

	// Cap at 20, keeping the most recent.
	info = &SessionInfo{}
	for i := 0; i < 30; i++ {
		appendOldSessionID(info, string(rune('A'+i%26))+string(rune('0'+i/26)))
	}
	if len(info.OldSessionIDs) != 20 {
		t.Fatalf("cap failed: len = %d, want 20", len(info.OldSessionIDs))
	}
	// The last-appended id must still be present (most recent retained).
	last := string(rune('A'+29%26)) + string(rune('0'+29/26))
	if info.OldSessionIDs[len(info.OldSessionIDs)-1] != last {
		t.Errorf("most recent id dropped: last = %q, want %q", info.OldSessionIDs[len(info.OldSessionIDs)-1], last)
	}
}

// TestUpdateConfigNoLostUpdates hammers updateConfig from many goroutines, each
// adding a distinct session, and asserts none are dropped (the whole point of
// serializing the load→mutate→save cycle).
func TestUpdateConfigNoLostUpdates(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Seed a minimal config on disk.
	if err := saveConfig(&Config{BotToken: "x", ChatID: 1, Sessions: map[string]*SessionInfo{}}); err != nil {
		t.Fatalf("seed saveConfig: %v", err)
	}

	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "sess-" + string(rune('a'+i))
			updateConfig(func(c *Config) bool {
				c.Sessions[name] = &SessionInfo{TopicID: int64(1000 + i)}
				return true
			})
		}(i)
	}
	wg.Wait()

	loaded, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(loaded.Sessions) != n {
		t.Fatalf("lost updates: got %d sessions, want %d", len(loaded.Sessions), n)
	}
	for i := 0; i < n; i++ {
		name := "sess-" + string(rune('a'+i))
		if loaded.Sessions[name] == nil {
			t.Errorf("session %q missing", name)
		}
	}
}

// TestReapSessionNoOpOnMismatch checks the race-proof re-check: reapSession must
// leave the entry untouched when the expected ids don't match the current entry,
// or when a resume just started (fresh ResumingAt). Both paths return before any
// Telegram call, so no network is exercised.
func TestReapSessionNoOpOnMismatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	seed := func() {
		cfg := &Config{
			BotToken: "x", ChatID: 1, GroupID: -100,
			Sessions: map[string]*SessionInfo{
				"s": {TopicID: 42, SessionID: "uuid-live", ShortID: "short-live"},
			},
		}
		if err := saveConfig(cfg); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// Mismatched SessionID → no-op.
	seed()
	reapSession("s", "uuid-STALE", "short-live", 42)
	if c, _ := loadConfig(); c.Sessions["s"] == nil {
		t.Error("entry reaped despite SessionID mismatch")
	}

	// Mismatched ShortID → no-op.
	seed()
	reapSession("s", "uuid-live", "short-STALE", 42)
	if c, _ := loadConfig(); c.Sessions["s"] == nil {
		t.Error("entry reaped despite ShortID mismatch")
	}

	// Fresh ResumingAt with matching ids → no-op.
	cfg := &Config{
		BotToken: "x", ChatID: 1, GroupID: -100,
		Sessions: map[string]*SessionInfo{
			"s": {TopicID: 42, SessionID: "uuid-live", ShortID: "short-live", ResumingAt: time.Now().Unix()},
		},
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("seed resuming: %v", err)
	}
	reapSession("s", "uuid-live", "short-live", 42)
	if c, _ := loadConfig(); c.Sessions["s"] == nil {
		t.Error("entry reaped despite fresh ResumingAt")
	}
}
