package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/gorm"
	"time"
)

// testScheduler wires a scheduler onto a test instance. The runner stays fake,
// so a watch that fires records a turn instead of spawning claude.
func testScheduler(t *testing.T) (*scheduler, *instance, *fakeRunner, *fakeBotAPI) {
	t.Helper()
	in, runner, api := testInstance(t)
	s := newScheduler(in)
	in.sched = s
	return s, in, runner, api
}

// A watch wakes its bot only when the command's output changes — the first run
// is a baseline, an unchanged run is free, and a change carries a diff.
func TestWatchWakesTheBotOnlyWhenTheOutputChanges(t *testing.T) {
	s, in, runner, _ := testScheduler(t)
	b, err := in.createBot("watcher", "")
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(t.TempDir(), "state.txt")
	if err := os.WriteFile(state, []byte("build: green\ntests: 12 passing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := upsertWatch(in.db, b.ID, "ci", "cat "+state, 60); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	s.runDueWatches(now) // baseline
	if len(runner.enqueued) != 0 {
		t.Fatalf("the first run must not wake anybody: %+v", runner.enqueued)
	}
	var w Watch
	if err := in.db.Where("bot_id = ?", b.ID).First(&w).Error; err != nil {
		t.Fatal(err)
	}
	if w.LastHash == "" || w.LastRunAt == nil {
		t.Fatal("the baseline was not recorded")
	}

	// Unchanged output, interval elapsed: still nothing.
	s.runDueWatches(now.Add(2 * time.Minute))
	if len(runner.enqueued) != 0 {
		t.Fatalf("an unchanged watch woke the bot: %+v", runner.enqueued)
	}

	// Now it changes.
	if err := os.WriteFile(state, []byte("build: RED\ntests: 11 passing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.runDueWatches(now.Add(4 * time.Minute))
	if len(runner.enqueued) != 1 {
		t.Fatalf("a changed watch enqueued %d turns, want 1", len(runner.enqueued))
	}
	turn := runner.enqueued[0]
	if turn.BotID != b.ID || turn.Source != sourceWatch {
		t.Errorf("turn = %+v, want a watch turn for the bot", turn)
	}
	for _, want := range []string{`Watch "ci" changed`, "- build: green", "+ build: RED"} {
		if !strings.Contains(turn.Text, want) {
			t.Errorf("the watch input is missing %q:\n%s", want, turn.Text)
		}
	}
	// Only the diff travels, never the whole before/after.
	if strings.Contains(turn.Text, "tests: 12 passing") && strings.Contains(turn.Text, "- tests: 12 passing") == false {
		t.Errorf("unchanged context leaked into the input:\n%s", turn.Text)
	}
}

// A watch respects its interval rather than running every tick.
func TestWatchRespectsItsInterval(t *testing.T) {
	s, in, _, _ := testScheduler(t)
	b, err := in.createBot("watcher", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upsertWatch(in.db, b.ID, "clock", "date +%s%N", 600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.runDueWatches(now)
	var first Watch
	in.db.Where("bot_id = ?", b.ID).First(&first)

	s.runDueWatches(now.Add(time.Minute)) // well inside the 10-minute interval
	var second Watch
	in.db.Where("bot_id = ?", b.ID).First(&second)
	if !first.LastRunAt.Equal(*second.LastRunAt) {
		t.Error("the watch ran again before its interval had elapsed")
	}
}

func TestWatchIntervalHasAFloor(t *testing.T) {
	_, in, _, _ := testScheduler(t)
	b, err := in.createBot("watcher", "")
	if err != nil {
		t.Fatal(err)
	}
	w, err := upsertWatch(in.db, b.ID, "hammer", "true", 1)
	if err != nil {
		t.Fatal(err)
	}
	if w.IntervalS != watchMinInterval {
		t.Errorf("interval = %d, want it clamped to %d", w.IntervalS, watchMinInterval)
	}
}

// Changing a watch's command resets its baseline, so the next run does not
// report the difference between two unrelated commands as a change.
func TestChangingAWatchCommandResetsTheBaseline(t *testing.T) {
	s, in, runner, _ := testScheduler(t)
	b, err := in.createBot("watcher", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upsertWatch(in.db, b.ID, "x", "echo one", 60); err != nil {
		t.Fatal(err)
	}
	s.runDueWatches(time.Now())
	if _, err := upsertWatch(in.db, b.ID, "x", "echo two", 60); err != nil {
		t.Fatal(err)
	}
	s.runDueWatches(time.Now())
	if len(runner.enqueued) != 0 {
		t.Errorf("switching commands reported a spurious change: %+v", runner.enqueued)
	}
}

func TestWatchOfAnArchivedBotIsDisabled(t *testing.T) {
	s, in, runner, _ := testScheduler(t)
	b, err := in.createBot("goner", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upsertWatch(in.db, b.ID, "x", "echo hi", 60); err != nil {
		t.Fatal(err)
	}
	if err := archiveBotRow(in.db, b.ID); err != nil {
		t.Fatal(err)
	}
	s.runDueWatches(time.Now())
	var w Watch
	in.db.Where("bot_id = ?", b.ID).First(&w)
	if w.Enabled {
		t.Error("a watch on an archived bot must stop running")
	}
	if len(runner.enqueued) != 0 {
		t.Error("an archived bot was woken")
	}
}

// A schedule fires once at its time; a cron schedule rolls forward instead.
func TestSchedulesFire(t *testing.T) {
	s, in, runner, _ := testScheduler(t)
	b, err := in.createBot("sleeper", "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()

	once := Schedule{BotID: b.ID, FireAt: now.Add(-time.Minute), Note: "check the deploy"}
	later := Schedule{BotID: b.ID, FireAt: now.Add(time.Hour), Note: "not yet"}
	daily := Schedule{BotID: b.ID, FireAt: now.Add(-time.Minute), Note: "morning report", RecurringCron: "0 9 * * *"}
	for _, sc := range []*Schedule{&once, &later, &daily} {
		if err := in.db.Create(sc).Error; err != nil {
			t.Fatal(err)
		}
	}

	s.fireDueSchedules(now)

	if len(runner.enqueued) != 2 {
		t.Fatalf("%d turns enqueued, want the two due schedules: %+v", len(runner.enqueued), runner.enqueued)
	}
	var notes []string
	for _, e := range runner.enqueued {
		if e.Source != sourceSchedule {
			t.Errorf("source = %q, want schedule", e.Source)
		}
		notes = append(notes, e.Text)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "check the deploy") || !strings.Contains(joined, "morning report") {
		t.Errorf("the notes did not reach the bot:\n%s", joined)
	}
	if strings.Contains(joined, "not yet") {
		t.Error("a future schedule fired early")
	}

	var firedOnce Schedule
	in.db.First(&firedOnce, once.ID)
	if firedOnce.FiredAt == nil {
		t.Error("the one-shot schedule was not retired")
	}
	var recurring Schedule
	in.db.First(&recurring, daily.ID)
	if recurring.FiredAt != nil {
		t.Error("a recurring schedule must not be retired")
	}
	if !recurring.FireAt.After(now) {
		t.Errorf("the recurring schedule did not roll forward: %s", recurring.FireAt)
	}

	// Firing again changes nothing: the due ones are gone or moved.
	before := len(runner.enqueued)
	s.fireDueSchedules(now)
	if len(runner.enqueued) != before {
		t.Error("a schedule fired twice for the same due time")
	}
}

func TestRoutineFiresInTimezoneAndPosts(t *testing.T) {
	madrid, err := time.LoadLocation(defaultRoutineTZ)
	if err != nil {
		t.Skip("timezone data not available")
	}
	s, in, runner, api := testScheduler(t)
	b, err := in.createBot("sales", "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, madrid)
	row, err := upsertRoutine(in.db, b.ID, "morning-ventas", "mira ventas", "0 9 * * *", defaultRoutineTZ, now)
	if err != nil {
		t.Fatal(err)
	}
	wantNext := time.Date(2026, 9, 16, 9, 0, 0, 0, madrid)
	if !row.FireAt.Equal(wantNext) {
		t.Fatalf("next fire = %s, want %s", row.FireAt, wantNext)
	}

	// Due now: fire as a fresh worker (not a turn of sales), post ⏰, roll.
	in.db.Model(&Schedule{}).Where("id = ?", row.ID).Update("fire_at", now.Add(-time.Minute))
	s.fireDueSchedules(now)
	if len(runner.enqueued) != 1 {
		t.Fatalf("%d turns, want 1: %+v", len(runner.enqueued), runner.enqueued)
	}
	got := runner.enqueued[0]
	if got.BotID == b.ID {
		t.Error("a named routine must not run as a turn of the owning bot")
	}
	worker, err := botByID(in.db, got.BotID)
	if err != nil {
		t.Fatalf("routine worker: %v", err)
	}
	if isGeneralBot(worker) || worker.TopicID >= 0 {
		t.Errorf("routine worker should be a backend session, got %+v", worker)
	}
	if !strings.HasPrefix(worker.Name, "routine-morning-ventas") {
		t.Errorf("worker name = %q, want routine-morning-ventas…", worker.Name)
	}
	if got.Source != "routine:morning-ventas" {
		t.Errorf("source = %q, want routine:morning-ventas", got.Source)
	}
	if !strings.Contains(got.Text, "mira ventas") {
		t.Errorf("prompt missing from turn: %q", got.Text)
	}
	if !strings.Contains(got.Text, "you are not General") || !strings.Contains(got.Text, "archive_bot") {
		t.Errorf("worker prompt must isolate the job:\n%s", got.Text)
	}
	posted := false
	for _, c := range api.since("sendMessage") {
		if strings.Contains(c.Params.Get("text"), "⏰") && strings.Contains(c.Params.Get("text"), "morning-ventas") {
			posted = true
		}
	}
	if !posted {
		t.Error("firing a routine did not post ⏰ in the topic")
	}
	var after Schedule
	in.db.First(&after, row.ID)
	if after.FiredAt != nil {
		t.Error("a routine must not be retired")
	}
	if !after.FireAt.Equal(wantNext) {
		t.Errorf("rolled to %s, want %s", after.FireAt, wantNext)
	}

	// Second fire reuses the same worker (even if it archived itself) with a
	// fresh conversation, rather than routine-morning-ventas-2.
	if err := archiveBotRow(in.db, worker.ID); err != nil {
		t.Fatal(err)
	}
	in.db.Model(&Bot{}).Where("id = ?", worker.ID).Update("session_id", "old-transcript")
	in.db.Model(&Schedule{}).Where("id = ?", row.ID).Update("fire_at", now.Add(-time.Minute))
	s.fireDueSchedules(now.Add(time.Second))
	if len(runner.enqueued) != 2 {
		t.Fatalf("second fire enqueued %d, want 2: %+v", len(runner.enqueued), runner.enqueued)
	}
	if runner.enqueued[1].BotID != worker.ID {
		t.Errorf("second fire bot = %d, want reused %d", runner.enqueued[1].BotID, worker.ID)
	}
	var reused Bot
	if err := in.db.First(&reused, worker.ID).Error; err != nil {
		t.Fatal(err)
	}
	if reused.ArchivedAt != nil {
		t.Error("reused routine worker should be live again")
	}
	if reused.SessionID != "" {
		t.Errorf("session_id = %q, want cleared so yesterday is not reread", reused.SessionID)
	}
	var extras int64
	in.db.Model(&Bot{}).Where("name LIKE ? AND id != ?", "routine-morning-ventas%", worker.ID).Count(&extras)
	if extras != 0 {
		t.Errorf("routine fire piled %d extra workers", extras)
	}
}

func TestRoutineReusesNamedWorker(t *testing.T) {
	s, in, runner, _ := testScheduler(t)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	row, err := upsertRoutine(in.db, chief.ID, "morning-ventas", "mira ventas", "@hourly", defaultRoutineTZ, now)
	if err != nil {
		t.Fatal(err)
	}
	in.db.Model(&Schedule{}).Where("id = ?", row.ID).Update("fire_at", now.Add(-time.Minute))
	s.fireDueSchedules(now)
	if len(runner.enqueued) != 1 {
		t.Fatalf("first fire: %d turns", len(runner.enqueued))
	}
	first := runner.enqueued[0].BotID
	if first == chief.ID {
		t.Fatal("routine ran on General")
	}
	in.db.Model(&Schedule{}).Where("id = ?", row.ID).Update("fire_at", now.Add(-time.Second))
	s.fireDueSchedules(now.Add(time.Minute))
	if len(runner.enqueued) != 2 {
		t.Fatalf("second fire: %d turns", len(runner.enqueued))
	}
	if runner.enqueued[1].BotID != first {
		t.Errorf("second fire spawned %d, want reuse of %d", runner.enqueued[1].BotID, first)
	}
	var workers int64
	in.db.Model(&Bot{}).Where("id != ? AND archived_at IS NULL", chief.ID).Count(&workers)
	if workers != 1 {
		t.Errorf("routine fires piled %d workers, want 1 reused", workers)
	}
}

func TestSetRoutineToolUpsertsByName(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("sales", "")
	if err != nil {
		t.Fatal(err)
	}
	s := &mcpServer{db: in.db, config: in.cfg, botID: b.ID}
	res, _, err := s.setRoutine(t.Context(), nil, setRoutineIn{
		Name: "morning-ventas", Prompt: "mira ventas", Cron: "0 9 * * 1-5",
	})
	if err != nil || res.IsError {
		t.Fatalf("set_routine: %+v %v", res, err)
	}
	res, _, err = s.setRoutine(t.Context(), nil, setRoutineIn{
		Name: "morning-ventas", Prompt: "mira ventas y Resend", Cron: "0 9 * * 1-5", Timezone: defaultRoutineTZ,
	})
	if err != nil || res.IsError {
		t.Fatalf("upsert: %+v %v", res, err)
	}
	rows, err := listRoutines(in.db, b.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("%d routines, want 1 (%v)", len(rows), err)
	}
	if rows[0].Note != "mira ventas y Resend" || rows[0].Timezone != defaultRoutineTZ {
		t.Errorf("upsert did not replace: %+v", rows[0])
	}

	other, err := in.createBot("other", "")
	if err != nil {
		t.Fatal(err)
	}
	otherS := &mcpServer{db: in.db, config: in.cfg, botID: other.ID}
	if res, _, _ := otherS.cancelRoutineTool(t.Context(), nil, cancelRoutineIn{Name: "morning-ventas"}); res.IsError {
		t.Fatal(res)
	}
	if left, _ := listRoutines(in.db, b.ID); len(left) != 1 {
		t.Error("another bot cancelled this bot's routine")
	}
	if res, _, _ := s.cancelRoutineTool(t.Context(), nil, cancelRoutineIn{Name: "morning-ventas"}); res.IsError {
		t.Fatal(res)
	}
	if left, _ := listRoutines(in.db, b.ID); len(left) != 0 {
		t.Error("cancel_routine did not remove it")
	}
}

func TestParseRoutineAdd(t *testing.T) {
	name, cron, tz, prompt, err := parseRoutineAdd([]string{
		"morning-ventas", "--cron", "0 9 * * 1-5", "--tz", "Europe/Madrid", "mira", "ventas",
	})
	if err != nil {
		t.Fatal(err)
	}
	if name != "morning-ventas" || cron != "0 9 * * 1-5" || tz != "Europe/Madrid" || prompt != "mira ventas" {
		t.Errorf("got %q %q %q %q", name, cron, tz, prompt)
	}
}

func TestParseCron(t *testing.T) {
	for _, good := range []string{"0 9 * * *", "*/15 * * * *", "@daily", "@every 1h"} {
		if _, err := parseCron(good); err != nil {
			t.Errorf("parseCron(%q) failed: %v", good, err)
		}
	}
	for _, bad := range []string{"", "not a cron", "99 99 99 99 99"} {
		if _, err := parseCron(bad); err == nil {
			t.Errorf("parseCron(%q) should have failed", bad)
		}
	}
}

func TestLineDiffIsTruncated(t *testing.T) {
	before := ""
	var after strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&after, "line %d\n", i)
	}
	diff := lineDiff(before, after.String(), 512)
	if len(diff) > 700 {
		t.Errorf("diff is %d bytes, want it capped near 512", len(diff))
	}
	if !strings.Contains(diff, "truncated") {
		t.Errorf("a truncated diff should say so: %q", diff)
	}
}

func TestLineDiffReportsBothSides(t *testing.T) {
	diff := lineDiff("a\nb\nc\n", "a\nc\nd\n", 4096)
	if !strings.Contains(diff, "- b") {
		t.Errorf("a removed line is missing: %q", diff)
	}
	if !strings.Contains(diff, "+ d") {
		t.Errorf("an added line is missing: %q", diff)
	}
	if strings.Contains(diff, "a") && strings.Contains(diff, "- a") {
		t.Errorf("an unchanged line was reported: %q", diff)
	}
}

func TestArchiveBotClosesTheTopicAndStopsAutomation(t *testing.T) {
	in, _, api := testInstance(t)
	b, err := in.createBot("done", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upsertWatch(in.db, b.ID, "x", "echo hi", 60); err != nil {
		t.Fatal(err)
	}
	if err := in.db.Create(&Schedule{BotID: b.ID, FireAt: time.Now().Add(time.Hour), Note: "later"}).Error; err != nil {
		t.Fatal(err)
	}

	s := &mcpServer{db: in.db, config: in.cfg, botID: b.ID}
	res, _, err := s.archiveBot(t.Context(), nil, archiveBotIn{})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("archiveBot failed: %+v", res.Content)
	}

	if _, err := botByName(in.db, "done"); err == nil {
		t.Error("the bot is still live after being archived")
	}
	if len(api.since("closeForumTopic")) != 0 {
		t.Error("backend sessions have no Telegram topic to close")
	}
	var w Watch
	in.db.Where("bot_id = ?", b.ID).First(&w)
	if w.Enabled {
		t.Error("the watch survived the archive")
	}
	var pending int64
	in.db.Model(&Schedule{}).Where("bot_id = ? AND fired_at IS NULL", b.ID).Count(&pending)
	if pending != 0 {
		t.Error("a pending schedule survived the archive")
	}
}

func TestScheduleWakeupToolValidatesItsInput(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("sleeper", "")
	if err != nil {
		t.Fatal(err)
	}
	s := &mcpServer{db: in.db, config: in.cfg, botID: b.ID}

	if res, _, _ := s.scheduleWakeup(t.Context(), nil, scheduleIn{Note: "nothing"}); !res.IsError {
		t.Error("a wakeup with no time should be refused")
	}
	if res, _, _ := s.scheduleWakeup(t.Context(), nil, scheduleIn{At: "tomorrow", Note: "x"}); !res.IsError {
		t.Error("a non-RFC3339 time should be refused")
	}
	if res, _, _ := s.scheduleWakeup(t.Context(), nil, scheduleIn{Cron: "every day", Note: "x"}); !res.IsError {
		t.Error("an unparseable cron should be refused")
	}
	if res, _, _ := s.scheduleWakeup(t.Context(), nil, scheduleIn{InSeconds: 100 * 365 * 24 * 3600, Note: "x"}); !res.IsError {
		t.Error("a wakeup a century away should be refused")
	}

	res, _, err := s.scheduleWakeup(t.Context(), nil, scheduleIn{InSeconds: 3600, Note: "check the build"})
	if err != nil || res.IsError {
		t.Fatalf("a valid wakeup was refused: %+v", res)
	}
	rows, err := listSchedules(in.db, b.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("%d schedules stored, want 1 (%v)", len(rows), err)
	}

	// A bot can only cancel its own wakeups.
	other, err := in.createBot("other", "")
	if err != nil {
		t.Fatal(err)
	}
	otherServer := &mcpServer{db: in.db, config: in.cfg, botID: other.ID}
	if res, _, _ := otherServer.cancelSchedule(t.Context(), nil, cancelScheduleIn{ID: rows[0].ID}); res.IsError {
		t.Error("cancelling someone else's schedule should report 'not yours', not error")
	}
	if left, _ := listSchedules(in.db, b.ID); len(left) != 1 {
		t.Error("another bot cancelled this bot's schedule")
	}
}

func TestProjectRegistryRoundTrip(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("coder", "")
	if err != nil {
		t.Fatal(err)
	}
	s := &mcpServer{db: in.db, config: in.cfg, botID: b.ID}

	res, _, err := s.getProject(t.Context(), nil, getProjectIn{Path: "/srv/app"})
	if err != nil || res.IsError {
		t.Fatalf("get_project on an unknown path should not error: %+v", res)
	}

	if _, _, err := s.setProject(t.Context(), nil, setProjectIn{
		Path: "/srv/app", Name: "app", Stack: "Go + SQLite", DeployNotes: "systemd on vps3",
	}); err != nil {
		t.Fatal(err)
	}
	// A partial update must not blank the other fields.
	if _, _, err := s.setProject(t.Context(), nil, setProjectIn{Path: "/srv/app", Description: "the thing"}); err != nil {
		t.Fatal(err)
	}

	res, _, err = s.getProject(t.Context(), nil, getProjectIn{Path: "/srv/app"})
	if err != nil {
		t.Fatal(err)
	}
	body := toolText(res)
	for _, want := range []string{"Go + SQLite", "systemd on vps3", "the thing"} {
		if !strings.Contains(body, want) {
			t.Errorf("get_project lost %q:\n%s", want, body)
		}
	}
}

func seedEndedTurn(t *testing.T, db *gorm.DB, botID int64, ended time.Time) {
	t.Helper()
	started := ended.Add(-time.Minute)
	tr := Turn{
		BotID: botID, Source: sourceUser, Status: turnDone,
		StartedAt: &started, EndedAt: &ended, CreatedAt: ended,
	}
	if err := db.Create(&tr).Error; err != nil {
		t.Fatal(err)
	}
}

func TestIdleSessionIsRotatedAfterIdle(t *testing.T) {
	s, in, runner, api := testScheduler(t)
	b, err := in.createBot("analyst", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := in.db.Model(&Bot{}).Where("id = ?", b.ID).Updates(map[string]any{
		"session_id": "fat-session", "status": botIdle,
	}).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	seedEndedTurn(t, in.db, b.ID, now.Add(-2*time.Hour))

	s.compactIdleSessions(now)

	var got Bot
	in.db.First(&got, b.ID)
	if got.SessionID != "" {
		t.Errorf("session_id = %q, want empty after idle compact", got.SessionID)
	}
	if len(runner.enqueued) != 0 {
		t.Errorf("idle compact must not spend a turn: %+v", runner.enqueued)
	}
	texts := api.texts("")
	if strings.Contains(strings.Join(texts, "\n"), "Fresh conversation") {
		t.Errorf("backend idle compact must not spam General, got %q", texts)
	}
}

func TestIdleSessionStaysWhileRecent(t *testing.T) {
	s, in, _, _ := testScheduler(t)
	b, err := in.createBot("analyst", "")
	if err != nil {
		t.Fatal(err)
	}
	in.db.Model(&Bot{}).Where("id = ?", b.ID).Updates(map[string]any{
		"session_id": "keep-me", "status": botIdle,
	})
	now := time.Now()
	seedEndedTurn(t, in.db, b.ID, now.Add(-10*time.Minute))
	s.compactIdleSessions(now)
	var got Bot
	in.db.First(&got, b.ID)
	if got.SessionID != "keep-me" {
		t.Errorf("session_id = %q, want keep-me", got.SessionID)
	}
}

func TestIdleSessionStaysWhileRunningOrWaiting(t *testing.T) {
	s, in, _, _ := testScheduler(t)
	now := time.Now()
	for _, status := range []string{botRunning, botWaiting} {
		b, err := in.createBot("bot-"+status, "")
		if err != nil {
			t.Fatal(err)
		}
		in.db.Model(&Bot{}).Where("id = ?", b.ID).Updates(map[string]any{
			"session_id": "keep-" + status, "status": status,
		})
		seedEndedTurn(t, in.db, b.ID, now.Add(-3*time.Hour))
		s.compactIdleSessions(now)
		var got Bot
		in.db.First(&got, b.ID)
		if got.SessionID != "keep-"+status {
			t.Errorf("status %s: session_id = %q, want kept", status, got.SessionID)
		}
	}
}

func TestIdleSessionStaysForOpenQuestion(t *testing.T) {
	s, in, _, _ := testScheduler(t)
	b, err := in.createBot("asker", "")
	if err != nil {
		t.Fatal(err)
	}
	in.db.Model(&Bot{}).Where("id = ?", b.ID).Updates(map[string]any{
		"session_id": "keep-question", "status": botIdle,
	})
	now := time.Now()
	seedEndedTurn(t, in.db, b.ID, now.Add(-3*time.Hour))
	if err := in.db.Create(&Question{BotID: b.ID, Question: "ship it?"}).Error; err != nil {
		t.Fatal(err)
	}
	s.compactIdleSessions(now)
	var got Bot
	in.db.First(&got, b.ID)
	if got.SessionID != "keep-question" {
		t.Errorf("session_id = %q, want keep-question while ask_owner is open", got.SessionID)
	}
	r := &Runner{db: in.db, cfg: in.cfg}
	id, resume := r.sessionForTurn(&got)
	if id != "keep-question" || !resume {
		t.Errorf("sessionForTurn on a parked question = %q resume=%v", id, resume)
	}
}

func TestSessionForTurnRotatesAColdSession(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("analyst", "")
	if err != nil {
		t.Fatal(err)
	}
	in.db.Model(&Bot{}).Where("id = ?", b.ID).Update("session_id", "fat-session")
	seedEndedTurn(t, in.db, b.ID, time.Now().Add(-2*time.Hour))
	live, _ := botByID(in.db, b.ID)
	r := &Runner{db: in.db, cfg: in.cfg}
	id, resume := r.sessionForTurn(live)
	if resume || id == "fat-session" {
		t.Errorf("cold session was resumed: id=%q resume=%v", id, resume)
	}
}

func TestWatchExpiresAndWakesTheBot(t *testing.T) {
	s, in, runner, _ := testScheduler(t)
	b, err := in.createBot("watcher", "")
	if err != nil {
		t.Fatal(err)
	}
	w, err := upsertWatch(in.db, b.ID, "ci", "echo hi", 60)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.expireWatches(now)
	if len(runner.enqueued) != 0 {
		t.Fatalf("a fresh watch expired immediately: %+v", runner.enqueued)
	}
	s.expireWatches(now.Add(watchTTL(in.config()) + time.Minute))
	var n int64
	in.db.Model(&Watch{}).Where("id = ?", w.ID).Count(&n)
	if n != 0 {
		t.Error("the expired watch is still in the table")
	}
	if len(runner.enqueued) != 1 {
		t.Fatalf("expire enqueued %d turns, want 1", len(runner.enqueued))
	}
	got := runner.enqueued[0]
	if got.BotID != b.ID || got.Source != sourceSystem {
		t.Errorf("turn = %+v, want source=system on the bot that set the watch", got)
	}
	for _, want := range []string{`Watch "ci" timed out`, "echo hi", "set_routine"} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("expire input missing %q:\n%s", want, got.Text)
		}
	}
}

func TestWatchUpsertRenewsTheTTL(t *testing.T) {
	s, in, runner, _ := testScheduler(t)
	b, err := in.createBot("watcher", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upsertWatch(in.db, b.ID, "ci", "echo hi", 60); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	in.db.Model(&Watch{}).Where("bot_id = ?", b.ID).UpdateColumn("created_at", now.Add(-5*time.Hour))
	if _, err := upsertWatch(in.db, b.ID, "ci", "echo hi", 60); err != nil {
		t.Fatal(err)
	}
	s.expireWatches(now)
	var n int64
	in.db.Model(&Watch{}).Where("bot_id = ? AND name = ?", b.ID, "ci").Count(&n)
	if n != 1 {
		t.Error("renewing a watch must restart its TTL")
	}
	if len(runner.enqueued) != 0 {
		t.Errorf("a renewed watch was cancelled: %+v", runner.enqueued)
	}
}

func TestLegacyWatchWithZeroCreatedAtExpires(t *testing.T) {
	s, in, runner, _ := testScheduler(t)
	b, err := in.createBot("watcher", "")
	if err != nil {
		t.Fatal(err)
	}
	w, err := upsertWatch(in.db, b.ID, "old", "echo hi", 60)
	if err != nil {
		t.Fatal(err)
	}
	if err := in.db.Model(&Watch{}).Where("id = ?", w.ID).UpdateColumn("created_at", time.Time{}).Error; err != nil {
		t.Fatal(err)
	}
	s.expireWatches(time.Now())
	var n int64
	in.db.Model(&Watch{}).Where("id = ?", w.ID).Count(&n)
	if n != 0 {
		t.Error("a pre-TTL watch must expire on upgrade, not live forever")
	}
	if len(runner.enqueued) != 1 || runner.enqueued[0].Source != sourceSystem {
		t.Errorf("legacy expire should wake the bot: %+v", runner.enqueued)
	}
}

func TestWatchExpiryDoesNotWakeGeneral(t *testing.T) {
	s, in, runner, _ := testScheduler(t)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	w, err := upsertWatch(in.db, chief.ID, "ci", "echo hi", 60)
	if err != nil {
		t.Fatal(err)
	}
	in.db.Model(&Watch{}).Where("id = ?", w.ID).UpdateColumn("created_at", time.Now().Add(-5*time.Hour))
	s.expireWatches(time.Now())
	var n int64
	in.db.Model(&Watch{}).Where("id = ?", w.ID).Count(&n)
	if n != 0 {
		t.Error("the expired watch should still be deleted")
	}
	if len(runner.enqueued) != 0 {
		t.Errorf("watch TTL must not wake General: %+v", runner.enqueued)
	}
}

func TestRoutinesDoNotExpireWithWatches(t *testing.T) {
	s, in, runner, _ := testScheduler(t)
	b, err := in.createBot("sales", "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := upsertRoutine(in.db, b.ID, "morning", "mira ventas", "0 9 * * *", defaultRoutineTZ, now); err != nil {
		t.Fatal(err)
	}
	if _, err := upsertWatch(in.db, b.ID, "ci", "echo hi", 60); err != nil {
		t.Fatal(err)
	}
	in.db.Model(&Watch{}).Where("bot_id = ?", b.ID).UpdateColumn("created_at", now.Add(-5*time.Hour))
	s.expireWatches(now)
	var watches, schedules int64
	in.db.Model(&Watch{}).Where("bot_id = ?", b.ID).Count(&watches)
	in.db.Model(&Schedule{}).Where("bot_id = ? AND name = ?", b.ID, "morning").Count(&schedules)
	if watches != 0 {
		t.Error("the watch should have expired")
	}
	if schedules != 1 {
		t.Error("a named routine must survive watch expiry")
	}
	if len(runner.enqueued) != 1 || runner.enqueued[0].Source != sourceSystem {
		t.Errorf("only the watch should have woken the bot: %+v", runner.enqueued)
	}
}

func TestWatchTTLDisabledLeavesWatchesAlone(t *testing.T) {
	s, in, runner, _ := testScheduler(t)
	zero := 0
	in.cfg.WatchTTLS = &zero
	b, err := in.createBot("watcher", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upsertWatch(in.db, b.ID, "ci", "echo hi", 60); err != nil {
		t.Fatal(err)
	}
	in.db.Model(&Watch{}).Where("bot_id = ?", b.ID).UpdateColumn("created_at", time.Now().Add(-48*time.Hour))
	s.expireWatches(time.Now())
	var n int64
	in.db.Model(&Watch{}).Count(&n)
	if n != 1 {
		t.Error("watch_ttl_s=0 must disable expiry")
	}
	if len(runner.enqueued) != 0 {
		t.Errorf("disabled TTL still woke the bot: %+v", runner.enqueued)
	}
}

func TestWatchExpiryLabel(t *testing.T) {
	now := time.Now()
	w := Watch{Enabled: true, CreatedAt: now.Add(-time.Hour)}
	got := watchExpiryLabel(w, now, 4*time.Hour)
	if !strings.Contains(got, "expires in") {
		t.Errorf("label = %q, want remaining time", got)
	}
	if watchExpiryLabel(Watch{Enabled: false}, now, 4*time.Hour) != "disabled" {
		t.Error("disabled watches should say so")
	}
	if watchExpiryLabel(w, now, 0) != "no expiry" {
		t.Error("ttl 0 should say no expiry")
	}
}

func TestIdleRemindWakesGeneralEveryTenMinutes(t *testing.T) {
	s, in, runner, api := testScheduler(t)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	b, err := in.createBot("deployer", "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	seedEndedTurn(t, in.db, b.ID, now.Add(-15*time.Minute))

	s.remindIdleSessions(now)
	if n := len(runner.enqueued); n != 1 {
		t.Fatalf("want 1 General turn, got %d: %+v", n, runner.enqueued)
	}
	got := runner.enqueued[0]
	if got.BotID != chief.ID || got.Source != sourceBot {
		t.Errorf("turn = %+v, want source=bot on General", got)
	}
	if !strings.Contains(got.Text, "deployer") || !strings.Contains(got.Text, "ask_owner") {
		t.Errorf("General input missing the idle nag:\n%s", got.Text)
	}
	if !strings.Contains(got.Text, "Message from deployer") {
		t.Errorf("idle nag must look like a worker report:\n%s", got.Text)
	}
	assertIdleRemindNotOnTelegram(t, api)

	var inbox []InboxMessage
	in.db.Where("to_bot_id = ? AND from_bot_id = ?", chief.ID, b.ID).Find(&inbox)
	if len(inbox) != 1 || inbox[0].DeliveredAt == nil || inbox[0].TurnID == nil {
		t.Fatalf("want one delivered inbox row, got %+v", inbox)
	}

	s.remindIdleSessions(now.Add(time.Minute))
	if n := len(runner.enqueued); n != 1 {
		t.Errorf("reminded again after 1m (%d turns); cadence is 10m", n)
	}

	s.remindIdleSessions(now.Add(idleRemindInterval + time.Minute))
	if n := len(runner.enqueued); n != 2 {
		t.Errorf("after 10m more, want a second General turn, got %d", n)
	}
	assertIdleRemindNotOnTelegram(t, api)
}

func assertIdleRemindNotOnTelegram(t *testing.T, api *fakeBotAPI) {
	t.Helper()
	for _, c := range api.since("sendMessage") {
		text := c.Params.Get("text")
		if strings.Contains(text, "esperando") || strings.Contains(text, idleRemindText("deployer")) ||
			(strings.Contains(text, "Idle session") && strings.Contains(text, "ask_owner")) {
			t.Errorf("idle remind must not post to Telegram: %q", text)
		}
	}
}

func TestIdleRemindSkipsWorkingKeepaliveAndGeneral(t *testing.T) {
	s, in, runner, api := testScheduler(t)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	seedEndedTurn(t, in.db, chief.ID, now.Add(-time.Hour))

	running, err := in.createBot("running-one", "")
	if err != nil {
		t.Fatal(err)
	}
	in.db.Model(&Bot{}).Where("id = ?", running.ID).Update("status", botRunning)
	seedEndedTurn(t, in.db, running.ID, now.Add(-time.Hour))

	watched, err := in.createBot("watched", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upsertWatch(in.db, watched.ID, "ci", "echo hi", 60); err != nil {
		t.Fatal(err)
	}
	seedEndedTurn(t, in.db, watched.ID, now.Add(-time.Hour))

	queued, err := in.createBot("queued", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := in.db.Create(&Turn{BotID: queued.ID, Source: sourceUser, Input: "go", Status: turnQueued}).Error; err != nil {
		t.Fatal(err)
	}

	archived, err := in.createBot("old", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := archiveBotRow(in.db, archived.ID); err != nil {
		t.Fatal(err)
	}
	seedEndedTurn(t, in.db, archived.ID, now.Add(-time.Hour))

	s.remindIdleSessions(now)
	if n := len(runner.enqueued); n != 0 {
		t.Errorf("reminded sessions that are not waiting on the owner: %+v", runner.enqueued)
	}
	if n := len(api.texts("")); n != 0 {
		t.Errorf("idle remind leaked to Telegram: %q", api.texts(""))
	}
}

func TestIdleRemindStopsAfterUserActivity(t *testing.T) {
	s, in, runner, api := testScheduler(t)
	if _, err := in.ensureGeneralBot(); err != nil {
		t.Fatal(err)
	}
	b, err := in.createBot("analyst", "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	seedEndedTurn(t, in.db, b.ID, now.Add(-20*time.Minute))
	s.remindIdleSessions(now)
	if n := len(runner.enqueued); n != 1 {
		t.Fatalf("want the first General turn, got %d", n)
	}

	seedEndedTurn(t, in.db, b.ID, now.Add(time.Minute))
	s.remindIdleSessions(now.Add(2 * time.Minute))
	if n := len(runner.enqueued); n != 1 {
		t.Errorf("a fresh user turn must stop the reminders, got %d", n)
	}
	assertIdleRemindNotOnTelegram(t, api)
}

func TestIdleRemindSkipsWaitingAskOwner(t *testing.T) {
	s, in, runner, api := testScheduler(t)
	if _, err := in.ensureGeneralBot(); err != nil {
		t.Fatal(err)
	}
	b, err := in.createBot("asker", "")
	if err != nil {
		t.Fatal(err)
	}
	in.db.Model(&Bot{}).Where("id = ?", b.ID).Update("status", botWaiting)
	now := time.Now()
	seedEndedTurn(t, in.db, b.ID, now.Add(-12*time.Minute))
	if err := in.db.Create(&Question{BotID: b.ID, Question: "ship it?"}).Error; err != nil {
		t.Fatal(err)
	}
	s.remindIdleSessions(now)
	if n := len(runner.enqueued); n != 0 {
		t.Errorf("a parked ask_owner must not nag General, got %d: %+v", n, runner.enqueued)
	}
	assertIdleRemindNotOnTelegram(t, api)
}

func TestIdleRemindSkipsBackgroundAndSchedule(t *testing.T) {
	s, in, runner, api := testScheduler(t)
	now := time.Now()

	bg, err := in.createBot("builder", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := in.db.Create(&BackgroundJob{BotID: bg.ID, Name: "build", Status: jobRunning, Kind: jobKindShell, Command: "sleep 9"}).Error; err != nil {
		t.Fatal(err)
	}
	seedEndedTurn(t, in.db, bg.ID, now.Add(-time.Hour))

	sched, err := in.createBot("cronned", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := in.db.Create(&Schedule{BotID: sched.ID, Note: "later", FireAt: now.Add(time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	seedEndedTurn(t, in.db, sched.ID, now.Add(-time.Hour))

	s.remindIdleSessions(now)
	if n := len(runner.enqueued); n != 0 {
		t.Errorf("keepalive sessions were reminded: %+v", runner.enqueued)
	}
	if n := len(api.texts("")); n != 0 {
		t.Errorf("idle remind leaked to Telegram: %q", api.texts(""))
	}
}

// noopTestUI is a botUI that does nothing, for tests that build a real Runner
// only to exercise its database side.
type noopTestUI struct{}

func (noopTestUI) Post(int64, string) (int64, error) { return 0, nil }
func (noopTestUI) Edit(int64, int64, string) error   { return nil }
func (noopTestUI) React(int64, string)               {}
