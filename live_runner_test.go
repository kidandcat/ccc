//go:build live

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A real end-to-end exercise of the v3 runner against the installed `claude`
// and the real `ccc mcp` server. No Telegram is involved: the instance has no
// bot token, so every Telegram call is skipped and the assertions are made
// against the database.
//
//	go test -tags live -run TestLive -v -timeout 900s
//
// It spends tokens on the default profile, so it is behind a build tag.

type noopUI struct{}

func (noopUI) Post(int64, string) (int64, error) { return 0, nil }
func (noopUI) Edit(int64, int64, string) error   { return nil }
func (noopUI) React(int64, string)               {}

// liveSetup builds the ccc binary (the MCP server Claude Code spawns is this
// same binary, and under `go test` os.Executable() is the test binary), opens a
// throwaway database and returns a runner wired to the default profile.
func liveSetup(t *testing.T) (*Runner, *Bot) {
	t.Helper()
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude is not on PATH")
	}
	dir := t.TempDir()

	bin := filepath.Join(dir, "ccc")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build ccc: %v", err)
	}
	oldPath := cccPath
	cccPath = bin
	t.Cleanup(func() { cccPath = oldPath })

	cfg := &Config{DataDir: dir, Model: "sonnet"} // no BotToken: Telegram is skipped
	db, err := openStore(dbPath(cfg))
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	ws := botWorkspace(cfg, "liveprobe")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	b := &Bot{Name: "liveprobe", Role: "a test bot exercising the ccc tools", Cwd: ws, Status: botIdle}
	if err := db.Create(b).Error; err != nil {
		t.Fatal(err)
	}
	r := newRunner(db, cfg, noopUI{})
	t.Cleanup(r.Close)
	// The transcripts land in the PROFILE's projects/ dir, outside t.TempDir():
	// remove the ones this test created so it leaves nothing behind.
	t.Cleanup(func() { removeLiveTranscripts(t, ws) })
	return r, b
}

// removeLiveTranscripts deletes the default profile's transcript directory for
// a workspace path. Claude Code slugs the cwd by replacing every non-alphanumeric
// character with "-", so the directory is found by matching that slug.
func removeLiveTranscripts(t *testing.T, cwd string) {
	t.Helper()
	// claude records the RESOLVED cwd, and on macOS t.TempDir() hands back a
	// /var/... path that really lives under /private/var/...
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	slug := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, cwd)
	dir := filepath.Join(profileProjectsDir(implicitProfile()), slug)
	if _, err := os.Stat(dir); err != nil {
		t.Logf("no transcript dir to clean at %s", dir)
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Logf("could not clean %s: %v", dir, err)
	}
}

// runTurn enqueues one input and waits for the turn to leave the queue.
func runTurn(t *testing.T, r *Runner, botID int64, text string) *Turn {
	t.Helper()
	queued, err := r.Enqueue(botID, sourceUser, text, 0)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		var turn Turn
		if err := r.db.First(&turn, queued.ID).Error; err != nil {
			t.Fatalf("reload turn: %v", err)
		}
		if turn.Status == turnDone || turn.Status == turnFailed {
			if turn.Status == turnFailed {
				t.Fatalf("turn failed (%s): %s", turn.ErrorClass, turn.StopReason)
			}
			return &turn
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("turn did not finish within 5 minutes")
	return nil
}

func liveAgentCount(t *testing.T) int {
	t.Helper()
	out, err := runClaudeOutput(implicitProfile(), 30*time.Second, "agents", "--json", "--all")
	if err != nil && len(out) == 0 {
		t.Logf("claude agents unavailable: %v", err)
		return -1
	}
	var agents []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out, &agents); err != nil {
		t.Logf("cannot parse the agents view: %v", err)
		return -1
	}
	return len(agents)
}

// TestLiveMemoryRoundTrip: a turn stores a memory through the MCP server, and
// the NEXT turn (a --resume of the same session) reads it back.
func TestLiveMemoryRoundTrip(t *testing.T) {
	r, b := liveSetup(t)
	before := liveAgentCount(t)

	const key = "live-probe-key"
	const secret = "the tortoise wears a blue hat"

	first := runTurn(t, r, b.ID,
		"Call the remember tool with scope=\"bot\", key=\""+key+"\" and text=\""+secret+"\". "+
			"Then reply with exactly: STORED")
	t.Logf("turn 1 output: %s", truncate(first.Output, 200))

	var mem Memory
	if err := r.db.Where("key = ?", key).First(&mem).Error; err != nil {
		t.Fatalf("the remember tool did not write a memory row: %v", err)
	}
	if mem.Text != secret {
		t.Errorf("stored text = %q, want %q", mem.Text, secret)
	}
	if mem.Scope != scopeBot {
		t.Errorf("scope = %q, want bot", mem.Scope)
	}

	reloaded, err := botByID(r.db, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.SessionID == "" {
		t.Fatal("the first turn did not record a session id, so turn 2 cannot resume it")
	}

	second := runTurn(t, r, b.ID,
		"Call the recall tool with query \""+key+"\", then reply with ONLY the exact text that was stored, nothing else.")
	t.Logf("turn 2 output: %s", truncate(second.Output, 200))
	if !strings.Contains(strings.ToLower(second.Output), "tortoise wears a blue hat") {
		t.Errorf("the resumed turn did not recall the stored text; output was %q", second.Output)
	}

	after := liveAgentCount(t)
	if before >= 0 && after >= 0 && after != before {
		t.Errorf("the agents view changed (%d -> %d): `claude -p` turns must not leave background sessions", before, after)
	}
}

// TestLiveAskOwner: a turn that asks the owner a question records it and ends.
func TestLiveAskOwner(t *testing.T) {
	r, b := liveSetup(t)

	turn := runTurn(t, r, b.ID,
		"Call the ask_owner tool with question \"Should I deploy to production?\" and "+
			"options [\"yes\",\"no\"]. As soon as it returns, end your turn without doing anything else.")
	t.Logf("turn output: %s", truncate(turn.Output, 200))

	var q Question
	if err := r.db.Where("bot_id = ?", b.ID).First(&q).Error; err != nil {
		t.Fatalf("ask_owner did not create a questions row: %v", err)
	}
	if !strings.Contains(strings.ToLower(q.Question), "deploy") {
		t.Errorf("stored question = %q", q.Question)
	}
	if q.AnsweredAt != nil {
		t.Error("the question should still be unanswered")
	}
	opts := questionOptions(&q)
	if len(opts) == 0 || opts[len(opts)-1] != skipOptionLabel {
		t.Errorf("options = %v, want Omitir last", opts)
	}

	// The turn ended, and the bot is parked waiting for the answer.
	after, err := botByID(r.db, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != botWaiting {
		t.Errorf("bot status = %q, want waiting after ask_owner", after.Status)
	}

	// A queued input must not run while the bot waits.
	if _, err := r.Enqueue(b.ID, sourceUser, "are you there?", 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	var queued int64
	r.db.Model(&Turn{}).Where("bot_id = ? AND status = ?", b.ID, turnQueued).Count(&queued)
	if queued != 1 {
		t.Errorf("%d turns queued, want the input to stay queued while the bot waits", queued)
	}

	// Answering releases it.
	answer := answerQuestion(r.db, &q, "no")
	if !strings.Contains(answer, "Should I deploy") || !strings.Contains(answer, "no") {
		t.Errorf("answer envelope = %q", answer)
	}
	var stored Question
	r.db.First(&stored, q.ID)
	if stored.AnsweredAt == nil {
		t.Error("the answer was not recorded")
	}
}

// TestLiveMemoryCompaction: the real compaction turn (DESIGN §7) against the
// installed claude, on the cheap model the setting names. Ten synthetic
// memories, two of them obvious duplicates of others, must come back as fewer
// entries — with every original safe in memories_archive.
func TestLiveMemoryCompaction(t *testing.T) {
	r, _ := liveSetup(t)
	cfg := r.config()
	// The compaction turn runs in the data dir, so its transcript lands under a
	// different slug than the bot's workspace and needs its own cleanup.
	t.Cleanup(func() { removeLiveTranscripts(t, dataDir(cfg)) })
	before := liveAgentCount(t)

	seed := []struct{ key, text string }{
		{"deploy-target", "fecha is deployed to the OVH VPS with systemd and Caddy"},
		{"deploy-host", "fecha is deployed to the OVH VPS with systemd and Caddy"}, // duplicate of deploy-target
		{"db-choice", "personal projects use SQLite with Litestream replication"},
		{"database", "personal projects use SQLite with Litestream replication"}, // duplicate of db-choice
		{"language", "the owner writes code and commits in English"},
		{"mobile-stack", "new mobile apps are built with Flutter, not Expo"},
		{"ci-mobile", "mobile builds run on Codemagic, triggered by v* git tags"},
		{"containers", "local containers run on Colima, not Docker Desktop"},
		{"monitoring", "every deployed project is registered in the Gatus status panel"},
		{"editor-note", "the owner prefers short, concrete replies in chat"},
	}
	for i, s := range seed {
		m := Memory{
			Scope: scopeUser, Key: s.key, Text: s.text,
			CreatedAt: time.Now().Add(time.Duration(i) * time.Minute),
			UpdatedAt: time.Now().Add(time.Duration(i) * time.Minute),
		}
		if err := r.db.Create(&m).Error; err != nil {
			t.Fatal(err)
		}
	}

	st := memoryScopeStat{Scope: scopeUser, ScopeKey: "", Count: len(seed)}
	res, err := compactScope(r.db, cfg, r, st, "user memories", time.Now())
	if err != nil {
		t.Fatalf("compaction failed: %v", err)
	}
	t.Logf("compaction %d: %d → %d", res.CompactionID, res.Before, res.After)

	if res.Before != len(seed) {
		t.Errorf("compacted %d entries, want the %d seeded", res.Before, len(seed))
	}
	if res.After >= res.Before {
		t.Errorf("the merged list has %d entries, want fewer than %d: the duplicates were not merged",
			res.After, res.Before)
	}

	var live []Memory
	if err := r.db.Where("scope = ?", scopeUser).Order("key").Find(&live).Error; err != nil {
		t.Fatal(err)
	}
	if len(live) != res.After {
		t.Errorf("%d memories in the scope, want the %d the compaction produced", len(live), res.After)
	}
	for _, m := range live {
		if strings.TrimSpace(m.Key) == "" || strings.TrimSpace(m.Text) == "" {
			t.Errorf("a compacted memory is empty: %+v", m)
		}
	}

	var archived []MemoryArchive
	if err := r.db.Where("compaction_id = ?", res.CompactionID).Order("id").Find(&archived).Error; err != nil {
		t.Fatal(err)
	}
	if len(archived) != len(seed) {
		t.Fatalf("%d rows archived, want every original (%d)", len(archived), len(seed))
	}
	for i, a := range archived {
		if a.Key != seed[i].key || a.Text != seed[i].text {
			t.Errorf("archive row %d = %s/%q, want %s/%q", i, a.Key, a.Text, seed[i].key, seed[i].text)
		}
	}

	// And the undo works against what the real model produced.
	if _, n, err := restoreCompaction(r.db, res.CompactionID); err != nil || n != len(seed) {
		t.Errorf("restore put back %d entries (%v), want %d", n, err, len(seed))
	}

	after := liveAgentCount(t)
	if before >= 0 && after >= 0 && after != before {
		t.Errorf("the agents view changed (%d -> %d): a compaction turn must not leave a background session", before, after)
	}
}
