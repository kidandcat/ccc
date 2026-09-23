package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gorm.io/gorm"
)

// errRoutineBusy means the previous fire of this routine is still running.
// The caller must not clear session_id or enqueue another turn on top of it.
var errRoutineBusy = errors.New("routine worker still running")

// defaultRoutineTZ is what a routine uses when the bot omits timezone.
// Both the Mac and the work VM must fire "9:00" as 9:00 in Madrid, not
// whatever the box happens to have as time.Local (the VM is UTC).
const defaultRoutineTZ = "Europe/Madrid"

func routineSource(name string) string {
	return sourceRoutine + ":" + name
}

func sanitizeRoutineName(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-_")
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

func tzLocation(name string) *time.Location {
	name = strings.TrimSpace(name)
	if name == "" {
		return time.Local
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.Local
	}
	return loc
}

// cronNextIn is the next fire time of expr after `from`, interpreted in tz
// (empty = the process local zone, which is what unnamed schedule_wakeup uses).
func cronNextIn(expr, tz string, from time.Time) (time.Time, error) {
	schedule, err := parseCron(expr)
	if err != nil {
		return time.Time{}, err
	}
	return schedule.Next(from.In(tzLocation(tz))), nil
}

func upsertRoutine(db *gorm.DB, botID int64, name, prompt, cronExpr, tz string, now time.Time) (*Schedule, error) {
	name = sanitizeRoutineName(name)
	prompt = strings.TrimSpace(prompt)
	cronExpr = strings.TrimSpace(cronExpr)
	if name == "" {
		return nil, fmt.Errorf("a routine needs a name")
	}
	if prompt == "" {
		return nil, fmt.Errorf("a routine needs a prompt (what to do when it fires)")
	}
	if cronExpr == "" {
		return nil, fmt.Errorf("a routine needs a cron expression")
	}
	tz = strings.TrimSpace(tz)
	if tz == "" {
		tz = defaultRoutineTZ
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return nil, fmt.Errorf("unknown timezone %q", tz)
	}
	fireAt, err := cronNextIn(cronExpr, tz, now)
	if err != nil {
		return nil, fmt.Errorf("cannot parse cron %q: %w", cronExpr, err)
	}
	// UTC so a later reader does not depend on which offset the driver
	// happened to serialize. Comparison is still by instant, not text.
	fireAt = fireAt.UTC()
	prompt = truncate(prompt, 2000)
	var row Schedule
	err = db.Where("bot_id = ? AND name = ?", botID, name).First(&row).Error
	if err == nil {
		updates := map[string]any{
			"note": prompt, "recurring_cron": cronExpr, "timezone": tz,
			"fire_at": fireAt, "fired_at": nil,
		}
		if err := db.Model(&Schedule{}).Where("id = ?", row.ID).Updates(updates).Error; err != nil {
			return nil, err
		}
		row.Note = prompt
		row.RecurringCron = cronExpr
		row.Timezone = tz
		row.FireAt = fireAt
		row.FiredAt = nil
		return &row, nil
	}
	row = Schedule{
		BotID: botID, Name: name, Note: prompt,
		RecurringCron: cronExpr, Timezone: tz, FireAt: fireAt,
	}
	if err := db.Create(&row).Error; err != nil {
		return nil, err
	}
	return &row, nil
}

func listRoutines(db *gorm.DB, botID int64) ([]Schedule, error) {
	var out []Schedule
	err := db.Where("bot_id = ? AND name != '' AND fired_at IS NULL", botID).Order("name").Find(&out).Error
	return out, err
}

func cancelRoutine(db *gorm.DB, botID int64, name string) (bool, error) {
	name = sanitizeRoutineName(name)
	if name == "" {
		return false, fmt.Errorf("a routine name is required")
	}
	res := db.Where("bot_id = ? AND name = ?", botID, name).Delete(&Schedule{})
	return res.RowsAffected > 0, res.Error
}

func (s *scheduler) postRoutineFired(b *Bot, sc Schedule) {
	html := fmt.Sprintf("⏰ routine <code>%s</code>", htmlEscape(sc.Name))
	s.in.notifyBot(b, html)
}

// routineWorkerPrompt is the first turn of the isolated session a routine
// fire starts. Short on purpose: the whole point is not inheriting General.
func routineWorkerPrompt(name, note string) string {
	return fmt.Sprintf(
		"Scheduled routine %q. Do this work yourself (you are not General). When finished, report_to_general with a readable owner-facing digest (short sections + bullets, not one compressed paragraph) and archive_bot. Do not spawn_session and do not wait for more input. Do not also notify_owner unless something is urgent or red — General will post the digest as-is.\n\n%s",
		name, note,
	)
}

// startRoutineWorker enqueues the routine on an isolated worker named
// routine-<name>. Named routines must not run as a turn of the owning bot
// (usually General): that re-reads the dispatcher's whole transcript.
// A live or archived worker with that name is reused with session_id
// cleared so a daily fire does not reread yesterday or pile -2/-3 sessions.
func (s *scheduler) startRoutineWorker(owner *Bot, name, note string) error {
	if s == nil || s.in == nil || s.in.runner == nil {
		return fmt.Errorf("no runner")
	}
	if owner == nil {
		return fmt.Errorf("unknown owner")
	}
	sessionName := "routine-" + sanitizeRoutineName(name)
	b, err := s.routineWorkerBot(owner, sessionName)
	if err != nil {
		return err
	}
	prompt := routineWorkerPrompt(name, note)
	if _, err := s.in.runner.Enqueue(b.ID, routineSource(name), prompt, 0); err != nil {
		return err
	}
	return nil
}

func (s *scheduler) routineWorkerBot(owner *Bot, sessionName string) (*Bot, error) {
	var existing Bot
	err := s.in.db.Where("name = ?", sessionName).First(&existing).Error
	if err == nil && !isGeneralBot(&existing) {
		if s.routineWorkerBusy(&existing) {
			return nil, errRoutineBusy
		}
		if existing.ArchivedAt != nil {
			if err := unarchiveBotRow(s.in.db, existing.ID); err != nil {
				return nil, err
			}
		}
		if err := s.in.db.Model(&Bot{}).Where("id = ?", existing.ID).
			Updates(map[string]any{"session_id": "", "status": botIdle}).Error; err != nil {
			return nil, err
		}
		existing.SessionID = ""
		existing.Status = botIdle
		existing.ArchivedAt = nil
		return &existing, nil
	}
	b, err := createBotRow(s.in.db, s.in.config(), sessionName, "", "")
	if err != nil {
		return nil, err
	}
	if eng := botEngine(owner); eng != "" && botEngine(b) != eng {
		if err := s.in.db.Model(&Bot{}).Where("id = ?", b.ID).Update("engine", eng).Error; err != nil {
			_ = archiveBotRow(s.in.db, b.ID) // safe-ignore: empty worker must not linger
			return nil, err
		}
		b.Engine = eng
	}
	return b, nil
}

func (s *scheduler) routineWorkerBusy(b *Bot) bool {
	if b == nil || s == nil || s.in == nil {
		return false
	}
	if r, ok := s.in.runner.(*Runner); ok && r.Running(b.ID) {
		return true
	}
	// A queued turn has not minted a session yet. Only a turn that is
	// actually running will write its session id back after we clear it.
	if b.Status == botRunning {
		return true
	}
	if s.in.db == nil {
		return false
	}
	var n int64
	s.in.db.Model(&Turn{}).Where("bot_id = ? AND status = ?", b.ID, turnRunning).Count(&n)
	return n > 0
}

// ---------------------------------------------------------------------------
// `ccc routine` — Grok/agy substitute for set_routine / list / cancel
// ---------------------------------------------------------------------------

func runRoutineCommand(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: ccc routine add|list|cancel …")
	}
	db, _, from, err := openSenderStore()
	if err != nil {
		return err
	}
	defer closeStore(db)

	switch args[0] {
	case "add":
		name, cronExpr, tz, prompt, err := parseRoutineAdd(args[1:])
		if err != nil {
			return err
		}
		row, err := upsertRoutine(db, from.ID, name, prompt, cronExpr, tz, time.Now())
		if err != nil {
			return err
		}
		fmt.Printf("routine %s next %s (%s %s)\n", row.Name, row.FireAt.Format(time.RFC3339), row.RecurringCron, row.Timezone)
		return nil
	case "list":
		rows, err := listRoutines(db, from.ID)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			fmt.Println("no routines")
			return nil
		}
		for _, r := range rows {
			fmt.Printf("%s  next %s  %s %s\n  %s\n", r.Name, r.FireAt.Format(time.RFC3339), r.RecurringCron, r.Timezone, truncate(r.Note, 120))
		}
		return nil
	case "cancel", "remove":
		if len(args) < 2 {
			return fmt.Errorf("usage: ccc routine cancel <name>")
		}
		ok, err := cancelRoutine(db, from.ID, args[1])
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no routine named %q", sanitizeRoutineName(args[1]))
		}
		fmt.Printf("cancelled %s\n", sanitizeRoutineName(args[1]))
		return nil
	default:
		return fmt.Errorf("usage: ccc routine add|list|cancel …")
	}
}

func parseRoutineAdd(args []string) (name, cronExpr, tz, prompt string, err error) {
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			rest = append(rest, args[i+1:]...)
			i = len(args)
		case a == "--cron" && i+1 < len(args):
			cronExpr = args[i+1]
			i++
		case strings.HasPrefix(a, "--cron="):
			cronExpr = strings.TrimPrefix(a, "--cron=")
		case a == "--tz" && i+1 < len(args):
			tz = args[i+1]
			i++
		case strings.HasPrefix(a, "--tz="):
			tz = strings.TrimPrefix(a, "--tz=")
		case a == "--prompt" && i+1 < len(args):
			prompt = args[i+1]
			i++
		case strings.HasPrefix(a, "--prompt="):
			prompt = strings.TrimPrefix(a, "--prompt=")
		case strings.HasPrefix(a, "-"):
			return "", "", "", "", fmt.Errorf("unknown flag %s", a)
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) == 0 {
		return "", "", "", "", fmt.Errorf("usage: ccc routine add <name> --cron \"0 9 * * 1-5\" [--tz Europe/Madrid] <prompt>")
	}
	name = rest[0]
	body := strings.TrimSpace(strings.Join(rest[1:], " "))
	if prompt == "" {
		prompt = body
	}
	if prompt == "" {
		b, readErr := io.ReadAll(os.Stdin)
		if readErr != nil {
			return "", "", "", "", fmt.Errorf("read stdin: %w", readErr)
		}
		prompt = strings.TrimSpace(string(b))
	}
	if cronExpr == "" || prompt == "" {
		return "", "", "", "", fmt.Errorf("usage: ccc routine add <name> --cron \"0 9 * * 1-5\" [--tz Europe/Madrid] <prompt>")
	}
	return name, cronExpr, tz, prompt, nil
}

func openSenderStore() (*gorm.DB, *Config, *Bot, error) {
	config, err := loadConfig()
	if err != nil || config == nil {
		return nil, nil, nil, fmt.Errorf("no config found: %w", err)
	}
	path := os.Getenv("CCC_DB")
	if path == "" {
		path = dbPath(config)
	}
	db, err := openStore(path)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open the ccc database: %w", err)
	}
	from, err := senderBot(db)
	if err != nil {
		closeStore(db)
		return nil, nil, nil, err
	}
	return db, config, from, nil
}
