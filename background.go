package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gorm.io/gorm"
)

// background.go is the supervisor for long-running shell jobs (DESIGN §7).
// The MCP tool only inserts a queued row; `ccc listen` starts and reaps the
// process so the conversational turn is never blocked and never holds
// turns.status=running for the job.
//
// Jobs are detached from listen's lifetime: a /bin/sh wrapper writes
// stdout/stderr to <data_dir>/background/<id>/out.log and the exit code to
// exit.code, in its own process group. A listen restart reattaches by
// polling those artifacts (and kill(pid, 0)) instead of failing the row.

const (
	// backgroundTick is how often the listen loop looks for queued jobs and
	// cancel requests. Watches stay on schedulerTick; jobs should start in
	// about a second, not fifteen.
	backgroundTick = time.Second
	// backgroundWatchPoll is how often a supervisor goroutine looks for
	// exit.code or process death. Faster than backgroundTick so a finished
	// job wakes the bot without waiting a full second.
	backgroundWatchPoll = 100 * time.Millisecond
	// backgroundTimeout is a safety cap so a forgotten `sleep 999999` cannot
	// live forever. Builds and installs that need longer should be split.
	// Enforced by the supervisor against the row's deadline, not by binding
	// the process to listen's context — that would kill the job on restart.
	backgroundTimeout = 4 * time.Hour
	// maxBackgroundPerBot caps concurrent running jobs for one bot.
	maxBackgroundPerBot = 8
	// backgroundOutputLimit is how much combined stdout/stderr is stored.
	backgroundOutputLimit = 64 * 1024
	// jobLogReadCap is how much of the log is loaded before redaction.
	// Truncating first splits a secret that straddles the 64KiB boundary,
	// and the leftover prefix no longer matches the redactor.
	jobLogReadCap = 1 << 20
	// backgroundWakeLimit is how much of that reaches the model on wakeup.
	backgroundWakeLimit = 8 * 1024
	// backgroundListLimit is how many recent jobs list_background returns.
	backgroundListLimit = 20

	bgLogFile  = "out.log"
	bgExitFile = "exit.code"
	bgPidFile  = "pid"
)

// backgroundWrapper is the /bin/sh program that actually runs a job. $1 is
// the artifact directory, $2 is the user command (argv0 is "bgwrap"). It
// redirects the command onto out.log and writes exit.code last, atomically,
// so a new listen can finish the job without Wait4 on a reparented child.
const backgroundWrapper = `
jobdir=$1
cmd=$2
printf '%s\n' "$$" > "$jobdir/pid"
/bin/sh -c "$cmd" > "$jobdir/out.log" 2>&1
ec=$?
printf '%s\n' "$ec" > "$jobdir/exit.code.tmp"
mv "$jobdir/exit.code.tmp" "$jobdir/exit.code"
`

// bgHandle is one job this listen process is supervising. cmd is set only
// when we started the wrapper ourselves (so we can Wait and reap); a
// reattached orphan has only the PID.
type bgHandle struct {
	pid    int
	cmd    *exec.Cmd
	redact []string
}

// bgRuntime holds the live jobs the supervisor is watching. PIDs are also
// written to the row so a cancel from `ccc mcp` can SIGTERM them after a
// restart, when this map is empty.
type bgRuntime struct {
	mu      sync.Mutex
	handles map[int64]*bgHandle
}

func (r *bgRuntime) adopt(id int64, pid int, cmd *exec.Cmd, redact ...[]string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handles == nil {
		r.handles = map[int64]*bgHandle{}
	}
	if _, ok := r.handles[id]; ok {
		return false
	}
	h := &bgHandle{pid: pid, cmd: cmd}
	if len(redact) > 0 {
		h.redact = redact[0]
	}
	r.handles[id] = h
	return true
}

func (r *bgRuntime) redactOf(id int64) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handles == nil || r.handles[id] == nil {
		return nil
	}
	return r.handles[id].redact
}

// spawned reports whether this process started the wrapper (cmd != nil).
// A reattached row only has a PID, which is not safe to signal blindly.
func (r *bgRuntime) spawned(id int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handles == nil {
		return false
	}
	h := r.handles[id]
	return h != nil && h.cmd != nil
}

func (r *bgRuntime) pidOf(id int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handles == nil {
		return 0
	}
	h := r.handles[id]
	if h == nil {
		return 0
	}
	return h.pid
}

func (r *bgRuntime) drop(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.handles, id)
}

func killProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		_ = syscall.Kill(pid, syscall.SIGTERM) // safe-ignore: best-effort; the process may already be gone
	}
}

// systemBootTime is the last time this machine booted. Tests replace it.
// A background job's PID is not identity across a reboot: the number is
// handed out again to whatever starts first.
var systemBootTime = readSystemBootTime

func readSystemBootTime() (time.Time, bool) {
	if b, err := os.ReadFile("/proc/stat"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(line, "btime ") {
				continue
			}
			n, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "btime ")), 10, 64)
			if err == nil && n > 0 {
				return time.Unix(n, 0), true
			}
		}
	}
	out, err := exec.Command("sysctl", "-n", "kern.boottime").Output()
	if err != nil {
		return time.Time{}, false
	}
	var sec, usec int64
	if _, err := fmt.Sscanf(string(out), "{ sec = %d, usec = %d", &sec, &usec); err != nil || sec <= 0 {
		return time.Time{}, false
	}
	return time.Unix(sec, usec*1000), true
}

// jobPredatesBoot reports whether started is from before the current boot.
// A nil start (legacy row) is not treated as predating: we still refuse to
// signal it unless the process is the wrapper.
func jobPredatesBoot(started *time.Time) bool {
	if started == nil || started.IsZero() {
		return false
	}
	boot, ok := systemBootTime()
	if !ok {
		return false
	}
	return started.Before(boot.Add(-2 * time.Second))
}

func processArgs(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "args=").Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// processIsJobWrapper is true when pid is the /bin/sh wrapper this package
// starts (argv0 "bgwrap", and the job directory when we know it).
func processIsJobWrapper(pid int, jobDir string) bool {
	args, ok := processArgs(pid)
	if !ok || !strings.Contains(args, "bgwrap") {
		return false
	}
	if jobDir != "" && !strings.Contains(args, jobDir) {
		return false
	}
	return true
}

// killOwnedJobPID signals pid only when it is still our wrapper. A reused
// PID (after reboot, or after the wrapper exited) must not be killed.
// jobDir empty skips the directory check. Returns whether a signal was sent.
func killOwnedJobPID(pid int, started *time.Time, jobDir string) bool {
	if pid <= 0 || jobPredatesBoot(started) || !processIsJobWrapper(pid, jobDir) {
		return false
	}
	killProcessGroup(pid)
	return true
}

// backgroundProcessAlive is processAlive plus the boot-time check. A PID
// that outlived the job's start across a reboot is someone else's process.
func backgroundProcessAlive(j *BackgroundJob, dir string) bool {
	if j == nil || jobPredatesBoot(j.StartedAt) {
		return false
	}
	return processAlive(jobPID(j, dir))
}

// processAlive is kill(pid, 0): the process exists (or we lack permission
// to signal it, which still means it exists). Reparented orphans cannot be
// Wait4'd, so this is how a new listen decides a job is still running.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

// ---------------------------------------------------------------------------
// Durable artifacts
// ---------------------------------------------------------------------------

func backgroundJobDir(cfg *Config, id int64) string {
	return filepath.Join(dataDir(cfg), "background", strconv.FormatInt(id, 10))
}

func readExitCode(dir string) (int, bool) {
	b, err := os.ReadFile(filepath.Join(dir, bgExitFile))
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

func readJobLog(dir string) string {
	f, err := os.Open(filepath.Join(dir, bgLogFile))
	if err != nil {
		return ""
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, jobLogReadCap))
	if err != nil {
		return ""
	}
	return string(b)
}

func clipJobOutput(s string) string {
	if len(s) <= backgroundOutputLimit {
		return s
	}
	return s[:backgroundOutputLimit] + "\n…(output truncated)"
}

// redactJobOutput redacts first, then clips. A vault read error withholds
// the text instead of storing it raw.
func redactJobOutput(raw string, extra []string) (string, error) {
	redacted, err := redactVaultOutput(raw, extra)
	if err != nil {
		return "", err
	}
	return clipJobOutput(redacted), nil
}

func readPIDFile(dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, bgPidFile))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func jobPID(j *BackgroundJob, dir string) int {
	if j.PID > 0 {
		return j.PID
	}
	return readPIDFile(dir)
}

func jobDeadline(j *BackgroundJob) time.Time {
	if j.Deadline != nil {
		return *j.Deadline
	}
	if j.StartedAt != nil {
		return j.StartedAt.Add(backgroundTimeout)
	}
	return time.Now().Add(backgroundTimeout)
}

// ---------------------------------------------------------------------------
// Store helpers (MCP tools and the supervisor)
// ---------------------------------------------------------------------------

// bgSecrets is optional vault inject for a background job. Names only; values
// are read at start time.
type bgSecrets struct {
	Env         map[string]string
	StdinSecret string
}

// queueBackgroundJob inserts a queued shell job. The listen supervisor starts
// it; this never blocks the calling turn.
func queueBackgroundJob(db *gorm.DB, botID, turnID int64, name, command string, opts ...bgSecrets) (*BackgroundJob, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, fmt.Errorf("run_background needs a command")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = truncate(command, 80)
	} else {
		name = truncate(name, 120)
	}
	var opt bgSecrets
	if len(opts) > 0 {
		opt = opts[0]
	}
	if _, _, err := resolveSecretEnv(opt.Env); err != nil {
		return nil, err
	}
	if _, err := resolveStdinSecret(opt.StdinSecret); err != nil {
		return nil, err
	}
	row := BackgroundJob{
		BotID:       botID,
		Name:        name,
		Status:      jobQueued,
		Kind:        jobKindShell,
		Command:     command,
		StdinSecret: strings.TrimSpace(opt.StdinSecret),
	}
	if len(opt.Env) > 0 {
		body, err := json.Marshal(opt.Env)
		if err != nil {
			return nil, err
		}
		row.EnvJSON = string(body)
	}
	if turnID != 0 {
		row.CreatedByTurnID = &turnID
	}
	if err := db.Create(&row).Error; err != nil {
		return nil, err
	}
	return &row, nil
}

func parseJobSecretEnv(j *BackgroundJob) map[string]string {
	if j == nil || strings.TrimSpace(j.EnvJSON) == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(j.EnvJSON), &m); err != nil {
		return nil
	}
	return m
}

func listBackgroundJobs(db *gorm.DB, botID int64) ([]BackgroundJob, error) {
	var out []BackgroundJob
	err := db.Where("bot_id = ?", botID).Order("id DESC").Limit(backgroundListLimit).Find(&out).Error
	return out, err
}

func getBackgroundJob(db *gorm.DB, botID, id int64) (*BackgroundJob, error) {
	var j BackgroundJob
	if err := db.Where("id = ? AND bot_id = ?", id, botID).First(&j).Error; err != nil {
		return nil, fmt.Errorf("you have no background job #%d", id)
	}
	return &j, nil
}

// cancelBackgroundJob is best-effort: a queued job is marked failed at once; a
// running one is asked to die (cancel_requested + SIGTERM). kill may be nil.
func cancelBackgroundJob(db *gorm.DB, botID, id int64, kill func(int)) (string, error) {
	var j BackgroundJob
	if err := db.Where("id = ? AND bot_id = ?", id, botID).First(&j).Error; err != nil {
		return "", fmt.Errorf("you have no background job #%d", id)
	}
	switch j.Status {
	case jobQueued:
		now := time.Now()
		res := db.Model(&BackgroundJob{}).Where("id = ? AND status = ?", j.ID, jobQueued).
			Updates(map[string]any{"status": jobFailed, "error": "cancelled", "ended_at": now})
		if res.Error != nil {
			return "", res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Sprintf("job #%d is no longer queued", j.ID), nil
		}
		return fmt.Sprintf("cancelled queued job #%d", j.ID), nil
	case jobRunning:
		if err := db.Model(&BackgroundJob{}).Where("id = ?", j.ID).
			Update("cancel_requested", true).Error; err != nil {
			return "", err
		}
		if kill != nil && j.PID > 0 {
			kill(j.PID)
		}
		return fmt.Sprintf("cancel requested for running job #%d", j.ID), nil
	default:
		return fmt.Sprintf("job #%d is already %s", j.ID, j.Status), nil
	}
}

func formatBackgroundList(jobs []BackgroundJob) string {
	if len(jobs) == 0 {
		return "no background jobs"
	}
	var sb strings.Builder
	for _, j := range jobs {
		fmt.Fprintf(&sb, "#%d [%s] %s", j.ID, j.Status, j.Name)
		if j.Status == jobRunning && j.StartedAt != nil {
			fmt.Fprintf(&sb, " · running %s", humanDuration(time.Since(*j.StartedAt)))
		}
		if (j.Status == jobDone || j.Status == jobFailed) && j.ExitCode != nil {
			fmt.Fprintf(&sb, " · exit %d", *j.ExitCode)
		}
		sb.WriteByte('\n')
	}
	return strings.TrimRight(sb.String(), "\n")
}

func formatBackgroundJob(j *BackgroundJob) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "job #%d [%s] %s\nkind: %s\ncommand: %s\n", j.ID, j.Status, j.Name, j.Kind, j.Command)
	if j.StartedAt != nil {
		fmt.Fprintf(&sb, "started: %s\n", j.StartedAt.Format(time.RFC3339))
	}
	if j.EndedAt != nil {
		fmt.Fprintf(&sb, "ended: %s\n", j.EndedAt.Format(time.RFC3339))
	}
	if j.ExitCode != nil {
		fmt.Fprintf(&sb, "exit: %d\n", *j.ExitCode)
	}
	if j.Error != "" {
		fmt.Fprintf(&sb, "error: %s\n", j.Error)
	}
	if out := strings.TrimSpace(j.Output); out != "" {
		fmt.Fprintf(&sb, "\noutput:\n%s\n", truncate(out, backgroundWakeLimit))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// renderBackgroundWake is the input a finished job hands its bot.
func renderBackgroundWake(j *BackgroundJob) string {
	var sb strings.Builder
	status := "finished"
	if j.Status == jobFailed {
		status = "failed"
	}
	fmt.Fprintf(&sb, "Background job #%d %q %s.\n", j.ID, j.Name, status)
	if j.ExitCode != nil {
		fmt.Fprintf(&sb, "Exit code: %d\n", *j.ExitCode)
	}
	if j.Error != "" {
		fmt.Fprintf(&sb, "Error: %s\n", j.Error)
	}
	if out := strings.TrimSpace(j.Output); out == "" {
		sb.WriteString("\n(no output)\n")
	} else {
		fmt.Fprintf(&sb, "\nstdout/stderr (truncated):\n%s\n", truncate(out, backgroundWakeLimit))
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// Supervisor (runs inside the scheduler goroutine)
// ---------------------------------------------------------------------------

func (s *scheduler) stopped() bool {
	select {
	case <-s.stop:
		return true
	default:
		return false
	}
}

func (s *scheduler) tickBackground() {
	s.applyBackgroundCancels()
	s.startQueuedBackground()
}

func (s *scheduler) applyBackgroundCancels() {
	var jobs []BackgroundJob
	if err := s.in.db.Where("status = ? AND cancel_requested = ?", jobRunning, true).Find(&jobs).Error; err != nil {
		return
	}
	for i := range jobs {
		j := jobs[i]
		if s.bg.spawned(j.ID) {
			if pid := s.bg.pidOf(j.ID); pid > 0 {
				killProcessGroup(pid)
			}
			continue
		}
		dir := backgroundJobDir(s.in.config(), j.ID)
		killOwnedJobPID(jobPID(&j, dir), j.StartedAt, dir)
	}
}

func (s *scheduler) startQueuedBackground() {
	var jobs []BackgroundJob
	if err := s.in.db.Where("status = ?", jobQueued).Order("id").Find(&jobs).Error; err != nil {
		return
	}
	for i := range jobs {
		j := jobs[i]
		b, err := botByID(s.in.db, j.BotID)
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			// database is locked and the like: leave the row queued.
			continue
		}
		if err != nil || b.ArchivedAt != nil {
			now := time.Now()
			s.in.db.Model(&BackgroundJob{}).Where("id = ?", j.ID).Updates(map[string]any{
				"status": jobFailed, "error": "bot is gone", "ended_at": now,
			})
			continue
		}
		var n int64
		s.in.db.Model(&BackgroundJob{}).Where("bot_id = ? AND status = ?", j.BotID, jobRunning).Count(&n)
		if n >= maxBackgroundPerBot {
			continue
		}
		now := time.Now()
		deadline := now.Add(backgroundTimeout)
		res := s.in.db.Model(&BackgroundJob{}).Where("id = ? AND status = ?", j.ID, jobQueued).
			Updates(map[string]any{"status": jobRunning, "started_at": now, "deadline": deadline})
		if res.RowsAffected == 0 {
			continue
		}
		j.Status = jobRunning
		j.StartedAt = &now
		j.Deadline = &deadline
		s.in.syncPanel(true)
		go s.runBackgroundJob(b, &j)
	}
}

func (s *scheduler) runBackgroundJob(b *Bot, j *BackgroundJob) {
	cfg := s.in.config()
	dir := backgroundJobDir(cfg, j.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.finishBackgroundJob(j, jobFailed, -1, "", err.Error())
		return
	}

	// No CommandContext: a cancelled listen context must not kill the job.
	// The 4h cap is the deadline column, checked by watchBackgroundJob.
	extra, redact, err := resolveSecretEnv(parseJobSecretEnv(j))
	if err != nil {
		s.finishBackgroundJob(j, jobFailed, -1, "", err.Error())
		return
	}
	stdin, err := resolveStdinSecret(j.StdinSecret)
	if err != nil {
		s.finishBackgroundJob(j, jobFailed, -1, "", err.Error())
		return
	}
	if stdin != "" {
		redact = append(redact, stdin)
	}

	cmd := exec.Command("/bin/sh", "-c", backgroundWrapper, "bgwrap", dir, j.Command)
	cmd.Dir = botCwd(cfg, b)
	cmd.Env = overlayEnv(instanceEnv(cfg), extra)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		s.finishBackgroundJob(j, jobFailed, -1, "", err.Error())
		return
	}
	j.PID = cmd.Process.Pid
	s.in.db.Model(&BackgroundJob{}).Where("id = ?", j.ID).Update("pid", j.PID)
	if !s.bg.adopt(j.ID, j.PID, cmd, redact) {
		// Another supervisor already claimed this id (reattach raced us).
		// Reap so we do not leave a zombie, then leave watching to them.
		go func() { _ = cmd.Wait() }() // safe-ignore: Wait is only to reap; finish comes from exit.code
		return
	}
	go func() { _ = cmd.Wait() }() // safe-ignore: Wait is only to reap; finish comes from exit.code
	s.watchBackgroundJob(j)
}

// reattachBackgroundJobs is what a new listen does with leftover running
// rows: finish the ones that already wrote exit.code, supervise the ones
// whose PID is still alive, fail the rest. It does not kill anyone. A job
// that is still running gets a notifying ping in its bot's topic so the
// owner can see it was resumed rather than lost.
func (s *scheduler) reattachBackgroundJobs() {
	var jobs []BackgroundJob
	if err := s.in.db.Where("status = ?", jobRunning).Find(&jobs).Error; err != nil {
		return
	}
	for i := range jobs {
		j := jobs[i]
		dir := backgroundJobDir(s.in.config(), j.ID)
		if pid := jobPID(&j, dir); pid > 0 && j.PID != pid {
			j.PID = pid
			s.in.db.Model(&BackgroundJob{}).Where("id = ?", j.ID).Update("pid", pid)
		}
		if !s.bg.adopt(j.ID, j.PID, nil) { // reattach: redact from the live vault at finish
			continue
		}
		if _, done := readExitCode(dir); !done && backgroundProcessAlive(&j, dir) {
			s.notifyJobResumed(&j)
		}
		go s.watchBackgroundJob(&j)
	}
}

func (s *scheduler) notifyJobResumed(j *BackgroundJob) {
	b, err := botByID(s.in.db, j.BotID)
	if err != nil || b.ArchivedAt != nil {
		return
	}
	s.in.notifyBot(b, renderJobResumedNotice(j))
}

// watchBackgroundJob waits for exit.code or process death without requiring
// Wait on a child. listen shutdown returns without killing the job.
func (s *scheduler) watchBackgroundJob(j *BackgroundJob) {
	defer s.bg.drop(j.ID)
	cfg := s.in.config()
	dir := backgroundJobDir(cfg, j.ID)
	ticker := time.NewTicker(backgroundWatchPoll)
	defer ticker.Stop()

	killed := false
	for {
		if s.finishIfComplete(j, dir) {
			return
		}
		if s.stopped() {
			return
		}
		if !killed && s.shouldStopJob(j) {
			if s.bg.spawned(j.ID) {
				if pid := s.bg.pidOf(j.ID); pid > 0 {
					killProcessGroup(pid)
				}
			} else if pid := jobPID(j, dir); pid > 0 {
				if !killOwnedJobPID(pid, j.StartedAt, dir) {
					// Not our wrapper. Do not signal. Finish the row so a
					// reused PID cannot sit "running" until its deadline.
					s.finishBackgroundJob(j, jobFailed, -1, "", "process is not the job wrapper")
					return
				}
			}
			killed = true
		}
		select {
		case <-s.stop:
			return
		case <-ticker.C:
		}
	}
}

func (s *scheduler) shouldStopJob(j *BackgroundJob) bool {
	var latest BackgroundJob
	if err := s.in.db.First(&latest, j.ID).Error; err != nil {
		return false
	}
	j.CancelRequested = latest.CancelRequested
	j.Deadline = latest.Deadline
	j.StartedAt = latest.StartedAt
	j.PID = latest.PID
	j.Status = latest.Status
	if j.Status != jobRunning {
		return false
	}
	if j.CancelRequested {
		return true
	}
	return time.Now().After(jobDeadline(j))
}

func (s *scheduler) finishIfComplete(j *BackgroundJob, dir string) bool {
	var latest BackgroundJob
	if err := s.in.db.First(&latest, j.ID).Error; err != nil {
		// A missing row is done. A lock timeout is not: returning true
		// dropped the watcher and left the job running until the next boot.
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return true
		}
		return false
	}
	if latest.Status != jobRunning {
		return true
	}
	*j = latest

	if code, ok := readExitCode(dir); ok {
		s.finishFromArtifacts(j, dir, code, true)
		return true
	}
	if jobPredatesBoot(j.StartedAt) {
		out, errText := jobOutputOrWithheld(dir, s.bg.redactOf(j.ID), "process did not survive reboot")
		s.finishBackgroundJob(j, jobFailed, -1, out, errText)
		return true
	}
	pid := jobPID(j, dir)
	if processAlive(pid) {
		return false
	}
	// PID is gone. The wrapper writes exit.code and then exits, so a
	// restart that lands between those two steps should wait a beat.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if code, ok := readExitCode(dir); ok {
			s.finishFromArtifacts(j, dir, code, true)
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.finishFromArtifacts(j, dir, -1, false)
	return true
}

func jobOutputOrWithheld(dir string, extra []string, reason string) (string, string) {
	out, err := redactJobOutput(readJobLog(dir), extra)
	if err != nil {
		return "", reason + " (output withheld)"
	}
	return out, reason
}

func (s *scheduler) finishFromArtifacts(j *BackgroundJob, dir string, code int, haveCode bool) {
	out, rerr := redactJobOutput(readJobLog(dir), s.bg.redactOf(j.ID))
	status, errText, exit := classifyBackgroundFinish(j, code, haveCode)
	if rerr != nil {
		out = ""
		if errText != "" {
			errText += " "
		}
		errText += "(output withheld)"
	} else if redacted, err := redactVaultOutput(errText, s.bg.redactOf(j.ID)); err != nil {
		errText = "(output withheld)"
	} else {
		errText = redacted
	}
	s.finishBackgroundJob(j, status, exit, out, errText)
}

func classifyBackgroundFinish(j *BackgroundJob, code int, haveCode bool) (status string, errText string, exit int) {
	exit = code
	if !haveCode {
		exit = -1
	}
	if j.CancelRequested {
		return jobFailed, "cancelled", exit
	}
	// A job that actually finished successfully while listen was down is
	// done, even if that was past the 4h deadline. The deadline is a kill
	// switch for still-running jobs, not a rejection of a written exit 0.
	if haveCode && code == 0 {
		return jobDone, "", 0
	}
	if time.Now().After(jobDeadline(j)) {
		return jobFailed, "timed out after " + backgroundTimeout.String(), exit
	}
	if !haveCode {
		return jobFailed, "process died without writing an exit status", -1
	}
	return jobFailed, fmt.Sprintf("exit status %d", code), code
}

func (s *scheduler) finishBackgroundJob(j *BackgroundJob, status string, exit int, output, errText string) {
	now := time.Now()
	code := exit
	res := s.in.db.Model(&BackgroundJob{}).Where("id = ? AND status = ?", j.ID, jobRunning).
		Updates(map[string]any{
			"status": status, "ended_at": now, "output": output, "error": errText, "pid": 0, "exit_code": code,
		})
	if res.Error != nil {
		hookLog("background %d: finish update: %v", j.ID, res.Error)
		return
	}
	if res.RowsAffected == 0 {
		return
	}
	j.Status = status
	j.EndedAt = &now
	j.Output = output
	j.Error = errText
	j.ExitCode = &code
	s.in.syncPanel(true)
	s.enqueueBackgroundWake(j)
}

func (s *scheduler) enqueueBackgroundWake(j *BackgroundJob) {
	b, err := botByID(s.in.db, j.BotID)
	if err != nil || b.ArchivedAt != nil {
		return
	}
	// A failed job pings Telegram immediately. The source=background wake
	// still runs so the model can react, but the owner must not depend on
	// that turn (it dies if listen restarts again). Cancels are quiet: the
	// owner already asked for them.
	if _, err := s.in.runner.Enqueue(b.ID, sourceBackground, renderBackgroundWake(j), 0); err != nil {
		hookLog("background %d: enqueue failed: %v", j.ID, err)
	}
	if j.Status == jobFailed && j.Error != "cancelled" {
		s.in.notifyBot(b, renderJobFailedNotice(j))
	}
}

func renderJobResumedNotice(j *BackgroundJob) string {
	return fmt.Sprintf("▶️ Resumed background job #%d <i>%s</i>", j.ID, htmlEscape(jobNoticeName(j)))
}

func renderJobFailedNotice(j *BackgroundJob) string {
	detail := strings.TrimSpace(j.Error)
	if detail == "" {
		detail = "failed"
	}
	return fmt.Sprintf("❌ Background job #%d <i>%s</i> failed.\n<code>%s</code>",
		j.ID, htmlEscape(jobNoticeName(j)), htmlEscape(truncate(detail, 400)))
}

func jobNoticeName(j *BackgroundJob) string {
	name := strings.TrimSpace(j.Name)
	if name == "" {
		name = strings.TrimSpace(j.Command)
	}
	return truncate(name, 80)
}
