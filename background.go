package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
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

const (
	// backgroundTick is how often the listen loop looks for queued jobs and
	// cancel requests. Watches stay on schedulerTick; jobs should start in
	// about a second, not fifteen.
	backgroundTick = time.Second
	// backgroundTimeout is a safety cap so a forgotten `sleep 999999` cannot
	// live forever. Builds and installs that need longer should be split.
	backgroundTimeout = 4 * time.Hour
	// maxBackgroundPerBot caps concurrent running jobs for one bot.
	maxBackgroundPerBot = 8
	// backgroundOutputLimit is how much combined stdout/stderr is stored.
	backgroundOutputLimit = 64 * 1024
	// backgroundWakeLimit is how much of that reaches the model on wakeup.
	backgroundWakeLimit = 8 * 1024
	// backgroundListLimit is how many recent jobs list_background returns.
	backgroundListLimit = 20
)

// bgRuntime holds the live process handles the supervisor started. PIDs are
// also written to the row so a cancel from `ccc mcp` can SIGTERM them.
type bgRuntime struct {
	mu   sync.Mutex
	cmds map[int64]*exec.Cmd
}

func (r *bgRuntime) set(id int64, cmd *exec.Cmd) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cmds == nil {
		r.cmds = map[int64]*exec.Cmd{}
	}
	r.cmds[id] = cmd
}

func (r *bgRuntime) get(id int64) *exec.Cmd {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cmds == nil {
		return nil
	}
	return r.cmds[id]
}

func (r *bgRuntime) drop(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cmds, id)
}

func (r *bgRuntime) snapshot() map[int64]*exec.Cmd {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[int64]*exec.Cmd, len(r.cmds))
	for k, v := range r.cmds {
		out[k] = v
	}
	return out
}

func killProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		_ = syscall.Kill(pid, syscall.SIGTERM) // safe-ignore: best-effort; the process may already be gone
	}
}

// ---------------------------------------------------------------------------
// Store helpers (MCP tools and the supervisor)
// ---------------------------------------------------------------------------

// queueBackgroundJob inserts a queued shell job. The listen supervisor starts
// it; this never blocks the calling turn.
func queueBackgroundJob(db *gorm.DB, botID, turnID int64, name, command string) (*BackgroundJob, error) {
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
	row := BackgroundJob{
		BotID:   botID,
		Name:    name,
		Status:  jobQueued,
		Kind:    jobKindShell,
		Command: command,
	}
	if turnID != 0 {
		row.CreatedByTurnID = &turnID
	}
	if err := db.Create(&row).Error; err != nil {
		return nil, err
	}
	return &row, nil
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

// failOrphanedBackgroundJobs marks leftover running jobs failed after a listen
// restart: those processes died with the previous ccc. Queued jobs stay queued
// so the new supervisor can start them. Each orphan wakes its bot.
func failOrphanedBackgroundJobs(db *gorm.DB, r turnRunner) {
	var jobs []BackgroundJob
	if err := db.Where("status = ?", jobRunning).Find(&jobs).Error; err != nil {
		return
	}
	now := time.Now()
	for i := range jobs {
		j := &jobs[i]
		res := db.Model(&BackgroundJob{}).Where("id = ? AND status = ?", j.ID, jobRunning).
			Updates(map[string]any{
				"status": jobFailed, "error": "ccc restarted while the job was running", "ended_at": now, "pid": 0,
			})
		if res.RowsAffected == 0 {
			continue
		}
		j.Status = jobFailed
		j.Error = "ccc restarted while the job was running"
		j.EndedAt = &now
		if r == nil {
			continue
		}
		if _, err := r.Enqueue(j.BotID, sourceBackground, renderBackgroundWake(j), 0); err != nil {
			hookLog("background %d: orphan wakeup: %v", j.ID, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Supervisor (runs inside the scheduler goroutine)
// ---------------------------------------------------------------------------

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
		if cmd := s.bg.get(j.ID); cmd != nil && cmd.Process != nil {
			killProcessGroup(cmd.Process.Pid)
			continue
		}
		if j.PID > 0 {
			killProcessGroup(j.PID)
		}
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
		res := s.in.db.Model(&BackgroundJob{}).Where("id = ? AND status = ?", j.ID, jobQueued).
			Updates(map[string]any{"status": jobRunning, "started_at": now})
		if res.RowsAffected == 0 {
			continue
		}
		j.Status = jobRunning
		j.StartedAt = &now
		go s.runBackgroundJob(b, &j)
	}
}

func (s *scheduler) runBackgroundJob(b *Bot, j *BackgroundJob) {
	cfg := s.in.config()
	ctx, cancel := context.WithTimeout(context.Background(), backgroundTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", j.Command)
	cmd.Dir = botCwd(cfg, b)
	cmd.Env = instanceEnv(cfg)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	if err := cmd.Start(); err != nil {
		s.finishBackgroundJob(j, jobFailed, -1, "", err.Error())
		return
	}
	s.bg.set(j.ID, cmd)
	s.in.db.Model(&BackgroundJob{}).Where("id = ?", j.ID).Update("pid", cmd.Process.Pid)

	waitErr := cmd.Wait()
	s.bg.drop(j.ID)

	out := buf.String()
	if len(out) > backgroundOutputLimit {
		out = out[:backgroundOutputLimit] + "\n…(output truncated)"
	}
	exit := 0
	if cmd.ProcessState != nil {
		exit = cmd.ProcessState.ExitCode()
	}
	status := jobDone
	errText := ""
	if waitErr != nil {
		status = jobFailed
		errText = waitErr.Error()
		if ctx.Err() != nil {
			errText = "timed out after " + backgroundTimeout.String()
		}
	}
	var latest BackgroundJob
	if s.in.db.First(&latest, j.ID).Error == nil && latest.CancelRequested {
		status = jobFailed
		if errText == "" {
			errText = "cancelled"
		} else if !strings.Contains(errText, "cancel") {
			errText = "cancelled: " + errText
		}
	}
	s.finishBackgroundJob(j, status, exit, out, errText)
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
	s.enqueueBackgroundWake(j)
}

func (s *scheduler) enqueueBackgroundWake(j *BackgroundJob) {
	b, err := botByID(s.in.db, j.BotID)
	if err != nil || b.ArchivedAt != nil {
		return
	}
	if _, err := s.in.runner.Enqueue(b.ID, sourceBackground, renderBackgroundWake(j), 0); err != nil {
		hookLog("background %d: enqueue failed: %v", j.ID, err)
	}
}

func (s *scheduler) killAllBackground() {
	for _, cmd := range s.bg.snapshot() {
		if cmd != nil && cmd.Process != nil {
			killProcessGroup(cmd.Process.Pid)
		}
	}
}
