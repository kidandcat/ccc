package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func waitJob(t *testing.T, in *instance, id int64) BackgroundJob {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last BackgroundJob
	for time.Now().Before(deadline) {
		if err := in.db.First(&last, id).Error; err == nil {
			if last.Status == jobDone || last.Status == jobFailed {
				return last
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %d did not finish (last status %q)", id, last.Status)
	return last
}

func TestRunBackgroundQueuesWithoutBlockingTheTurn(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := in.db.Create(&Turn{BotID: b.ID, Source: sourceUser, Input: "please build", Status: turnQueued}).Error; err != nil {
		t.Fatal(err)
	}
	s := &mcpServer{db: in.db, config: in.cfg, botID: b.ID, turnID: 99}
	res, _, err := s.runBackground(t.Context(), nil, runBackgroundIn{Name: "go test", Command: "echo ok"})
	if err != nil || res.IsError {
		t.Fatalf("run_background: %+v %v", res, err)
	}
	body := toolText(res)
	if !strings.Contains(body, "job #") {
		t.Errorf("tool result missing job id: %s", body)
	}

	var job BackgroundJob
	if err := in.db.Where("bot_id = ?", b.ID).First(&job).Error; err != nil {
		t.Fatal(err)
	}
	if job.Status != jobQueued || job.Kind != jobKindShell || job.Command != "echo ok" {
		t.Errorf("queued job = %+v", job)
	}
	if job.CreatedByTurnID == nil || *job.CreatedByTurnID != 99 {
		t.Errorf("created_by_turn_id = %v, want 99", job.CreatedByTurnID)
	}

	var userTurn Turn
	if err := in.db.Where("bot_id = ? AND source = ?", b.ID, sourceUser).First(&userTurn).Error; err != nil {
		t.Fatal(err)
	}
	if userTurn.Status != turnQueued {
		t.Errorf("the conversational turn became %q; a background job must not hold turns.status=running", userTurn.Status)
	}
}

func TestListAndGetBackground(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}
	s := &mcpServer{db: in.db, config: in.cfg, botID: b.ID}
	if res, _, _ := s.runBackground(t.Context(), nil, runBackgroundIn{Name: "one", Command: "true"}); res.IsError {
		t.Fatal(toolText(res))
	}
	if res, _, _ := s.listBackground(t.Context(), nil, emptyIn{}); !strings.Contains(toolText(res), "#") {
		t.Errorf("list_background: %s", toolText(res))
	}
	var job BackgroundJob
	in.db.Where("bot_id = ?", b.ID).First(&job)
	res, _, err := s.getBackground(t.Context(), nil, backgroundIDIn{ID: job.ID})
	if err != nil || res.IsError {
		t.Fatalf("get_background: %+v %v", res, err)
	}
	if !strings.Contains(toolText(res), "one") {
		t.Errorf("get_background missing name: %s", toolText(res))
	}
	if res, _, _ := s.getBackground(t.Context(), nil, backgroundIDIn{ID: 99999}); !res.IsError {
		t.Error("get_background of an unknown id should be a tool error")
	}
}

func TestCancelQueuedBackground(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}
	s := &mcpServer{db: in.db, config: in.cfg, botID: b.ID}
	if res, _, _ := s.runBackground(t.Context(), nil, runBackgroundIn{Command: "sleep 30"}); res.IsError {
		t.Fatal(toolText(res))
	}
	var job BackgroundJob
	in.db.Where("bot_id = ?", b.ID).First(&job)
	res, _, err := s.cancelBackground(t.Context(), nil, backgroundIDIn{ID: job.ID})
	if err != nil || res.IsError {
		t.Fatalf("cancel: %+v %v", res, err)
	}
	in.db.First(&job, job.ID)
	if job.Status != jobFailed || job.Error != "cancelled" {
		t.Errorf("cancelled job = %+v", job)
	}
}

func TestBackgroundJobWakesTheBotOnSuccess(t *testing.T) {
	s, in, runner, _ := testScheduler(t)
	b, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}
	job, err := queueBackgroundJob(in.db, b.ID, 0, "hello", "echo hello-from-bg")
	if err != nil {
		t.Fatal(err)
	}
	s.tickBackground()
	got := waitJob(t, in, job.ID)
	if got.Status != jobDone {
		t.Fatalf("status = %q error=%q output=%q", got.Status, got.Error, got.Output)
	}
	if !strings.Contains(got.Output, "hello-from-bg") {
		t.Errorf("output = %q", got.Output)
	}
	if len(runner.enqueued) != 1 {
		t.Fatalf("enqueued %d, want 1 wakeup: %+v", len(runner.enqueued), runner.enqueued)
	}
	wake := runner.enqueued[0]
	if wake.BotID != b.ID || wake.Source != sourceBackground {
		t.Errorf("wakeup = %+v", wake)
	}
	for _, want := range []string{`Background job #`, `"hello"`, "finished", "Exit code: 0", "hello-from-bg"} {
		if !strings.Contains(wake.Text, want) {
			t.Errorf("wakeup missing %q:\n%s", want, wake.Text)
		}
	}

	var userTurns int64
	in.db.Model(&Turn{}).Where("bot_id = ? AND source = ? AND status = ?", b.ID, sourceUser, turnRunning).Count(&userTurns)
	if userTurns != 0 {
		t.Error("a background job must not mark a conversational turn running")
	}
}

func TestBackgroundJobWakesTheBotOnFailure(t *testing.T) {
	s, in, runner, _ := testScheduler(t)
	b, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}
	job, err := queueBackgroundJob(in.db, b.ID, 0, "boom", "echo nope >&2; exit 7")
	if err != nil {
		t.Fatal(err)
	}
	s.tickBackground()
	got := waitJob(t, in, job.ID)
	if got.Status != jobFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if got.ExitCode == nil || *got.ExitCode != 7 {
		t.Errorf("exit = %v, want 7", got.ExitCode)
	}
	if len(runner.enqueued) != 1 || runner.enqueued[0].Source != sourceBackground {
		t.Fatalf("wakeup = %+v", runner.enqueued)
	}
	if !strings.Contains(runner.enqueued[0].Text, "failed") || !strings.Contains(runner.enqueued[0].Text, "Exit code: 7") {
		t.Errorf("failure wakeup:\n%s", runner.enqueued[0].Text)
	}
}

func TestCancelRunningBackground(t *testing.T) {
	s, in, runner, _ := testScheduler(t)
	b, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}
	job, err := queueBackgroundJob(in.db, b.ID, 0, "sleep", "sleep 30")
	if err != nil {
		t.Fatal(err)
	}
	s.tickBackground()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		in.db.First(job, job.ID)
		if job.Status == jobRunning && job.PID > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if job.Status != jobRunning {
		t.Fatalf("job never started: %+v", job)
	}
	msg, err := cancelBackgroundJob(in.db, b.ID, job.ID, killProcessGroup)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "cancel") {
		t.Errorf("cancel said %q", msg)
	}
	s.tickBackground()
	got := waitJob(t, in, job.ID)
	if got.Status != jobFailed {
		t.Errorf("cancelled job status = %q error=%q", got.Status, got.Error)
	}
	if len(runner.enqueued) != 1 || runner.enqueued[0].Source != sourceBackground {
		t.Errorf("expected a failed wakeup, got %+v", runner.enqueued)
	}
}

func writeJobArtifacts(t *testing.T, cfg *Config, id int64, output string, exit *int) {
	t.Helper()
	dir := backgroundJobDir(cfg, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, bgLogFile), []byte(output), 0o644); err != nil {
		t.Fatal(err)
	}
	if exit != nil {
		if err := os.WriteFile(filepath.Join(dir, bgExitFile), []byte(strconv.Itoa(*exit)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReattachFinishedWhileDownWakesTheBot(t *testing.T) {
	s, in, runner, _ := testScheduler(t)
	b, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	exit := 0
	job := BackgroundJob{BotID: b.ID, Name: "while-down", Status: jobRunning, Kind: jobKindShell, Command: "echo hi", StartedAt: &now, PID: 999999}
	if err := in.db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	writeJobArtifacts(t, in.config(), job.ID, "hello-from-disk\n", &exit)
	s.reattachBackgroundJobs()
	got := waitJob(t, in, job.ID)
	if got.Status != jobDone {
		t.Fatalf("status = %q error=%q output=%q", got.Status, got.Error, got.Output)
	}
	if !strings.Contains(got.Output, "hello-from-disk") {
		t.Errorf("output = %q", got.Output)
	}
	if len(runner.enqueued) != 1 || runner.enqueued[0].Source != sourceBackground {
		t.Fatalf("wakeup = %+v", runner.enqueued)
	}
	if !strings.Contains(runner.enqueued[0].Text, "finished") || !strings.Contains(runner.enqueued[0].Text, "hello-from-disk") {
		t.Errorf("wakeup:\n%s", runner.enqueued[0].Text)
	}
}

func TestReattachDeadPIDWithoutExitFileFailsAndWakes(t *testing.T) {
	s, in, runner, _ := testScheduler(t)
	b, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	job := BackgroundJob{BotID: b.ID, Name: "lost", Status: jobRunning, Kind: jobKindShell, Command: "true", StartedAt: &now, PID: 999999}
	if err := in.db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	s.reattachBackgroundJobs()
	got := waitJob(t, in, job.ID)
	if got.Status != jobFailed || !strings.Contains(got.Error, "without writing an exit status") {
		t.Errorf("dead orphan = %+v", got)
	}
	if len(runner.enqueued) != 1 || runner.enqueued[0].Source != sourceBackground {
		t.Fatalf("orphan wakeup = %+v", runner.enqueued)
	}
}

func TestReattachAlivePIDKeepsRunning(t *testing.T) {
	s, in, runner, _ := testScheduler(t)
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
		s.Close()
		killProcessGroup(cmd.Process.Pid)
		_ = cmd.Wait()
	})
	now := time.Now()
	job := BackgroundJob{BotID: b.ID, Name: "still-going", Status: jobRunning, Kind: jobKindShell, Command: "sleep 30", StartedAt: &now, PID: cmd.Process.Pid}
	if err := in.db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	s.reattachBackgroundJobs()
	time.Sleep(250 * time.Millisecond)
	in.db.First(&job, job.ID)
	if job.Status != jobRunning {
		t.Fatalf("alive reattach marked the job %q error=%q", job.Status, job.Error)
	}
	if len(runner.enqueued) != 0 {
		t.Fatalf("alive reattach woke the bot early: %+v", runner.enqueued)
	}
	listed, err := listBackgroundJobs(in.db, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(formatBackgroundList(listed), "[running]") {
		t.Errorf("list_background after reattach: %s", formatBackgroundList(listed))
	}
}

func TestBackgroundJobSurvivesSchedulerRestart(t *testing.T) {
	s1, in, runner, _ := testScheduler(t)
	b, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}
	flag := filepath.Join(t.TempDir(), "go")
	job, err := queueBackgroundJob(in.db, b.ID, 0, "hold",
		fmt.Sprintf("while [ ! -f %s ]; do sleep 0.05; done; echo survived", strconv.Quote(flag)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		var latest BackgroundJob
		if in.db.First(&latest, job.ID).Error == nil && latest.PID > 0 {
			killProcessGroup(latest.PID)
		}
	})
	s1.tickBackground()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		in.db.First(job, job.ID)
		if job.Status == jobRunning && job.PID > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if job.Status != jobRunning || job.PID <= 0 {
		t.Fatalf("job never started: %+v", job)
	}
	s1.Close()
	if !processAlive(job.PID) {
		t.Fatal("Close() killed the background job")
	}
	in.db.First(job, job.ID)
	if job.Status != jobRunning {
		t.Fatalf("Close() changed status to %q", job.Status)
	}

	s2 := newScheduler(in)
	in.sched = s2
	t.Cleanup(func() { s2.Close() })
	s2.reattachBackgroundJobs()
	time.Sleep(150 * time.Millisecond)
	in.db.First(job, job.ID)
	if job.Status != jobRunning {
		t.Fatalf("reattach changed status to %q error=%q", job.Status, job.Error)
	}
	if len(runner.enqueued) != 0 {
		t.Fatalf("reattach of a live job woke the bot: %+v", runner.enqueued)
	}
	if err := os.WriteFile(flag, []byte("go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := waitJob(t, in, job.ID)
	if got.Status != jobDone {
		t.Fatalf("after restart: status=%q error=%q output=%q", got.Status, got.Error, got.Output)
	}
	if !strings.Contains(got.Output, "survived") {
		t.Errorf("output = %q", got.Output)
	}
	if len(runner.enqueued) != 1 || runner.enqueued[0].Source != sourceBackground {
		t.Fatalf("expected the normal background wakeup, got %+v", runner.enqueued)
	}
}

func TestCancelAfterReattach(t *testing.T) {
	s1, in, runner, _ := testScheduler(t)
	b, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}
	job, err := queueBackgroundJob(in.db, b.ID, 0, "sleep", "sleep 30")
	if err != nil {
		t.Fatal(err)
	}
	s1.tickBackground()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		in.db.First(job, job.ID)
		if job.Status == jobRunning && job.PID > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if job.Status != jobRunning {
		t.Fatalf("job never started: %+v", job)
	}
	s1.Close()
	s2 := newScheduler(in)
	in.sched = s2
	t.Cleanup(func() { s2.Close() })
	s2.reattachBackgroundJobs()
	msg, err := cancelBackgroundJob(in.db, b.ID, job.ID, killProcessGroup)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "cancel") {
		t.Errorf("cancel said %q", msg)
	}
	s2.tickBackground()
	got := waitJob(t, in, job.ID)
	if got.Status != jobFailed || !strings.Contains(got.Error, "cancel") {
		t.Errorf("cancelled after reattach = %+v", got)
	}
	if len(runner.enqueued) != 1 || runner.enqueued[0].Source != sourceBackground {
		t.Errorf("expected a failed wakeup, got %+v", runner.enqueued)
	}
}

func TestRunBackgroundRejectsEmptyCommand(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}
	s := &mcpServer{db: in.db, config: in.cfg, botID: b.ID}
	if res, _, _ := s.runBackground(t.Context(), nil, runBackgroundIn{Name: "x"}); !res.IsError {
		t.Error("empty command should be refused")
	}
}

func TestBackgroundJobScopedToTheCallingBot(t *testing.T) {
	in, _, _ := testInstance(t)
	a, _ := in.createBot("a", "")
	bb, _ := in.createBot("b", "")
	job, err := queueBackgroundJob(in.db, a.ID, 0, "x", "true")
	if err != nil {
		t.Fatal(err)
	}
	other := &mcpServer{db: in.db, config: in.cfg, botID: bb.ID}
	if res, _, _ := other.getBackground(t.Context(), nil, backgroundIDIn{ID: job.ID}); !res.IsError {
		t.Error("another bot must not read this job")
	}
	if res, _, _ := other.cancelBackground(t.Context(), nil, backgroundIDIn{ID: job.ID}); !res.IsError {
		t.Error("another bot must not cancel this job")
	}
}
