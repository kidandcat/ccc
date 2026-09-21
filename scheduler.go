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
// fires schedules, starts/reaps background jobs and keeps the account health
// up to date. Watches and schedules are deterministic and model-free; a
// finished background job enqueues a source=background turn so the model can
// report to the owner.

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
	bg     bgRuntime
}

func newScheduler(in *instance) *scheduler {
	return &scheduler{in: in, stop: make(chan struct{}), doctor: &doctorState{}}
}

func (s *scheduler) Close() {
	// Leave running jobs alone: they are detached and a new listen reattaches.
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
}

// Run is the loop. It never returns an error: a failing watch is reported to
// its bot, and a failing doctor probe is simply retried next tick.
func (s *scheduler) Run() {
	// Reattach before the doctor probe: leftover running jobs should be
	// supervised (or finished) even if auth checks take a while.
	s.reattachBackgroundJobs()
	// Probe the accounts once at boot so /status is meaningful immediately.
	now := time.Now()
	s.runDoctor(now)
	s.expireWatches(now)
	s.compactIdleSessions(now)
	s.remindIdleSessions(now)
	ticker := time.NewTicker(schedulerTick)
	bgTicker := time.NewTicker(backgroundTick)
	defer ticker.Stop()
	defer bgTicker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case now := <-ticker.C:
			s.compactIdleSessions(now)
			s.remindIdleSessions(now)
			s.runDueWatches(now)
			s.expireWatches(now)
			s.fireDueSchedules(now)
			if now.Sub(s.doctor.lastRun) >= doctorInterval {
				s.runDoctor(now)
			}
			s.runMaintenanceIfDue(now)
		case <-bgTicker.C:
			s.tickBackground()
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

// expireWatches cancels enabled watches older than watch_ttl_s and wakes the
// bot that set them. Routines are named cron on the schedules table and are
// not touched. A zero TTL disables expiry.
func (s *scheduler) expireWatches(now time.Time) {
	ttl := watchTTL(s.in.config())
	if ttl <= 0 {
		return
	}
	var watches []Watch
	if err := s.in.db.Where("enabled = ?", true).Find(&watches).Error; err != nil {
		return
	}
	for i := range watches {
		w := watches[i]
		if !watchExpired(w, now, ttl) {
			continue
		}
		if err := s.in.db.Delete(&Watch{}, w.ID).Error; err != nil {
			hookLog("watch %s: expire delete failed: %v", w.Name, err)
			continue
		}
		b, err := botByID(s.in.db, w.BotID)
		if err != nil || b.ArchivedAt != nil {
			continue
		}
		if isGeneralBot(b) {
			// Re-setting a watch on General is a no-op wakeup of the fattest
			// transcript. The watch is already gone.
			continue
		}
		if _, err := s.in.runner.Enqueue(b.ID, sourceSystem, renderWatchExpired(&w, ttl), 0); err != nil {
			hookLog("watch %s: expire enqueue failed: %v", w.Name, err)
		}
	}
}

// watchExpired is true when the watch has lived past its TTL. A zero CreatedAt
// (legacy row from before the column existed) is treated as already expired
// so a long-running watch does not become immortal by upgrading.
func watchExpired(w Watch, now time.Time, ttl time.Duration) bool {
	if ttl <= 0 || !w.Enabled {
		return false
	}
	if w.CreatedAt.IsZero() {
		return true
	}
	return !w.CreatedAt.After(now.Add(-ttl))
}

func renderWatchExpired(w *Watch, ttl time.Duration) string {
	return fmt.Sprintf(
		"Watch %q timed out after %s and was cancelled.\nCommand: %s\nRe-set it with watch if you still need it. For standing jobs use set_routine.",
		w.Name, humanDuration(ttl), w.Command)
}

// watchExpiryLabel is the "expires in 2h" fragment for /watches and list_watches.
func watchExpiryLabel(w Watch, now time.Time, ttl time.Duration) string {
	if !w.Enabled {
		return "disabled"
	}
	if ttl <= 0 {
		return "no expiry"
	}
	if watchExpired(w, now, ttl) {
		return "expires now"
	}
	left := ttl - now.Sub(w.CreatedAt)
	if left < 0 {
		left = 0
	}
	return "expires in " + humanDuration(left)
}

// compactIdleSessions clears session_id on idle bots whose last turn ended
// longer than idle_compact_s ago. Same effect as /new: memories stay, the
// next turn is a fresh conversation. Skips running and waiting bots (a parked
// ask_owner still needs the transcript). A zero setting disables it.
func (s *scheduler) compactIdleSessions(now time.Time) {
	after := idleCompact(s.in.config())
	if after <= 0 {
		return
	}
	var bots []Bot
	if err := s.in.db.Where("archived_at IS NULL AND session_id != ? AND status = ?", "", botIdle).Find(&bots).Error; err != nil {
		return
	}
	for i := range bots {
		b := bots[i]
		if !sessionIdleTooLong(s.in.db, b.ID, now, after) {
			continue
		}
		res := s.in.db.Model(&Bot{}).Where("id = ? AND status = ? AND session_id != ?", b.ID, botIdle, "").
			Update("session_id", "")
		if res.Error != nil || res.RowsAffected == 0 {
			continue
		}
		if isGeneralBot(&b) {
			if chat, thread, ok := destForTopic(s.in.config(), 0); ok {
				_, _ = sendMessageHTMLGetIDSilent(s.in.config(), chat, thread, renderGeneralRotated(after))
			}
		}
	}
}

func renderGeneralRotated(after time.Duration) string {
	return fmt.Sprintf("🧹 Fresh conversation after %s idle. Memories are kept.", humanDuration(after))
}

// remindIdleSessions wakes General about live workers that are idle (not
// parked on ask_owner), have nothing keeping them alive, and have been
// waiting on the owner for idleRemindInterval. One reminder per idle spell
// (IdleRemindedAt latches until the worker leaves idle-waiting). The nag
// is an inbox row plus an immediate Enqueue on General — the same path as
// report_to_general — so the dispatcher actually runs. Nothing is posted to
// Telegram. Waiting bots already asked; the owner sees them on the next DM.
func (s *scheduler) remindIdleSessions(now time.Time) {
	if s.in.runner == nil {
		return
	}
	chief, err := s.in.ensureGeneralBot()
	if err != nil {
		hookLog("idle remind: General: %v", err)
		return
	}
	var bots []Bot
	if err := s.in.db.Where("archived_at IS NULL").Find(&bots).Error; err != nil {
		return
	}
	for i := range bots {
		b := bots[i]
		if !sessionIdleWaitingOnUser(s.in.db, &b) {
			if b.IdleRemindedAt != nil {
				s.in.db.Model(&Bot{}).Where("id = ?", b.ID).
					Select("idle_reminded_at").Update("idle_reminded_at", nil)
			}
			continue
		}
		if !shouldIdleRemind(s.in.db, &b, now) {
			continue
		}
		if err := s.enqueueIdleRemind(chief, &b, now); err != nil {
			hookLog("idle remind %s: %v", b.Name, err)
			continue
		}
		s.in.db.Model(&Bot{}).Where("id = ?", b.ID).Update("idle_reminded_at", now)
	}
}

// enqueueIdleRemind is the wake path: queueBotMessage (inbox, wake=true) then
// Runner.Enqueue(chief, source=bot, inboxInput) so General runs now. Worker
// reports wait for deliverInbox at end of the sender's turn; an idle worker
// will not start a turn, so the scheduler must Enqueue itself. The inbox row
// is marked delivered to the new turn so a listen restart does not fire twice.
func (s *scheduler) enqueueIdleRemind(chief, worker *Bot, now time.Time) error {
	body := idleRemindText(worker.Name)
	_, msg, err := queueBotMessage(s.in.db, worker, chief.Name, body, true)
	if err != nil {
		return err
	}
	turn, err := s.in.runner.Enqueue(chief.ID, sourceBot, inboxInput(s.in.db, *msg), 0)
	if err != nil {
		s.in.db.Delete(msg)
		return err
	}
	updates := map[string]any{"delivered_at": now}
	if turn != nil {
		updates["turn_id"] = turn.ID
	}
	s.in.db.Model(&InboxMessage{}).Where("id = ?", msg.ID).Updates(updates)
	return nil
}

// sessionIdleWaitingOnUser is the idle-remind predicate: a live worker that
// is idle, has no watch/schedule/routine/background keeping it alive, and has
// no queued work — so it is waiting on the owner. Parked ask_owner
// (botWaiting) is not reminded: the question is already in the DM, and the
// owner sees the list again when they next write. Nagging General would make
// it re-ask.
func sessionIdleWaitingOnUser(db *gorm.DB, b *Bot) bool {
	if b == nil || b.ArchivedAt != nil || isGeneralBot(b) {
		return false
	}
	if b.Status != botIdle {
		return false
	}
	if sessionHasKeepalive(db, b.ID) {
		return false
	}
	var queued int64
	db.Model(&Turn{}).Where("bot_id = ? AND status IN ?", b.ID, []string{turnQueued, turnRunning}).Count(&queued)
	if queued > 0 {
		return false
	}
	var waking int64
	db.Model(&InboxMessage{}).Where("to_bot_id = ? AND delivered_at IS NULL AND wake = ?", b.ID, true).Count(&waking)
	return waking == 0
}

func sessionHasKeepalive(db *gorm.DB, botID int64) bool {
	var n int64
	db.Model(&Watch{}).Where("bot_id = ? AND enabled = ?", botID, true).Count(&n)
	if n > 0 {
		return true
	}
	db.Model(&Schedule{}).Where("bot_id = ? AND fired_at IS NULL", botID).Count(&n)
	if n > 0 {
		return true
	}
	db.Model(&BackgroundJob{}).Where("bot_id = ? AND status IN ?", botID, []string{jobQueued, jobRunning}).Count(&n)
	return n > 0
}

func sessionIdleSince(db *gorm.DB, b *Bot) time.Time {
	var last Turn
	if err := db.Where("bot_id = ? AND ended_at IS NOT NULL", b.ID).Order("ended_at DESC").First(&last).Error; err == nil && last.EndedAt != nil {
		return *last.EndedAt
	}
	return b.CreatedAt
}

func shouldIdleRemind(db *gorm.DB, b *Bot, now time.Time) bool {
	if !sessionIdleWaitingOnUser(db, b) {
		return false
	}
	if b.IdleRemindedAt != nil {
		return false
	}
	return now.Sub(sessionIdleSince(db, b)) >= idleRemindInterval
}

// sessionIdleTooLong is the shared idle check: last finished turn older than
// `after`, and no unanswered ask_owner. Status is the caller's problem (the
// runner has already marked the bot running when it asks).
func sessionIdleTooLong(db *gorm.DB, botID int64, now time.Time, after time.Duration) bool {
	if after <= 0 {
		return false
	}
	var pending int64
	db.Model(&Question{}).Where("bot_id = ? AND answered_at IS NULL", botID).Count(&pending)
	if pending > 0 {
		return false
	}
	var last Turn
	if err := db.Where("bot_id = ? AND ended_at IS NOT NULL", botID).Order("ended_at DESC").First(&last).Error; err != nil {
		return false
	}
	if last.EndedAt == nil {
		return false
	}
	return now.Sub(*last.EndedAt) >= after
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

// instanceEnv is the environment deterministic subprocesses (watches,
// background jobs) run with: the same whitelist as a bot, plus
// env_passthrough, and never a CLAUDE_CONFIG_DIR — nothing here talks to Claude.
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

// fireDueSchedules fires every schedule whose time has come. Unnamed wakeups
// enqueue on the owning bot; named routines start a fresh worker.
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
		name := strings.TrimSpace(sc.Name)
		if name != "" {
			if err := s.startRoutineWorker(b, name, note); err != nil {
				hookLog("schedule %d: routine worker: %v", sc.ID, err)
			}
			s.postRoutineFired(b, sc)
		} else {
			input := fmt.Sprintf("Scheduled wakeup: %s", note)
			if _, err := s.in.runner.Enqueue(b.ID, sourceSchedule, input, 0); err != nil {
				hookLog("schedule %d: enqueue failed: %v", sc.ID, err)
				continue
			}
		}
		// A recurring schedule rolls forward instead of being retired, so one
		// row keeps firing for the life of the bot. Named routines interpret
		// cron in their timezone so 9:00 means 9:00 in Madrid on the UTC VM.
		if sc.RecurringCron != "" {
			if next, err := cronNextIn(sc.RecurringCron, sc.Timezone, now); err == nil {
				s.in.db.Model(&Schedule{}).Where("id = ?", sc.ID).Update("fire_at", next)
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

// notifyGeneral posts into General (the owner's DM): maintenance belongs to
// the instance, not to any one bot.
func (in *instance) notifyGeneral(text string) {
	in.notifyTopic(0, htmlEscape(text))
}

// notifyBot posts owner-facing HTML for a session in General (the DM),
// labelled with the session name when it is not General itself.
func (in *instance) notifyBot(b *Bot, html string) {
	in.notifyBotOpts(b, html, false)
}

func (in *instance) notifyBotSilent(b *Bot, html string) {
	in.notifyBotOpts(b, html, true)
}

func (in *instance) notifyBotOpts(b *Bot, html string, silent bool) {
	if b == nil || strings.TrimSpace(html) == "" {
		return
	}
	body := html
	if !isGeneralBot(b) {
		body = fmt.Sprintf("<b>%s</b> · %s", htmlEscape(b.Name), html)
	}
	in.notifyTopicOpts(0, body, silent)
}

// notifyTopic posts HTML to a Telegram destination (0 = General / the DM).
// The send is a notifying sendMessage: Telegram does not ping on edits, and
// these are the events the owner must actually see (boot, a failed job).
func (in *instance) notifyTopic(topicID int64, html string) {
	in.notifyTopicOpts(topicID, html, false)
}

// notifyTopicSilent is the same post without a Telegram ping. Idle-session
// rotation is bookkeeping, not something that should buzz the owner's phone.
func (in *instance) notifyTopicSilent(topicID int64, html string) {
	in.notifyTopicOpts(topicID, html, true)
}

func (in *instance) notifyTopicOpts(topicID int64, html string, silent bool) {
	cfg := in.config()
	chat, thread, ok := destForTopic(cfg, topicID)
	if !ok || strings.TrimSpace(html) == "" {
		return
	}
	var err error
	if silent {
		_, err = sendMessageHTMLGetIDSilent(cfg, chat, thread, html)
	} else {
		_, err = sendMessageHTMLGetID(cfg, chat, thread, html)
	}
	if err != nil {
		hookLog("topic %d notification: %v", topicID, err)
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
		}
		// Live usage for Claude / Grok / Codex (5 min cache). Antigravity is n/a.
		refreshProfileUsage(p)
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
	buttons := [][]InlineKeyboardButton{{{Text: "🔑 Relogin " + shown, CallbackData: "account:login:" + accountButtonTarget(p)}}}
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
		updates := map[string]any{
			"command":    command,
			"interval_s": intervalS,
			"enabled":    true,
			// Renewing a watch (same name) restarts the TTL, which is how a
			// bot puts back a watch that just timed out (DESIGN §7).
			"created_at": time.Now(),
		}
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
		if err := db.Where("id = ?", w.ID).First(&w).Error; err != nil {
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
