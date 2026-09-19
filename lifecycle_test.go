package main

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func sendContaining(api *fakeBotAPI, substr string) (apiCall, bool) {
	for _, c := range api.since("sendMessage") {
		if strings.Contains(c.Params.Get("text"), substr) {
			return c, true
		}
	}
	return apiCall{}, false
}

// waitSendContaining waits for a sendMessage that contains substr. Job
// finish writes the row, then notifies; tests that only wait on the row
// otherwise lose the race.
func waitSendContaining(t *testing.T, api *fakeBotAPI, substr string) apiCall {
	t.Helper()
	var got apiCall
	waitUntil(t, 2*time.Second, func() bool {
		c, ok := sendContaining(api, substr)
		if ok {
			got = c
		}
		return ok
	})
	return got
}

func TestRecoverAfterRestartPingsEvenWhenIdle(t *testing.T) {
	_, in, _, api := testScheduler(t)
	in.recoverAfterRestart()

	boot, ok := sendContaining(api, "ccc is back")
	if !ok {
		t.Fatalf("want a boot ping, got %v", api.texts(""))
	}
	if boot.Params.Get("disable_notification") == "true" {
		t.Fatal("the boot ping must notify")
	}
	if boot.Params.Get("message_thread_id") != "" {
		t.Fatal("the boot ping belongs in General")
	}
}

func TestRecoverAfterRestartReportsInterruptedTurns(t *testing.T) {
	_, in, _, api := testScheduler(t)
	b, err := in.createBot("dev", "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := in.db.Create(&Turn{
		BotID: b.ID, Source: sourceUser, Input: "ship the fix",
		Status: turnRunning, StartedAt: &now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := in.db.Model(&Bot{}).Where("id = ?", b.ID).Update("status", botRunning).Error; err != nil {
		t.Fatal(err)
	}

	in.recoverAfterRestart()

	var turn Turn
	if err := in.db.Where("bot_id = ?", b.ID).First(&turn).Error; err != nil {
		t.Fatal(err)
	}
	if turn.Status != turnQueued {
		t.Fatalf("interrupted turn = status=%q, want queued so it is retried", turn.Status)
	}
	if turn.StartedAt != nil {
		t.Errorf("requeued turn still has started_at=%v", turn.StartedAt)
	}
	var bot Bot
	in.db.First(&bot, b.ID)
	if bot.Status != botIdle {
		t.Errorf("bot status = %q, want idle", bot.Status)
	}

	if _, ok := sendContaining(api, "ccc is back"); !ok {
		t.Error("missing boot ping")
	}
	got, ok := sendContaining(api, "Retrying")
	if !ok {
		t.Fatalf("missing retry ping, texts=%v", api.texts(""))
	}
	if got.Params.Get("disable_notification") == "true" {
		t.Fatal("the retry ping must notify")
	}
	if got.Params.Get("message_thread_id") != "" {
		t.Errorf("retry ping thread = %q, want General (the DM)", got.Params.Get("message_thread_id"))
	}
	if !strings.Contains(got.Params.Get("text"), "dev") {
		t.Errorf("retry ping should name the session: %s", got.Params.Get("text"))
	}
	if strings.Contains(got.Params.Get("text"), "ship the fix") {
		t.Errorf("retry ping must not dump the turn input: %s", got.Params.Get("text"))
	}
}

func TestRecoverAfterRestartDoesNotRetryOldFailures(t *testing.T) {
	_, in, _, api := testScheduler(t)
	b, err := in.createBot("dev", "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := Turn{
		BotID: b.ID, Source: sourceUser, Input: "already dead",
		Status: turnFailed, StopReason: "fatal", ErrorClass: errFatal,
		StartedAt: &now, EndedAt: &now,
	}
	if err := in.db.Create(&old).Error; err != nil {
		t.Fatal(err)
	}

	in.recoverAfterRestart()

	var got Turn
	if err := in.db.First(&got, old.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != turnFailed || got.StopReason != "fatal" {
		t.Fatalf("historical failure was touched: status=%q reason=%q", got.Status, got.StopReason)
	}
	if _, ok := sendContaining(api, "Retrying"); ok {
		t.Fatalf("old failures must not be retried, texts=%v", api.texts(""))
	}
}

func TestRecoverAfterRestartKeepsInterruptedTurnOldest(t *testing.T) {
	_, in, _, _ := testScheduler(t)
	b, err := in.createBot("dev", "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	running := Turn{BotID: b.ID, Source: sourceUser, Input: "first", Status: turnRunning, StartedAt: &now}
	if err := in.db.Create(&running).Error; err != nil {
		t.Fatal(err)
	}
	queued := Turn{BotID: b.ID, Source: sourceUser, Input: "arrived later", Status: turnQueued}
	if err := in.db.Create(&queued).Error; err != nil {
		t.Fatal(err)
	}

	in.recoverAfterRestart()

	head, input, _, ok := foldQueue(in.db, b.ID)
	if !ok {
		t.Fatal("expected a queued turn after recover")
	}
	if head.ID != running.ID {
		t.Fatalf("retried turn id=%d, want the original interrupted id=%d (must stay oldest)", head.ID, running.ID)
	}
	if !strings.Contains(input, "first") || !strings.Contains(input, "arrived later") {
		t.Errorf("folded input = %q", input)
	}
	if !strings.HasPrefix(input, "first") {
		t.Errorf("interrupted input must come first, got %q", input)
	}
}

func TestRecoverAfterRestartResumesLiveBackgroundJob(t *testing.T) {
	_, in, runner, api := testScheduler(t)
	b, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if in.sched != nil {
			in.sched.Close()
		}
		killProcessGroup(cmd.Process.Pid)
		_ = cmd.Wait()
	})
	now := time.Now()
	job := BackgroundJob{
		BotID: b.ID, Name: "still-going", Status: jobRunning,
		Kind: jobKindShell, Command: "sleep 30", StartedAt: &now, PID: cmd.Process.Pid,
	}
	if err := in.db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}

	in.recoverAfterRestart()
	time.Sleep(150 * time.Millisecond)

	in.db.First(&job, job.ID)
	if job.Status != jobRunning {
		t.Fatalf("live job marked %q error=%q", job.Status, job.Error)
	}
	if queued := runner.queued(); len(queued) != 0 {
		t.Fatalf("resuming a live job must not wake the bot: %+v", queued)
	}
	got, ok := sendContaining(api, "Resumed")
	if !ok {
		t.Fatalf("missing resume ping, texts=%v", api.texts(""))
	}
	if got.Params.Get("disable_notification") == "true" {
		t.Fatal("the resume ping must notify")
	}
	if !strings.Contains(got.Params.Get("text"), "still-going") {
		t.Errorf("resume ping missing the job name: %s", got.Params.Get("text"))
	}
	if got.Params.Get("message_thread_id") != "" {
		t.Errorf("resume ping thread = %q, want General (the DM)", got.Params.Get("message_thread_id"))
	}
}
