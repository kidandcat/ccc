package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

// fakeTurner stands in for the model in the maintenance tests: it records the
// prompts it was given and answers with whatever the test wants back, so a
// compaction can be exercised end to end without spending a token.
type fakeTurner struct {
	// reply is called per attempt with the model name; returning an error makes
	// the attempt fail (which is how the model-fallback test works).
	reply   func(model, prompt string) (string, error)
	models  []string
	prompts []string
}

func (f *fakeTurner) PlainTurn(model, prompt string) (string, error) {
	f.models = append(f.models, model)
	f.prompts = append(f.prompts, prompt)
	return f.reply(model, prompt)
}

// answering is a turner that always returns the same text.
func answering(text string) *fakeTurner {
	return &fakeTurner{reply: func(string, string) (string, error) { return text, nil }}
}

// collector records the owner notifications a maintenance run produced.
type collector struct{ sent []string }

func (c *collector) notify(text string) { c.sent = append(c.sent, text) }

func (c *collector) joined() string { return strings.Join(c.sent, "\n") }

// seedTurns creates n turns for a bot, all created at the given time.
func seedTurns(t *testing.T, db *gorm.DB, botID int64, n int, at time.Time, input string) {
	t.Helper()
	for i := 0; i < n; i++ {
		started := at
		ended := at.Add(30 * time.Second)
		row := &Turn{
			BotID: botID, Source: sourceUser, Input: input, Output: "out", Status: turnDone,
			CreatedAt: at, StartedAt: &started, EndedAt: &ended,
			UsageJSON: `{"input_tokens":10,"output_tokens":5}`,
		}
		if err := db.Create(row).Error; err != nil {
			t.Fatalf("seed turn: %v", err)
		}
	}
}

// seedMemories creates n user-scope memories.
func seedMemories(t *testing.T, db *gorm.DB, n int) {
	t.Helper()
	base := time.Now().Add(-time.Duration(n) * time.Hour)
	for i := 0; i < n; i++ {
		m := Memory{
			Scope: scopeUser, Key: fmt.Sprintf("fact-%03d", i), Text: fmt.Sprintf("remembered thing number %d", i),
			CreatedAt: base.Add(time.Duration(i) * time.Hour), UpdatedAt: base.Add(time.Duration(i) * time.Hour),
		}
		if err := db.Create(&m).Error; err != nil {
			t.Fatalf("seed memory: %v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// Turn retention
// ---------------------------------------------------------------------------

// "30 days OR the last 200 per bot, whichever keeps more": a turn has to fail
// both halves before it is deleted.
func TestTurnRetentionKeepsWhicheverRuleKeepsMore(t *testing.T) {
	in, _, _ := testInstance(t)
	now := time.Now()
	old := now.AddDate(0, 0, -45)

	veteran, err := in.createBot("veteran", "")
	if err != nil {
		t.Fatal(err)
	}
	quiet, err := in.createBot("quiet", "")
	if err != nil {
		t.Fatal(err)
	}
	busy, err := in.createBot("busy", "")
	if err != nil {
		t.Fatal(err)
	}

	seedTurns(t, in.db, veteran.ID, turnRetentionPerBot+15, old, "x") // old AND over the count
	seedTurns(t, in.db, quiet.ID, 30, old, "x")                       // old but under the count
	seedTurns(t, in.db, busy.ID, turnRetentionPerBot+15, now, "x")    // over the count but recent

	var rep maintenanceReport
	deleted, _ := applyTurnRetention(in.db, now, &rep)
	if len(rep.Problems) != 0 {
		t.Fatalf("problems: %v", rep.Problems)
	}
	if deleted != 15 {
		// Only the veteran loses anything: 215 turns that are both outside the
		// last 200 and older than the window.
		t.Errorf("deleted = %d, want 15", deleted)
	}
	count := func(botID int64) int64 {
		var n int64
		in.db.Model(&Turn{}).Where("bot_id = ?", botID).Count(&n)
		return n
	}
	if got := count(veteran.ID); got != turnRetentionPerBot {
		t.Errorf("veteran kept %d turns, want the last %d: the count rule keeps more than the date rule here",
			got, turnRetentionPerBot)
	}
	if got := count(quiet.ID); got != 30 {
		t.Errorf("quiet kept %d turns, want all 30: fewer than %d turns is never pruned", got, turnRetentionPerBot)
	}
	if got := count(busy.ID); got != int64(turnRetentionPerBot+15) {
		t.Errorf("busy kept %d turns, want all %d: recent turns are never pruned", got, turnRetentionPerBot+15)
	}
}

// A turn older than a week keeps its usage and its status, but not its text.
func TestTurnBodiesAreTrimmedButUsageSurvives(t *testing.T) {
	in, b := testDB(t)
	now := time.Now()
	long := strings.Repeat("a very long message ", 200)

	oldTurn := &Turn{
		BotID: b.ID, Source: sourceUser, Input: long, Output: long, Status: turnDone,
		StopReason: "success", UsageJSON: `{"input_tokens":1234}`, CreatedAt: now.AddDate(0, 0, -10),
	}
	if err := in.db.Create(oldTurn).Error; err != nil {
		t.Fatal(err)
	}
	fresh := &Turn{BotID: b.ID, Source: sourceUser, Input: long, Output: long, Status: turnDone, CreatedAt: now}
	if err := in.db.Create(fresh).Error; err != nil {
		t.Fatal(err)
	}

	var rep maintenanceReport
	if n := trimTurnBodies(in.db, now, &rep); n != 1 {
		t.Fatalf("trimmed %d turns, want 1 (%v)", n, rep.Problems)
	}

	var got Turn
	in.db.First(&got, oldTurn.ID)
	if len(got.Input) != turnBodyLimit+len(turnBodyMarker) || !strings.HasSuffix(got.Input, turnBodyMarker) {
		t.Errorf("input was not trimmed to the limit with a marker: %d chars", len(got.Input))
	}
	if !strings.HasSuffix(got.Output, turnBodyMarker) {
		t.Error("output was not trimmed")
	}
	if got.UsageJSON != `{"input_tokens":1234}` || got.Status != turnDone || got.StopReason != "success" {
		t.Errorf("trimming must keep usage and status: %+v", got)
	}

	var untouched Turn
	in.db.First(&untouched, fresh.ID)
	if len(untouched.Input) != len(long) {
		t.Error("a turn from this week must keep its text")
	}

	// Running maintenance again must not eat into the summary it already made.
	var second maintenanceReport
	if n := trimTurnBodies(in.db, now, &second); n != 0 {
		t.Errorf("a second pass trimmed %d turns, want 0: trimming must be idempotent", n)
	}
	var again Turn
	in.db.First(&again, oldTurn.ID)
	if again.Input != got.Input {
		t.Error("a second pass rewrote an already trimmed body")
	}
}

// ---------------------------------------------------------------------------
// Memory compaction
// ---------------------------------------------------------------------------

// compactionAnswer builds a well-formed compaction result with n entries.
func compactionAnswer(n int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "merged-%03d: consolidated fact %d\n", i, i)
	}
	return sb.String()
}

func TestCompactionReplacesTheScopeAndArchivesTheOriginals(t *testing.T) {
	in, _, _ := testInstance(t)
	seedMemories(t, in.db, memCompactMaxCount+10)
	turner := answering(compactionAnswer(60))
	notes := &collector{}
	now := time.Now()

	rep := runMaintenance(in.db, in.cfg, maintenanceDeps{Turner: turner, Notify: notes.notify}, now)
	if len(rep.Problems) != 0 {
		t.Fatalf("problems: %v", rep.Problems)
	}
	if len(rep.Compactions) != 1 {
		t.Fatalf("compactions = %+v, want one", rep.Compactions)
	}
	c := rep.Compactions[0]
	if c.Before != memCompactMaxCount+10 || c.After != 60 {
		t.Errorf("compaction = %d → %d, want %d → 60", c.Before, c.After, memCompactMaxCount+10)
	}

	var live int64
	in.db.Model(&Memory{}).Where("scope = ?", scopeUser).Count(&live)
	if live != 60 {
		t.Errorf("%d memories left in the scope, want the 60 the compaction produced", live)
	}
	var archived int64
	in.db.Model(&MemoryArchive{}).Where("compaction_id = ?", c.CompactionID).Count(&archived)
	if archived != int64(memCompactMaxCount+10) {
		t.Errorf("%d rows archived, want every original (%d)", archived, memCompactMaxCount+10)
	}

	// The prompt is the scope as `key: text` lines, oldest first.
	if len(turner.prompts) != 1 {
		t.Fatalf("%d model calls, want exactly one per scope", len(turner.prompts))
	}
	if !strings.Contains(turner.prompts[0], "fact-000: remembered thing number 0") {
		t.Errorf("the prompt does not carry the memories as `key: text`:\n%s", truncate(turner.prompts[0], 400))
	}
	if turner.models[0] != defaultCompactionModel {
		t.Errorf("compaction ran on %q, want the cheap default %q", turner.models[0], defaultCompactionModel)
	}

	want := fmt.Sprintf("🧹 Compacted user memories: %d → 60 (/memory restore %d to undo)",
		memCompactMaxCount+10, c.CompactionID)
	if notes.joined() != want {
		t.Errorf("owner notification = %q, want %q", notes.joined(), want)
	}
}

func TestCompactionLeavesASmallScopeAlone(t *testing.T) {
	in, _, _ := testInstance(t)
	seedMemories(t, in.db, 20)
	turner := answering(compactionAnswer(5))

	rep := runMaintenance(in.db, in.cfg, maintenanceDeps{Turner: turner}, time.Now())
	if len(rep.Compactions) != 0 || len(turner.prompts) != 0 {
		t.Errorf("a scope under the threshold must not cost a model call: %+v", rep.Compactions)
	}
}

func TestCompactionIsTriggeredByBytesAsWellAsCount(t *testing.T) {
	in, _, _ := testInstance(t)
	big := strings.Repeat("x", 1024)
	for i := 0; i < 60; i++ {
		m := Memory{Scope: scopeUser, Key: fmt.Sprintf("big-%02d", i), Text: big}
		if err := in.db.Create(&m).Error; err != nil {
			t.Fatal(err)
		}
	}
	turner := answering(compactionAnswer(40))
	rep := runMaintenance(in.db, in.cfg, maintenanceDeps{Turner: turner}, time.Now())
	if len(rep.Compactions) != 1 {
		t.Fatalf("60 entries of 1 KB is over the %d byte threshold and must compact: %+v",
			memCompactMaxBytes, rep.Problems)
	}
}

func TestCompactionAbortsOnAParseFailure(t *testing.T) {
	in, _, _ := testInstance(t)
	seedMemories(t, in.db, memCompactMaxCount+10)
	notes := &collector{}
	turner := answering("Sure! Here is the consolidated list:\n\n- merged-1: something\n")

	rep := runMaintenance(in.db, in.cfg, maintenanceDeps{Turner: turner, Notify: notes.notify}, time.Now())
	if len(rep.Compactions) != 0 {
		t.Error("a result that does not parse must not be applied")
	}
	if len(rep.Problems) != 1 || !strings.Contains(rep.Problems[0], "abandoned") {
		t.Errorf("problems = %v, want the abandoned compaction", rep.Problems)
	}
	if !strings.Contains(notes.joined(), "abandoned") {
		t.Errorf("the owner was not told: %q", notes.joined())
	}
	var live int64
	in.db.Model(&Memory{}).Where("scope = ?", scopeUser).Count(&live)
	if live != int64(memCompactMaxCount+10) {
		t.Errorf("%d memories left, want every original untouched", live)
	}
	var archived int64
	in.db.Model(&MemoryArchive{}).Count(&archived)
	if archived != 0 {
		t.Error("an abandoned compaction must not archive anything")
	}
}

func TestCompactionAbortsWhenTooManyEntriesDisappear(t *testing.T) {
	in, _, _ := testInstance(t)
	seedMemories(t, in.db, memCompactMaxCount+10)
	notes := &collector{}
	// 10 entries out of 130 is a summary, not a consolidation.
	turner := answering(compactionAnswer(10))

	rep := runMaintenance(in.db, in.cfg, maintenanceDeps{Turner: turner, Notify: notes.notify}, time.Now())
	if len(rep.Compactions) != 0 {
		t.Error("a result that drops most of the entries must not be applied")
	}
	if len(rep.Problems) != 1 || !strings.Contains(rep.Problems[0], "kept only 10") {
		t.Errorf("problems = %v, want the drop to be named", rep.Problems)
	}
	var live int64
	in.db.Model(&Memory{}).Where("scope = ?", scopeUser).Count(&live)
	if live != int64(memCompactMaxCount+10) {
		t.Errorf("%d memories left, want every original untouched", live)
	}
}

// An unknown compaction model falls back to the instance model rather than
// leaving the memories to grow.
func TestCompactionFallsBackWhenTheModelIsUnknown(t *testing.T) {
	in, _, _ := testInstance(t)
	in.cfg.Model = "sonnet"
	seedMemories(t, in.db, memCompactMaxCount+10)
	in.cfg.CompactionModel = "haiku-from-the-future"
	turner := &fakeTurner{reply: func(model, _ string) (string, error) {
		if model == "haiku-from-the-future" {
			return "", errors.New(`[claude-code:unrecognized_model] {"model":"haiku-from-the-future"}`)
		}
		return compactionAnswer(70), nil
	}}

	rep := runMaintenance(in.db, in.cfg, maintenanceDeps{Turner: turner}, time.Now())
	if len(rep.Compactions) != 1 {
		t.Fatalf("the fallback did not run: %v", rep.Problems)
	}
	if len(turner.models) != 2 || turner.models[1] != "sonnet" {
		t.Errorf("models tried = %v, want the configured one then the instance model", turner.models)
	}
}

func TestCompactionReportsWhenNoModelIsAvailable(t *testing.T) {
	in, _, _ := testInstance(t)
	seedMemories(t, in.db, memCompactMaxCount+10)
	rep := runMaintenance(in.db, in.cfg, maintenanceDeps{}, time.Now())
	if len(rep.Problems) != 1 || !strings.Contains(rep.Problems[0], "no model") {
		t.Errorf("problems = %v, want the missing model to be reported", rep.Problems)
	}
}

func TestRestoreCompactionPutsTheOriginalsBack(t *testing.T) {
	in, _, _ := testInstance(t)
	seedMemories(t, in.db, memCompactMaxCount+10)
	rep := runMaintenance(in.db, in.cfg,
		maintenanceDeps{Turner: answering(compactionAnswer(60))}, time.Now())
	if len(rep.Compactions) != 1 {
		t.Fatalf("no compaction to restore: %v", rep.Problems)
	}
	id := rep.Compactions[0].CompactionID

	label, n, err := restoreCompaction(in.db, id)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if n != memCompactMaxCount+10 || label != "user memories" {
		t.Errorf("restored %d entries into %q", n, label)
	}
	var live []Memory
	in.db.Where("scope = ?", scopeUser).Order("key").Find(&live)
	if len(live) != memCompactMaxCount+10 {
		t.Fatalf("%d memories after the restore, want the originals back", len(live))
	}
	if live[0].Key != "fact-000" || live[0].Text != "remembered thing number 0" {
		t.Errorf("the restored rows are not the originals: %+v", live[0])
	}
	var leftover int64
	in.db.Model(&MemoryArchive{}).Where("compaction_id = ?", id).Count(&leftover)
	if leftover != 0 {
		t.Error("a restored compaction must consume its archive")
	}
	if _, _, err := restoreCompaction(in.db, id); err == nil {
		t.Error("restoring the same compaction twice should fail")
	}
}

func TestParseMemoryList(t *testing.T) {
	ok, err := parseMemoryList("deploy: fecha ships from deploy/ \n\nlikes: peninsular spanish\n")
	if err != nil {
		t.Fatalf("a clean list must parse: %v", err)
	}
	if len(ok) != 2 || ok[0].Key != "deploy" || ok[1].Text != "peninsular spanish" {
		t.Errorf("parsed = %+v", ok)
	}
	// A repeated key is one entry, and the later line wins.
	dup, err := parseMemoryList("k: first\nk: second\n")
	if err != nil || len(dup) != 1 || dup[0].Text != "second" {
		t.Errorf("duplicate keys = %+v (%v)", dup, err)
	}
	for _, bad := range []string{
		"Here is the list:\ndeploy: ships",
		"- deploy: ships",
		"```\ndeploy: ships\n```",
		"deploy ships from deploy/",
		"",
		": no key",
	} {
		if _, err := parseMemoryList(bad); err == nil {
			t.Errorf("parsing %q should have failed", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// Cleanup
// ---------------------------------------------------------------------------

func TestCleanupRemovesWhatNobodyCanReadAnyMore(t *testing.T) {
	in, b := testDB(t)
	now := time.Now()
	old := now.AddDate(0, 0, -40)
	recent := now.AddDate(0, 0, -2)

	mustCreate := func(v any) {
		t.Helper()
		if err := in.db.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	mustCreate(&InboxMessage{ToBotID: b.ID, Text: "old", DeliveredAt: &old})
	mustCreate(&InboxMessage{ToBotID: b.ID, Text: "recent", DeliveredAt: &recent})
	mustCreate(&InboxMessage{ToBotID: b.ID, Text: "pending"})
	mustCreate(&Question{BotID: b.ID, Question: "old?", AnsweredAt: &old})
	mustCreate(&Question{BotID: b.ID, Question: "open?"})

	// A bot archived more than a month ago: its private notes go with it.
	gone, err := in.createBot("retired", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := in.db.Model(&Bot{}).Where("id = ?", gone.ID).Update("archived_at", old).Error; err != nil {
		t.Fatal(err)
	}
	mustCreate(&Memory{Scope: scopeBot, ScopeKey: fmt.Sprint(gone.ID), Key: "note", Text: "mine"})
	mustCreate(&Memory{Scope: scopeBot, ScopeKey: fmt.Sprint(b.ID), Key: "note", Text: "still live"})
	mustCreate(&MemoryArchive{CompactionID: 1, ArchivedAt: now.AddDate(0, 0, -100), Scope: scopeUser, Key: "ancient"})
	mustCreate(&MemoryArchive{CompactionID: 2, ArchivedAt: now.AddDate(0, 0, -10), Scope: scopeUser, Key: "fresh"})

	var rep maintenanceReport
	runCleanup(in.db, in.config(), now, &rep)
	if len(rep.Problems) != 0 {
		t.Fatalf("problems: %v", rep.Problems)
	}
	if rep.InboxDeleted != 1 || rep.QuestionsDeleted != 1 || rep.BotMemoriesDeleted != 1 || rep.ArchivesDeleted != 1 {
		t.Errorf("cleanup report = %+v, want exactly one row of each kind", rep)
	}
	var inbox int64
	in.db.Model(&InboxMessage{}).Count(&inbox)
	if inbox != 2 {
		t.Errorf("%d inbox rows left, want the recent and the undelivered one", inbox)
	}
	var mems int64
	in.db.Model(&Memory{}).Where("scope = ?", scopeBot).Count(&mems)
	if mems != 1 {
		t.Errorf("%d bot memories left, want only the live bot's", mems)
	}
}

// ---------------------------------------------------------------------------
// Scheduling
// ---------------------------------------------------------------------------

func TestMaintenanceRunsOnceADayAfterTheQuietHour(t *testing.T) {
	in, _, _ := testInstance(t)
	day := time.Date(2026, 9, 14, 0, 0, 0, 0, time.Local)

	if maintenanceDue(in.db, in.cfg, day.Add(2*time.Hour)) {
		t.Error("02:00 is before the default quiet hour; nothing is due yet")
	}
	if !maintenanceDue(in.db, in.cfg, day.Add(5*time.Hour)) {
		t.Error("05:00 is past the quiet hour and nothing has run today")
	}
	markMaintenanceRun(in.db, day.Add(5*time.Hour))
	if maintenanceDue(in.db, in.cfg, day.Add(9*time.Hour)) {
		t.Error("maintenance must run once a day, not once a tick")
	}
	if !maintenanceDue(in.db, in.cfg, day.AddDate(0, 0, 1).Add(5*time.Hour)) {
		t.Error("the next day is due again")
	}

	hour := 22
	in.cfg.MaintenanceHour = &hour
	if maintenanceDue(in.db, in.cfg, day.AddDate(0, 0, 1).Add(21*time.Hour)) {
		t.Error("the configured hour is not respected")
	}
}

func TestMemoryStatsReportsSizeAndLastCompaction(t *testing.T) {
	in, _, _ := testInstance(t)
	seedMemories(t, in.db, memCompactMaxCount+10)
	rep := runMaintenance(in.db, in.cfg, maintenanceDeps{Turner: answering(compactionAnswer(60))}, time.Now())
	if len(rep.Compactions) != 1 {
		t.Fatalf("no compaction: %v", rep.Problems)
	}
	card := renderMemoryStats(in.db)
	for _, want := range []string{"user memories", "60 entries", "last compaction #"} {
		if !strings.Contains(card, want) {
			t.Errorf("/memory stats is missing %q:\n%s", want, card)
		}
	}
}

// Trimming has to walk past rows it cannot trim: an already-trimmed body still
// matches the "too long" query, so a page full of them must not stall the pass.
func TestTrimmingWalksPastAlreadyTrimmedRows(t *testing.T) {
	in, b := testDB(t)
	now := time.Now()
	old := now.AddDate(0, 0, -10)
	long := strings.Repeat("y", 4000)

	// Two rows that are already trimmed, then one that is not.
	for i := 0; i < 2; i++ {
		row := &Turn{BotID: b.ID, Source: sourceUser, Status: turnDone, CreatedAt: old,
			Input: strings.Repeat("z", turnBodyLimit) + turnBodyMarker}
		if err := in.db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	last := &Turn{BotID: b.ID, Source: sourceUser, Status: turnDone, CreatedAt: old, Input: long}
	if err := in.db.Create(last).Error; err != nil {
		t.Fatal(err)
	}

	var rep maintenanceReport
	if n := trimTurnBodies(in.db, now, &rep); n != 1 {
		t.Fatalf("trimmed %d rows, want the one row behind the already-trimmed ones (%v)", n, rep.Problems)
	}
	var got Turn
	in.db.First(&got, last.ID)
	if !strings.HasSuffix(got.Input, turnBodyMarker) {
		t.Error("the untrimmed row was skipped")
	}
}
