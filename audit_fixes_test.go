package main

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestScheduleDueComparesInstantsNotText(t *testing.T) {
	s, in, runner, _ := testScheduler(t)
	b, err := in.createBot("sleeper", "")
	if err != nil {
		t.Fatal(err)
	}
	// now is 07:00 at UTC+2, which is 05:00Z. A lexicographic compare of the
	// driver format would treat 07:00+00 as already due (the "7" sorts before
	// the "8" of a +02 rendering of now) and would miss 07:00+02.
	madrid := time.FixedZone("CEST", 2*60*60)
	now := time.Date(2026, 9, 24, 7, 0, 0, 0, madrid)
	rows := []struct {
		at   string
		note string
		due  bool
	}{
		{"2026-09-24 07:00:00+00:00", "utc-later", false},
		{"2026-09-24 07:00:00+02:00", "madrid-due", true},
		{"2026-09-24 09:00:00+02:00", "madrid-later", false},
	}
	for _, row := range rows {
		if err := in.db.Exec(
			"INSERT INTO schedules (bot_id, name, fire_at, note, recurring_cron, timezone) VALUES (?, '', ?, ?, '', '')",
			b.ID, row.at, row.note,
		).Error; err != nil {
			t.Fatal(err)
		}
	}
	s.fireDueSchedules(now)
	got := runner.queued()
	if len(got) != 1 {
		t.Fatalf("enqueued %d, want only madrid-due: %+v", len(got), got)
	}
	if got[0].Text != "Scheduled wakeup: madrid-due" {
		t.Fatalf("fired %q", got[0].Text)
	}
}

func TestEnqueueIsIdempotentForTriggerMessage(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("dev", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := in.db.Model(&Bot{}).Where("id = ?", b.ID).Update("status", botDisabled).Error; err != nil {
		t.Fatal(err)
	}
	r := newRunner(in.db, in.config(), nil)
	t.Cleanup(r.Close)

	first, err := r.Enqueue(b.ID, sourceUser, "ship it", 55)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Enqueue(b.ID, sourceUser, "ship it again", 55)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("redelivery created turn %d, want %d", second.ID, first.ID)
	}
	var n int64
	if err := in.db.Model(&Turn{}).Where("bot_id = ? AND trigger_message_id = ?", b.ID, 55).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("turns for message 55 = %d, want 1", n)
	}
	third, err := r.Enqueue(b.ID, sourceSchedule, "internal", 0)
	if err != nil {
		t.Fatal(err)
	}
	fourth, err := r.Enqueue(b.ID, sourceSchedule, "internal again", 0)
	if err != nil {
		t.Fatal(err)
	}
	if third.ID == fourth.ID {
		t.Fatal("trigger 0 must not be deduped")
	}
}

func TestCloseKillsActiveTurnProcessGroup(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	r := &Runner{active: map[int64]*activeTurn{}, done: make(chan struct{})}
	r.active[1] = &activeTurn{cmd: cmd, pid: cmd.Process.Pid, exited: exited}
	r.Close()
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("Close left the engine process running")
	}
}

func TestTelegramOffsetRoundTrip(t *testing.T) {
	in, _, _ := testInstance(t)
	in.rememberUpdateOffset(77)
	if got := getSettingInt(in.db, settingTelegramOffset, 0); got != 77 {
		t.Fatalf("offset = %d, want 77", got)
	}
	in.rememberUpdateOffset(0)
	if got := getSettingInt(in.db, settingTelegramOffset, 0); got != 77 {
		t.Fatalf("offset 0 overwrote the stored value: %d", got)
	}
}

func TestReattachDoesNotKillAPIDFromBeforeBoot(t *testing.T) {
	s, in, runner, api := testScheduler(t)
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
		killProcessGroup(cmd.Process.Pid)
		_ = cmd.Wait()
	})
	prev := systemBootTime
	systemBootTime = func() (time.Time, bool) { return time.Now().Add(time.Hour), true }
	t.Cleanup(func() { systemBootTime = prev })

	now := time.Now()
	job := BackgroundJob{
		BotID: b.ID, Name: "stale-pid", Status: jobRunning,
		Kind: jobKindShell, Command: "sleep 30", StartedAt: &now, PID: cmd.Process.Pid,
	}
	if err := in.db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	s.reattachBackgroundJobs()
	got := waitJob(t, in, job.ID)
	if got.Status != jobFailed || got.Error != "process did not survive reboot" {
		t.Fatalf("status=%q error=%q", got.Status, got.Error)
	}
	if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
		t.Fatalf("reattach signaled the reused pid: %v", err)
	}
	queued := waitEnqueued(t, runner, 1)
	if len(queued) != 1 || queued[0].Source != sourceBackground {
		t.Fatalf("wakeup = %+v", queued)
	}
	if _, ok := sendContaining(api, "Resumed"); ok {
		t.Fatal("a pid from before boot must not be reported as resumed")
	}
}

func TestKillOwnedJobPIDRefusesAStranger(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		killProcessGroup(cmd.Process.Pid)
		_ = cmd.Wait()
	})
	started := time.Now()
	if killOwnedJobPID(cmd.Process.Pid, &started, t.TempDir()) {
		t.Fatal("signaled a process that is not the job wrapper")
	}
	if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
		t.Fatalf("stranger was killed: %v", err)
	}
}

func TestProcessIsJobWrapper(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("/bin/sh", "-c", "sleep 30", "bgwrap", dir, "sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		killProcessGroup(cmd.Process.Pid)
		_ = cmd.Wait()
	})
	if !processIsJobWrapper(cmd.Process.Pid, dir) {
		args, _ := processArgs(cmd.Process.Pid)
		t.Fatalf("wrapper not recognized: %q", args)
	}
	if processIsJobWrapper(cmd.Process.Pid, dir+"/nope") {
		t.Fatal("a different job directory matched")
	}
}
