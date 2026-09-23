package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStopDuringFailoverIsRemembered(t *testing.T) {
	r := newRunner(nil, &Config{}, &fakeUI{})
	if r.interruptActive(7, false) {
		t.Fatal("nothing was running to kill")
	}
	if !r.turnHalted(7) {
		t.Fatal("/stop must stick while no process is registered")
	}
	start := time.Now()
	if !r.sleepUnlessHalted(7, time.Second) {
		t.Fatal("the failover sleep must notice /stop")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("sleep waited out the whole gap")
	}
	r.clearHalt(7)
	if r.turnHalted(7) {
		t.Fatal("halt flag leaked")
	}
}

func TestUnknownUsageRanksBehindKnown(t *testing.T) {
	now := time.Now()
	stats := []profileStat{
		{Name: "busy", FiveHour: 90, Usage: profileUsage{FiveHourKnown: true}},
		{Name: "cold", FiveHour: 0},
	}
	if got := chooseProfile(spawnStatsAssumeIdleUnknown(stats), now); got != "busy" {
		t.Fatalf("chooseProfile = %q, want the account with real usage", got)
	}
}

func TestUnloggedFileEngineIsNotSpawnable(t *testing.T) {
	p := Profile{Name: "g", Engine: engineGrok, ConfigDir: t.TempDir()}
	if profileSpawnEligible(p) {
		t.Fatal("grok with no auth.json must not be selected")
	}
}

func TestLimitCooldownUsesTheFullWindow(t *testing.T) {
	cooldownMu.Lock()
	cooldowns = map[string]time.Time{}
	cooldownMu.Unlock()
	t.Cleanup(func() {
		cooldownMu.Lock()
		cooldowns = map[string]time.Time{}
		cooldownMu.Unlock()
	})

	now := time.Now()
	p := newFixtureProfile(t, "weekly", `{}`, "")
	usageMemPut(p, profileUsage{
		FiveHour: 5, FiveHourKnown: true, FiveHourResetAt: now.Add(40 * time.Minute),
		SevenDay: 100, SevenDayKnown: true, SevenDayResetAt: now.Add(72 * time.Hour),
	})
	noteProfileLimit(p, now)
	until := profileCooledUntil(p.Name, now)
	if until.Sub(now) < 70*time.Hour {
		t.Fatalf("cooldown = %s, want the weekly reset", until.Sub(now))
	}
}

func TestFoldKeepsUserSemanticsWithoutCap(t *testing.T) {
	in, _, _ := testInstance(t)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	if err := in.db.Create(&Turn{BotID: chief.ID, Source: sourceBot, Input: "Message from worker:\ndone", Status: turnQueued}).Error; err != nil {
		t.Fatal(err)
	}
	if err := in.db.Create(&Turn{BotID: chief.ID, Source: sourceUser, Input: "ship it", Status: turnQueued}).Error; err != nil {
		t.Fatal(err)
	}
	head, input, _, ok := foldQueue(in.db, chief.ID)
	if !ok {
		t.Fatal("fold")
	}
	if head.Source != sourceUser {
		t.Fatalf("source = %q, want user", head.Source)
	}
	if !head.FoldNoCap {
		t.Fatal("a folded report must drop the 60s cap")
	}
	if !strings.Contains(input, "ship it") {
		t.Fatalf("input = %q", input)
	}
	if d := chiefTimeoutFor(chief, head); d != 0 {
		t.Fatalf("timeout = %s, want none", d)
	}
}

func TestLastOwnerRequestIgnoresLaterQueued(t *testing.T) {
	in, _, _ := testInstance(t)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	orig := &Turn{BotID: chief.ID, Source: sourceUser, Input: "ship the fix", Status: turnFailed}
	if err := in.db.Create(orig).Error; err != nil {
		t.Fatal(err)
	}
	follow := &Turn{BotID: chief.ID, Source: sourceSystem, Input: chiefTimeoutInput(), Status: turnDone}
	if err := in.db.Create(follow).Error; err != nil {
		t.Fatal(err)
	}
	thanks := &Turn{BotID: chief.ID, Source: sourceUser, Input: "gracias", Status: turnQueued}
	if err := in.db.Create(thanks).Error; err != nil {
		t.Fatal(err)
	}
	if got := lastOwnerRequest(in.db, chief.ID, follow); got != "ship the fix" {
		t.Fatalf("lastOwnerRequest = %q", got)
	}
}

func TestPartialHandoffDoesNotCount(t *testing.T) {
	in, _, _ := testInstance(t)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	since := time.Now().Add(-time.Second)
	owner := "deploy fecha and write the post"
	msg := InboxMessage{ToBotID: chief.ID, FromBotID: &chief.ID, Text: "please do the deploy only", Wake: true}
	if err := in.db.Create(&msg).Error; err != nil {
		t.Fatal(err)
	}
	if chiefHandedOffSince(in.db, chief.ID, since, owner) {
		t.Fatal("a partial tell must not count as handing off the request")
	}
	full := InboxMessage{ToBotID: chief.ID, FromBotID: &chief.ID, Text: "worker prompt\n" + owner, Wake: true}
	if err := in.db.Create(&full).Error; err != nil {
		t.Fatal(err)
	}
	if !chiefHandedOffSince(in.db, chief.ID, since, owner) {
		t.Fatal("a delegation that carries the owner text must count")
	}
}

func TestRedactBeforeTruncate(t *testing.T) {
	secret := "SUPERSECRETVALUE123456"
	raw := strings.Repeat("a", backgroundOutputLimit-4) + secret + "tail"
	out, err := redactJobOutput(raw, []string{secret})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "SUPER") {
		t.Fatal("secret prefix survived the clip")
	}
}

func TestRedactFailsClosedWhenVaultUnreadable(t *testing.T) {
	isolateConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(secretsPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := redactVaultOutput("TOKEN=raw", nil); err == nil {
		t.Fatal("unreadable vault must withhold output")
	}
}

func TestRunTimeoutIsReported(t *testing.T) {
	isolateConfigEnv(t)
	t.Setenv("HOME", t.TempDir())
	if err := putSecret("tok", vaultTestValue); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	exit, output, err := runWithSecrets(ctx, &Config{}, t.TempDir(), "sleep 30", map[string]string{"TOKEN": "tok"}, "")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("exit=%d err=%v output=%q", exit, err, output)
	}
	assertNoSecret(t, output, "timeout output")
	assertNoSecret(t, err.Error(), "timeout error")
}

func TestRunKillsBackgroundChildren(t *testing.T) {
	isolateConfigEnv(t)
	t.Setenv("HOME", t.TempDir())
	if err := putSecret("tok", vaultTestValue); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	var err error
	go func() {
		_, _, err = runWithSecrets(ctx, &Config{}, t.TempDir(), "sleep 30 &", map[string]string{"TOKEN": "tok"}, "")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("run did not return; the process group was not killed")
	}
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v", err)
	}
}

func TestPanelTransientEditDoesNotRepost(t *testing.T) {
	in, _, _ := testInstance(t)
	ui := &fakeUI{editErr: errors.New("Post \"https://api.telegram.org\": timeout")}
	p := newSessionPanel(in.db, ui)
	p.msgID = 5
	w, err := in.createBot("deployer", "")
	if err != nil {
		t.Fatal(err)
	}
	setBotStatus(in.db, w.ID, botRunning)
	p.setActivity(w.ID, "thinking")
	if len(ui.posts) != 0 {
		t.Fatalf("transient edit posted a new card: %v", ui.posts)
	}
	if p.msgID != 5 {
		t.Fatalf("msgID = %d, want the original card kept", p.msgID)
	}
}

func TestSendFileRejectsDataDirViaSymlink(t *testing.T) {
	root := t.TempDir()
	cfg := &Config{DataDir: filepath.Join(root, "data")}
	logPath := filepath.Join(cfg.DataDir, "background", "12", "out.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "notes.txt")
	if err := os.Symlink(logPath, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sendFileForbidden(cfg, resolved); !ok {
		t.Fatal("symlink into the data dir must be refused")
	}
}

func TestArchiveDropsQueuedTurns(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("gone", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := in.db.Create(&Turn{BotID: b.ID, Source: sourceUser, Input: "hi", Status: turnQueued}).Error; err != nil {
		t.Fatal(err)
	}
	if err := archiveBotRow(in.db, b.ID); err != nil {
		t.Fatal(err)
	}
	var got Turn
	if err := in.db.Where("bot_id = ?", b.ID).First(&got).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != turnFailed || got.StopReason != "session archived" {
		t.Fatalf("turn = %+v", got)
	}
	r := newRunner(in.db, in.cfg, &fakeUI{})
	if _, err := r.Enqueue(b.ID, sourceUser, "late", 0); err == nil {
		t.Fatal("enqueue onto an archived session must fail")
	}
}

func TestRoutineSkipsWhilePreviousFireRuns(t *testing.T) {
	s, in, _, _ := testScheduler(t)
	owner, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	worker, err := in.createBot("routine-hourly", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := in.db.Create(&Turn{BotID: worker.ID, Source: sourceRoutine, Input: "still going", Status: turnRunning}).Error; err != nil {
		t.Fatal(err)
	}
	_, err = s.routineWorkerBot(owner, "routine-hourly")
	if !errors.Is(err, errRoutineBusy) {
		t.Fatalf("err = %v, want busy", err)
	}
	var fresh Bot
	if err := in.db.First(&fresh, worker.ID).Error; err != nil {
		t.Fatal(err)
	}
	if fresh.Status == botIdle && fresh.SessionID == "" && worker.SessionID != "" {
		t.Fatal("busy routine reset the live session")
	}
}

func TestWatchWriteRequiresTheSameCommand(t *testing.T) {
	s, in, runner, _ := testScheduler(t)
	b, err := in.createBot("watcher", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upsertWatch(in.db, b.ID, "x", "echo one", 60); err != nil {
		t.Fatal(err)
	}
	s.runDueWatches(time.Now())
	var snap Watch
	if err := in.db.Where("bot_id = ?", b.ID).First(&snap).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := upsertWatch(in.db, b.ID, "x", "echo two", 60); err != nil {
		t.Fatal(err)
	}
	s.runWatch(&snap, time.Now())
	if len(runner.enqueued) != 0 {
		t.Fatalf("stale watch run enqueued %+v", runner.enqueued)
	}
}

func TestDeviceLoginDoesNotConsumeTheNextDM(t *testing.T) {
	in, _, _ := testInstance(t)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	in.login.waiting = &loginWaiter{
		chatID: 1, topicID: 0, codes: make(chan string, 1), cancel: cancel,
	}
	if in.takeLoginCode(1, 0, "deploy staging") {
		t.Fatal("device login ate a normal message")
	}
	in.login.mu.Lock()
	in.login.waiting.acceptsCode = true
	in.login.mu.Unlock()
	if !in.takeLoginCode(1, 0, "123456") {
		t.Fatal("AskForCode must consume the code")
	}
}

func TestRenameMovesCooldownAndNeedsLogin(t *testing.T) {
	cooldownMu.Lock()
	cooldowns = map[string]time.Time{}
	cooldownMu.Unlock()
	until := time.Now().Add(time.Hour)
	cooldowns["default"] = until
	renameProfileCooldown("default", "me@example.com")
	if _, ok := cooldowns["default"]; ok {
		t.Fatal("old cooldown key remained")
	}
	if !cooldowns["me@example.com"].Equal(until) {
		t.Fatal("cooldown did not follow the email")
	}
	r := newRunner(nil, &Config{}, &fakeUI{})
	r.needsLogin["default"] = true
	r.renameNeedsLogin("default", "me@example.com")
	if r.needsLogin["default"] || !r.needsLogin["me@example.com"] {
		t.Fatalf("needsLogin = %+v", r.needsLogin)
	}
}
