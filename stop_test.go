package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestKillActiveTurnEscalatesToSIGKILL(t *testing.T) {
	old := killGrace
	killGrace = 150 * time.Millisecond
	defer func() { killGrace = old }()

	cmd := exec.Command("/bin/sh", "-c", `trap '' TERM; printf ready\n; while :; do sleep 1; done`)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	if _, err := stdout.Read(buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(buf), "ready") {
		t.Fatalf("child did not install the TERM trap: %q", buf)
	}
	exited := make(chan struct{})
	errc := make(chan error, 1)
	go func() {
		errc <- cmd.Wait()
		close(exited)
	}()
	at := &activeTurn{cmd: cmd, pid: cmd.Process.Pid, exited: exited}
	if !killActiveTurn(at) {
		t.Fatal("expected kill")
	}
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("process did not die after SIGKILL escalation")
	}
	err = <-errc
	if err == nil {
		t.Fatal("want a signal death")
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("wait err = %v", err)
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || ws.Signal() != syscall.SIGKILL {
		t.Errorf("got %v, want SIGKILL", ws.Signal())
	}
}

func TestKillActiveTurnStopsAtSIGTERMWhenItSuffices(t *testing.T) {
	old := killGrace
	killGrace = 2 * time.Second
	defer func() { killGrace = old }()

	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	errc := make(chan error, 1)
	go func() {
		errc <- cmd.Wait()
		close(exited)
	}()
	at := &activeTurn{cmd: cmd, pid: cmd.Process.Pid, exited: exited}
	if !killActiveTurn(at) {
		t.Fatal("expected kill")
	}
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("polite process did not die on SIGTERM")
	}
	err := <-errc
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("wait err = %v", err)
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || ws.Signal() != syscall.SIGTERM {
		t.Errorf("got %v, want SIGTERM", ws.Signal())
	}
}

func hangingGrokDir(t *testing.T, hang time.Duration) string {
	t.Helper()
	dir := t.TempDir()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\necho '{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"working on it\"}]}}'\nexec %q %g\n",
		sleep, hang.Seconds())
	if err := os.WriteFile(filepath.Join(dir, "grok"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func withHangingGrok(t *testing.T, hang time.Duration) {
	t.Helper()
	dir := hangingGrokDir(t, hang)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func testGrokRunner(t *testing.T, hang time.Duration, timeoutS int) (*instance, *Runner, *Bot) {
	t.Helper()
	withHangingGrok(t, hang)
	in, _, _ := testInstance(t)
	in.cfg.DefaultEngine = engineGrok
	in.cfg.Profiles = map[string]*Profile{
		"grok": {Name: "grok", Engine: engineGrok, ConfigDir: t.TempDir()},
	}
	in.cfg.WorkerTurnTimeoutS = &timeoutS
	if _, err := in.ensureGeneralBot(); err != nil {
		t.Fatal(err)
	}
	w, err := in.createBot("deployer", "")
	if err != nil {
		t.Fatal(err)
	}
	r := newRunner(in.db, in.cfg, &fakeUI{})
	in.runner = r
	return in, r, w
}

func waitUntil(t *testing.T, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting")
}

func waitTurnFinished(t *testing.T, db *gorm.DB, id int64, d time.Duration) Turn {
	t.Helper()
	var last Turn
	waitUntil(t, d, func() bool {
		if err := db.First(&last, id).Error; err != nil {
			return false
		}
		return last.Status == turnDone || last.Status == turnFailed
	})
	return last
}

func TestWorkerTurnTimeoutKillsAndRelays(t *testing.T) {
	in, r, w := testGrokRunner(t, 30*time.Second, 1)
	chief, err := generalBot(in.db)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := r.Enqueue(w.ID, sourceUser, "hang please", 0)
	if err != nil {
		t.Fatal(err)
	}
	got := waitTurnFinished(t, in.db, turn.ID, 8*time.Second)
	if got.Status != turnFailed || got.ErrorClass != errTurnTimeout {
		t.Fatalf("turn = status=%s class=%s reason=%s", got.Status, got.ErrorClass, got.StopReason)
	}
	if !strings.Contains(got.StopReason, "worker_turn_timeout_s") {
		t.Errorf("reason missing knob: %s", got.StopReason)
	}
	live, _ := botByID(in.db, w.ID)
	if live.Status != botIdle {
		t.Errorf("bot status = %s, want idle", live.Status)
	}
	if r.Running(w.ID) {
		t.Error("process still running after the cap")
	}
	var gen []Turn
	in.db.Where("bot_id = ? AND source = ?", chief.ID, sourceBot).Find(&gen)
	if len(gen) != 1 {
		t.Fatalf("General bot turns = %d, want 1 relay: %+v", len(gen), gen)
	}
	if !strings.Contains(gen[0].Input, "killed") || !strings.Contains(gen[0].Input, "worker_turn_timeout_s") ||
		!strings.Contains(gen[0].Input, "tell_session") {
		t.Errorf("relay missing expected words:\n%s", gen[0].Input)
	}
}

func TestWorkerTurnTimeoutZeroDisables(t *testing.T) {
	in, r, w := testGrokRunner(t, 30*time.Second, 0)
	turn, err := r.Enqueue(w.ID, sourceUser, "hang please", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 3*time.Second, func() bool { return r.Running(w.ID) })
	time.Sleep(1500 * time.Millisecond)
	if !r.Running(w.ID) {
		t.Fatal("a disabled cap must leave the turn running")
	}
	if !r.Stop(w.ID) {
		t.Fatal("/stop should kill the uncapped hang")
	}
	got := waitTurnFinished(t, in.db, turn.ID, 5*time.Second)
	if got.ErrorClass != "stopped" {
		t.Errorf("class = %s, want stopped", got.ErrorClass)
	}
}

func TestStopKillsAWorkerTurn(t *testing.T) {
	in, r, w := testGrokRunner(t, 30*time.Second, 0)
	chief, err := generalBot(in.db)
	if err != nil {
		t.Fatal(err)
	}
	head, err := r.Enqueue(w.ID, sourceUser, "hang please", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Enqueue(w.ID, sourceUser, "queued after", 0); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 3*time.Second, func() bool { return r.Running(w.ID) })
	if !r.Stop(w.ID) {
		t.Fatal("Stop should have killed the worker")
	}
	got := waitTurnFinished(t, in.db, head.ID, 5*time.Second)
	if got.Status != turnFailed || got.ErrorClass != "stopped" {
		t.Fatalf("head turn = status=%s class=%s", got.Status, got.ErrorClass)
	}
	var queued int64
	in.db.Model(&Turn{}).Where("bot_id = ? AND status = ?", w.ID, turnQueued).Count(&queued)
	if queued != 0 {
		t.Errorf("%d turns still queued", queued)
	}
	var gen int64
	in.db.Model(&Turn{}).Where("bot_id = ? AND source = ?", chief.ID, sourceBot).Count(&gen)
	if gen != 0 {
		t.Errorf("/stop must not relay to General, got %d", gen)
	}
	live, _ := botByID(in.db, w.ID)
	if live.Status != botIdle {
		t.Errorf("status = %s, want idle", live.Status)
	}
}

func TestGeneralReportTurnIsCappedByWorkerTimeout(t *testing.T) {
	in, r, _ := testGrokRunner(t, 30*time.Second, 1)
	chief, err := generalBot(in.db)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := r.Enqueue(chief.ID, sourceBot, "Message from deployer:\nreport please", 0)
	if err != nil {
		t.Fatal(err)
	}
	got := waitTurnFinished(t, in.db, turn.ID, 8*time.Second)
	if got.ErrorClass != errTurnTimeout {
		t.Fatalf("class = %s, want %s (not chief_timeout)", got.ErrorClass, errTurnTimeout)
	}
	if got.ErrorClass == chiefTimeoutClass {
		t.Error("a report turn must not use the 60s owner cap")
	}
	var workers int64
	in.db.Model(&Bot{}).Where("id != ? AND archived_at IS NULL AND name != ?", chief.ID, "deployer").Count(&workers)
	if workers != 0 {
		t.Errorf("timeout must not auto-spawn a worker, extra live bots=%d", workers)
	}
}

func TestOwnerSessionStatusTimedOut(t *testing.T) {
	got, ok := ownerSessionStatus(errTurnTimeout, false)
	if !ok || got != "timed out" {
		t.Errorf("status = %q ok=%v, want timed out", got, ok)
	}
	if cardGlyph("timed out") != "⏱" {
		t.Errorf("glyph = %q, want ⏱", cardGlyph("timed out"))
	}
}

func TestWorkerTurnTimeoutKnob(t *testing.T) {
	if got := workerTurnTimeout(nil); got != 30*time.Minute {
		t.Errorf("default = %s, want 30m", got)
	}
	zero := 0
	if got := workerTurnTimeout(&Config{WorkerTurnTimeoutS: &zero}); got != 0 {
		t.Errorf("0 should disable, got %s", got)
	}
	huge := 99 * 3600
	if got := workerTurnTimeout(&Config{WorkerTurnTimeoutS: &huge}); got != 24*time.Hour {
		t.Errorf("clamp = %s, want 24h", got)
	}
	n := 300
	if got := workerTurnTimeout(&Config{WorkerTurnTimeoutS: &n}); got != 5*time.Minute {
		t.Errorf("300s = %s, want 5m", got)
	}

	isolateConfigEnv(t)
	t.Setenv("HOME", t.TempDir())
	cfg := &Config{BotToken: "x", ChatID: 1}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	got, err := configGet(loaded, "worker_turn_timeout_s")
	if err != nil {
		t.Fatal(err)
	}
	if got != "1800 (default)" {
		t.Errorf("get default = %q, want 1800 (default)", got)
	}
	if err := configSet(loaded, "worker_turn_timeout_s", "900"); err != nil {
		t.Fatal(err)
	}
	if loaded.WorkerTurnTimeoutS == nil || *loaded.WorkerTurnTimeoutS != 900 {
		t.Errorf("set 900 stored %+v", loaded.WorkerTurnTimeoutS)
	}
	if err := configSet(loaded, "worker_turn_timeout_s", "-1"); err == nil {
		t.Error("negative must be refused")
	}
}
