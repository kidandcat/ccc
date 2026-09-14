package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"gorm.io/gorm"
)

// scheduler.go is the one background goroutine of DESIGN §7: it runs watches,
// fires schedules and keeps the account health up to date. Everything it does
// is deterministic and model-free — a watch that sees no change costs nothing,
// which is the whole point of watches existing instead of a bot polling.

const (
	// schedulerTick is how often the loop looks for due work. Watch intervals
	// are at least a minute, so this is fine-grained enough for both.
	schedulerTick = 15 * time.Second
	// doctorInterval is DESIGN §7's 15 minutes.
	doctorInterval = 15 * time.Minute
	// watchMinInterval is the floor on a watch interval, so a bot cannot ask
	// ccc to hammer a command.
	watchMinInterval = 60
	// watchTimeout caps one watch command.
	watchTimeout = 2 * time.Minute
	// watchOutputLimit is how much of a command's stdout is kept.
	watchOutputLimit = 64 * 1024
	// watchDiffLimit is how much of the diff reaches the model (DESIGN §7: ~8 KB).
	watchDiffLimit = 8 * 1024
)

// scheduler owns the loop. It is constructed by listenV3 and stopped with it.
type scheduler struct {
	in     *instance
	stop   chan struct{}
	doctor *doctorState
}

func newScheduler(in *instance) *scheduler {
	return &scheduler{in: in, stop: make(chan struct{}), doctor: &doctorState{}}
}

func (s *scheduler) Close() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
}

// Run is the loop. It never returns an error: a failing watch is reported to
// its bot, and a failing doctor probe is simply retried next tick.
func (s *scheduler) Run() {
	// Probe the accounts once at boot so /status is meaningful immediately.
	s.runDoctor(time.Now())
	ticker := time.NewTicker(schedulerTick)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case now := <-ticker.C:
			s.runDueWatches(now)
			s.fireDueSchedules(now)
			if now.Sub(s.doctor.lastRun) >= doctorInterval {
				s.runDoctor(now)
			}
			s.runMaintenanceIfDue(now)
		}
	}
}

// ---------------------------------------------------------------------------
// Watches
// ---------------------------------------------------------------------------

// runDueWatches runs every enabled watch whose interval has elapsed.
func (s *scheduler) runDueWatches(now time.Time) {
	var watches []Watch
	if err := s.in.db.Where("enabled = ?", true).Find(&watches).Error; err != nil {
		return
	}
	for i := range watches {
		w := watches[i]
		if w.LastRunAt != nil && now.Sub(*w.LastRunAt) < time.Duration(w.IntervalS)*time.Second {
			continue
		}
		s.runWatch(&w, now)
	}
}

// runWatch executes one watch and wakes its bot when the output changed.
func (s *scheduler) runWatch(w *Watch, now time.Time) {
	b, err := botByID(s.in.db, w.BotID)
	if err != nil || b.ArchivedAt != nil {
		// The bot is gone: retire the watch rather than running it forever.
		s.in.db.Model(&Watch{}).Where("id = ?", w.ID).Update("enabled", false)
		return
	}
	output, runErr := runWatchCommand(s.in.config(), b, w.Command)
	hash := hashOutput(output)

	updates := map[string]any{"last_run_at": now, "last_hash": hash, "last_output": output}
	s.in.db.Model(&Watch{}).Where("id = ?", w.ID).Updates(updates)

	if w.LastHash == "" && w.LastRunAt == nil {
		// First run: record the baseline without waking anybody. A watch is a
		// change detector, and "it exists" is not a change.
		return
	}
	if hash == w.LastHash {
		return
	}
	input := renderWatchChange(w, w.LastOutput, output, runErr)
	if _, err := s.in.runner.Enqueue(b.ID, sourceWatch, input, 0); err != nil {
		hookLog("watch %s: enqueue failed: %v", w.Name, err)
	}
}

// runWatchCommand runs a watch's command in the bot's working directory with
// the instance environment. No Claude profile is involved: this is a plain
// subprocess, and its output is data.
func runWatchCommand(cfg *Config, b *Bot, command string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), watchTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = botCwd(cfg, b)
	cmd.Env = instanceEnv(cfg)
	out, err := cmd.CombinedOutput()
	text := string(out)
	if len(text) > watchOutputLimit {
		text = text[:watchOutputLimit] + "\n…(output truncated)"
	}
	return text, err
}

// instanceEnv is the environment deterministic subprocesses (watches) run
// with: the same whitelist as a bot, plus env_passthrough, and never a
// CLAUDE_CONFIG_DIR — nothing here talks to Claude.
func instanceEnv(cfg *Config) []string {
	return botEnv(cfg, Profile{Name: defaultProfileName, Implicit: true})
}

func hashOutput(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// renderWatchChange is the input a changed watch hands its bot: what the watch
// is, and a line diff of before/after truncated to watchDiffLimit.
func renderWatchChange(w *Watch, before, after string, runErr error) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Watch %q changed.\nCommand: %s\n", w.Name, w.Command)
	if runErr != nil {
		fmt.Fprintf(&sb, "The command exited with an error: %v\n", runErr)
	}
	sb.WriteString("\nWhat changed:\n")
	sb.WriteString(lineDiff(before, after, watchDiffLimit))
	return sb.String()
}

// lineDiff renders a set-based diff: lines that disappeared with "-", lines
// that appeared with "+". It is not a minimal edit script, but a watch's job is
// to say WHAT is different, and duplicating a real diff algorithm here would
// buy nothing the model cannot read straight off these two lists.
func lineDiff(before, after string, limit int) string {
	oldLines := strings.Split(strings.TrimRight(before, "\n"), "\n")
	newLines := strings.Split(strings.TrimRight(after, "\n"), "\n")
	inNew := map[string]int{}
	for _, l := range newLines {
		inNew[l]++
	}

	var sb strings.Builder
	write := func(prefix, line string) bool {
		if sb.Len()+len(line)+3 > limit {
			sb.WriteString("…(diff truncated)\n")
			return false
		}
		sb.WriteString(prefix + line + "\n")
		return true
	}
	for _, l := range oldLines {
		if inNew[l] > 0 {
			inNew[l]--
			continue
		}
		if strings.TrimSpace(l) == "" {
			continue
		}
		if !write("- ", l) {
			return sb.String()
		}
	}
	// Recompute what is left over on the new side.
	remaining := map[string]int{}
	for _, l := range newLines {
		remaining[l]++
	}
	for _, l := range oldLines {
		if remaining[l] > 0 {
			remaining[l]--
		}
	}
	for _, l := range newLines {
		if remaining[l] == 0 {
			continue
		}
		remaining[l]--
		if strings.TrimSpace(l) == "" {
			continue
		}
		if !write("+ ", l) {
			return sb.String()
		}
	}
	if sb.Len() == 0 {
		// Same lines, different order or whitespace only.
		return "(the output was reordered; nothing was added or removed)\n"
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// Schedules
// ---------------------------------------------------------------------------

// cronParser accepts the 5-field crontab syntax plus @hourly/@daily-style
// descriptors, which is what a bot is most likely to write.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// parseCron validates a cron expression and returns its schedule.
func parseCron(expr string) (cron.Schedule, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, fmt.Errorf("empty cron expression")
	}
	return cronParser.Parse(expr)
}

// fireDueSchedules enqueues a turn for every schedule whose time has come.
func (s *scheduler) fireDueSchedules(now time.Time) {
	var due []Schedule
	if err := s.in.db.Where("fired_at IS NULL AND fire_at <= ?", now).Find(&due).Error; err != nil {
		return
	}
	for i := range due {
		sc := due[i]
		b, err := botByID(s.in.db, sc.BotID)
		if err != nil || b.ArchivedAt != nil {
			s.in.db.Model(&Schedule{}).Where("id = ?", sc.ID).Update("fired_at", now)
			continue
		}
		note := strings.TrimSpace(sc.Note)
		if note == "" {
			note = "(no note)"
		}
		input := fmt.Sprintf("Scheduled wakeup: %s", note)
		if _, err := s.in.runner.Enqueue(b.ID, sourceSchedule, input, 0); err != nil {
			hookLog("schedule %d: enqueue failed: %v", sc.ID, err)
			continue
		}
		// A recurring schedule rolls forward instead of being retired, so one
		// row keeps firing for the life of the bot.
		if sc.RecurringCron != "" {
			if schedule, err := parseCron(sc.RecurringCron); err == nil {
				s.in.db.Model(&Schedule{}).Where("id = ?", sc.ID).Update("fire_at", schedule.Next(now))
				continue
			}
			hookLog("schedule %d: unparseable cron %q, retiring it", sc.ID, sc.RecurringCron)
		}
		s.in.db.Model(&Schedule{}).Where("id = ?", sc.ID).Update("fired_at", now)
	}
}

// ---------------------------------------------------------------------------
// Maintenance
// ---------------------------------------------------------------------------

// runMaintenanceIfDue runs the daily growth-control job once a day, at the
// configured quiet hour (DESIGN §7). It is deliberately a check on the normal
// tick rather than a timer: a machine that was asleep at 04:00 still gets its
// maintenance the next time ccc is awake, and the marker being a date means it
// runs once either way.
func (s *scheduler) runMaintenanceIfDue(now time.Time) {
	if !maintenanceDue(s.in.db, s.in.config(), now) {
		return
	}
	// Claim the day before doing the work: a slow pass must not be started
	// twice by the next tick.
	markMaintenanceRun(s.in.db, now)
	go s.runMaintenanceNow(now)
}

// runMaintenanceNow is the job itself, off the scheduler's goroutine: the
// compaction turn can take minutes and watches must keep running meanwhile.
func (s *scheduler) runMaintenanceNow(now time.Time) {
	deps := maintenanceDeps{Notify: s.in.notifyGeneral}
	if r, ok := s.in.runner.(*Runner); ok {
		deps.Turner = r
	}
	rep := runMaintenance(s.in.db, s.in.config(), deps, now)
	listenLog("maintenance: %s", strings.ReplaceAll(strings.TrimSpace(rep.String()), "\n", "; "))
}

// notifyGeneral posts into the group's General topic: maintenance belongs to
// the instance, not to any one bot, so it is not written into a bot's topic.
func (in *instance) notifyGeneral(text string) {
	cfg := in.config()
	if cfg.BotToken == "" || cfg.GroupID == 0 {
		return
	}
	if _, err := sendMessageHTMLGetID(cfg, cfg.GroupID, 0, htmlEscape(text)); err != nil {
		hookLog("maintenance notification: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Doctor loop
// ---------------------------------------------------------------------------

// doctorState remembers what the last probe saw, so the owner is told about a
// TRANSITION into needs_login and not once every fifteen minutes.
type doctorState struct {
	lastRun  time.Time
	reported map[string]bool
	findings []doctorFinding
}

// doctorFinding is one line of the /status "doctor" section.
type doctorFinding struct {
	Profile string
	Problem string
}

// runDoctor probes every profile: login state, the bypass disclaimer, and the
// usage cache (DESIGN §7). A profile that has gone logged-out is excluded from
// selection and the owner gets a Relogin button.
func (s *scheduler) runDoctor(now time.Time) {
	s.doctor.lastRun = now
	if s.doctor.reported == nil {
		s.doctor.reported = map[string]bool{}
	}
	cfg := s.in.config()
	var findings []doctorFinding

	for _, p := range listProfiles(cfg) {
		loggedIn, account, err := profileLoggedIn(p)
		switch {
		case err != nil:
			findings = append(findings, doctorFinding{accountDisplay(p), "could not read auth status: " + truncate(err.Error(), 120)})
		case !loggedIn:
			findings = append(findings, doctorFinding{accountDisplay(p), "not logged in"})
			s.in.markNeedsLogin(p.Name)
			if !s.doctor.reported[p.Name] {
				s.doctor.reported[p.Name] = true
				s.in.notifyNeedsLogin(p, "it is not logged in")
			}
		default:
			// Back in business: clear the flag a failed turn may have set.
			if s.doctor.reported[p.Name] {
				delete(s.doctor.reported, p.Name)
			}
			s.in.clearNeedsLogin(p.Name)
			// The doctor run is where a profile learns (or confirms) which
			// account it holds: that is the name it is shown and addressed by
			// everywhere in Telegram (DESIGN §8).
			if profileEngine(p) == engineClaude {
				p.Name = s.in.rememberProfileEmail(p.Name, account)
			}
		}
		if profileEngine(p) == engineClaude {
			if accepted, known := bypassAccepted(p); known && !accepted {
				findings = append(findings, doctorFinding{accountDisplay(p), "bypass disclaimer not accepted"})
			}
			// Reading the usage cache is what refreshes the numbers chooseProfile
			// and /status use; the result is per-call, so this is the refresh.
			readProfileUsage(p)
		}
	}
	s.doctor.findings = findings
}

// findingsSnapshot is what /status renders.
func (s *scheduler) findingsSnapshot() []doctorFinding {
	if s == nil || s.doctor == nil {
		return nil
	}
	return s.doctor.findings
}

// notifyNeedsLogin DMs the owner with a button that starts the login flow.
func (in *instance) notifyNeedsLogin(p Profile, why string) {
	cfg := in.config()
	if cfg.BotToken == "" || cfg.ChatID == 0 {
		return
	}
	shown := accountDisplay(p)
	body := fmt.Sprintf("🔑 %s account <b>%s</b> needs a new login (%s).", engineLabel(profileEngine(p)), htmlEscape(shown), htmlEscape(why))
	buttons := [][]InlineKeyboardButton{{{Text: "🔑 Relogin " + shown, CallbackData: "account:login:" + accountTarget(p.Name)}}}
	if _, err := sendMessageKeyboardGetID(cfg, cfg.ChatID, 0, body, buttons); err != nil {
		hookLog("needs-login notification failed: %v", err)
	}
}

func (in *instance) markNeedsLogin(name string) {
	if r, ok := in.runner.(*Runner); ok {
		r.markProfileNeedsLogin(name)
	}
}

// ---------------------------------------------------------------------------
// Store helpers used by the MCP tools and the Telegram commands
// ---------------------------------------------------------------------------

// upsertWatch registers or replaces a watch by (bot, name).
func upsertWatch(db *gorm.DB, botID int64, name, command string, intervalS int) (*Watch, error) {
	name = strings.TrimSpace(name)
	command = strings.TrimSpace(command)
	if name == "" || command == "" {
		return nil, fmt.Errorf("a watch needs a name and a command")
	}
	if intervalS < watchMinInterval {
		intervalS = watchMinInterval
	}
	var w Watch
	err := db.Where("bot_id = ? AND name = ?", botID, name).First(&w).Error
	if err == nil {
		updates := map[string]any{"command": command, "interval_s": intervalS, "enabled": true}
		if w.Command != command {
			// A different command means a different baseline; forget the old one
			// so the next run does not report a spurious change.
			updates["last_hash"] = ""
			updates["last_output"] = ""
			updates["last_run_at"] = nil
		}
		if err := db.Model(&Watch{}).Where("id = ?", w.ID).Updates(updates).Error; err != nil {
			return nil, err
		}
		return &w, nil
	}
	w = Watch{BotID: botID, Name: name, Command: command, IntervalS: intervalS, Enabled: true}
	if err := db.Create(&w).Error; err != nil {
		return nil, err
	}
	return &w, nil
}

func deleteWatch(db *gorm.DB, botID int64, name string) (bool, error) {
	res := db.Where("bot_id = ? AND name = ?", botID, strings.TrimSpace(name)).Delete(&Watch{})
	return res.RowsAffected > 0, res.Error
}

func listWatches(db *gorm.DB, botID int64) ([]Watch, error) {
	var out []Watch
	err := db.Where("bot_id = ?", botID).Order("name").Find(&out).Error
	return out, err
}

func listSchedules(db *gorm.DB, botID int64) ([]Schedule, error) {
	var out []Schedule
	err := db.Where("bot_id = ? AND fired_at IS NULL", botID).Order("fire_at").Find(&out).Error
	return out, err
}
