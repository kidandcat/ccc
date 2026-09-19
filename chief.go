package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
)

// General is the persistent dispatcher session. The owner's 1:1 DM with the
// bot IS General. Sessions live in the backend (TopicID != 0); they have no
// Telegram chat. New workers get TopicID = -id so they cannot collide with
// General (0).
const generalBotName = "General"

func isGeneralBot(b *Bot) bool {
	return b != nil && b.TopicID == 0
}

// markBackendTopic assigns a negative TopicID so a worker cannot collide
// with General (0). Call after the row has an id.
func markBackendTopic(db *gorm.DB, b *Bot) error {
	if b == nil || b.ID == 0 {
		return fmt.Errorf("bot has no id")
	}
	if b.TopicID != 0 {
		return nil
	}
	id := -b.ID
	if err := db.Model(b).Update("topic_id", id).Error; err != nil {
		return err
	}
	b.TopicID = id
	return nil
}

// chiefTurnTimeout caps one General turn. Workers have no such cap. Tests may
// shorten it so they do not wait 60s.
var chiefTurnTimeout = 60 * time.Second

const (
	chiefTimeoutClass  = "chief_timeout"
	idleRemindInterval = 10 * time.Minute
)

func chiefTimeoutFor(b *Bot, t *Turn) time.Duration {
	if !isGeneralBot(b) {
		return 0
	}
	// The cap is for owner work. Session reports, idle nags, watches and
	// schedules are the dispatcher's job: killing them at 60s left the owner
	// with only "session ended" while General never summarized.
	if t != nil && t.Source != sourceUser && !isChiefTimeoutFollowUp(t.Input) {
		return 0
	}
	return chiefTurnTimeout
}

// chiefTimeoutInput is injected as the next General turn when the 60s cap
// fires. It is an error the dispatcher must act on, not a silent kill.
func chiefTimeoutInput() string {
	return "Error: this work is too long for General (60s cap). " +
		"You MUST pass it to a session with spawn_session (new) or tell_session (existing). " +
		"Do not continue the work yourself."
}

// chiefTimeoutInputAfterSpawn is the inject when ccc already started the
// worker: General still gets a turn so it knows, but must not spawn another.
func chiefTimeoutInputAfterSpawn(name string) string {
	return "Error: this work is too long for General (60s cap). " +
		fmt.Sprintf("CCC started session %q with the owner's request. ", name) +
		"Do not do the work yourself and do not spawn a duplicate. " +
		"tell_session if you need to add context."
}

func isChiefTimeoutFollowUp(input string) bool {
	return strings.Contains(input, "too long for General")
}

// chiefTimeoutWorkerPrompt is the first turn of a worker ccc started because
// General timed out without spawn_session / tell_session.
func chiefTimeoutWorkerPrompt(owner string) string {
	owner = strings.TrimSpace(owner)
	msg := "General hit its 60s cap and did not hand this off. Do the work."
	if owner == "" {
		return msg
	}
	return msg + "\n\n" + owner
}

func chiefTimeoutWindowStart(t *Turn) time.Time {
	if t == nil {
		return time.Time{}
	}
	if t.StartedAt != nil {
		return *t.StartedAt
	}
	return t.CreatedAt
}

// chiefTimeoutShouldSpawn is true when the timed-out turn was owner work
// (or the injected follow-up of that episode). Session reports, watches and
// schedules are not owner work: auto-spawning them duplicates turns.
func chiefTimeoutShouldSpawn(t *Turn, input string) bool {
	if t != nil && t.Source == sourceUser {
		return true
	}
	return isChiefTimeoutFollowUp(input)
}

// lastOwnerRequest is the owner's text General was dispatching. Session
// reports, watches, schedules, routines and the timeout inject itself are
// not that. Empty means there is nothing to hand off.
func lastOwnerRequest(db *gorm.DB, botID int64, timedOut *Turn) string {
	if timedOut != nil && timedOut.Source == sourceUser {
		if s := strings.TrimSpace(timedOut.Input); s != "" && !isChiefTimeoutFollowUp(s) {
			return s
		}
	}
	var turns []Turn
	if err := db.Where("bot_id = ? AND source = ?", botID, sourceUser).
		Order("id DESC").Limit(30).Find(&turns).Error; err != nil {
		return ""
	}
	for _, t := range turns {
		if isChiefTimeoutFollowUp(t.Input) {
			continue
		}
		if s := strings.TrimSpace(t.Input); s != "" {
			return s
		}
	}
	return ""
}

// chiefHandoffSince is the start of this timeout episode: the original
// General turn if this is a follow-up, otherwise the current turn. Inbox
// rows from General after that instant count as spawn_session / tell_session.
func chiefHandoffSince(db *gorm.DB, chiefID int64, current *Turn, input string) time.Time {
	since := chiefTimeoutWindowStart(current)
	if !isChiefTimeoutFollowUp(input) {
		return since
	}
	var orig Turn
	q := db.Where("bot_id = ? AND error_class = ?", chiefID, chiefTimeoutClass)
	if current != nil && current.ID != 0 {
		q = q.Where("id < ?", current.ID)
	}
	if q.Order("id DESC").First(&orig).Error != nil {
		return since
	}
	if t := chiefTimeoutWindowStart(&orig); !t.IsZero() && (since.IsZero() || t.Before(since)) {
		return t
	}
	return since
}

func chiefHandedOffSince(db *gorm.DB, chiefID int64, since time.Time) bool {
	q := db.Model(&InboxMessage{}).Where("from_bot_id = ? AND wake = ?", chiefID, true)
	if !since.IsZero() {
		q = q.Where("created_at >= ?", since)
	}
	var n int64
	q.Count(&n)
	return n > 0
}

func idleRemindText(name string) string {
	return fmt.Sprintf("Idle session %q is still waiting on the owner. Decide: ask_owner, tell_session, archive it, or ignore.", name)
}

func generalBot(db *gorm.DB) (*Bot, error) {
	var b Bot
	err := db.Where("topic_id = ? AND archived_at IS NULL", int64(0)).Order("id").First(&b).Error
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// ensureGeneralBot returns the dispatcher row, creating it if needed.
// General is the owner's 1:1 DM; it has no Telegram topic.
func (in *instance) ensureGeneralBot() (*Bot, error) {
	return ensureGeneralBotRow(in.db, in.config())
}

func ensureGeneralBotRow(db *gorm.DB, cfg *Config) (*Bot, error) {
	if b, err := generalBot(db); err == nil {
		return b, nil
	}
	cwd := botWorkspace(cfg, generalBotName)
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		return nil, err
	}
	name := uniqueBotName(db, generalBotName)
	b := &Bot{
		Name:    name,
		TopicID: 0,
		Cwd:     cwd,
		Engine:  defaultEngine(cfg),
		Status:  botIdle,
	}
	if err := db.Create(b).Error; err != nil {
		if existing, err2 := generalBot(db); err2 == nil {
			return existing, nil
		}
		return nil, err
	}
	return b, nil
}

// sessionLine is one live worker for the chief's envelope / list_sessions.
type sessionLine struct {
	Name   string
	Status string
	Engine string
	Age    string
	Last   string
}

func sessionRoster(db *gorm.DB, exceptID int64) []sessionLine {
	bots, err := liveBots(db)
	if err != nil {
		return nil
	}
	out := make([]sessionLine, 0, len(bots))
	for i := range bots {
		b := &bots[i]
		if b.ID == exceptID || isGeneralBot(b) {
			continue
		}
		line := sessionLine{Name: b.Name, Status: b.Status, Engine: botEngine(b)}
		var last Turn
		q := db.Where("bot_id = ? AND status = ?", b.ID, turnDone).Order("id DESC")
		if q.First(&last).Error == nil {
			line.Last = truncate(collapseWhitespace(last.Output), 120)
			if last.EndedAt != nil {
				line.Age = humanDuration(time.Since(*last.EndedAt))
			} else {
				line.Age = humanDuration(time.Since(last.CreatedAt))
			}
		}
		out = append(out, line)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func formatSessionRoster(lines []sessionLine) string {
	if len(lines) == 0 {
		return "no live sessions"
	}
	var sb strings.Builder
	for _, l := range lines {
		engine := l.Engine
		if engine == engineClaude {
			engine = ""
		}
		fmt.Fprintf(&sb, "%s [%s]", l.Name, l.Status)
		if engine != "" {
			fmt.Fprintf(&sb, " %s", engine)
		}
		if l.Age != "" {
			fmt.Fprintf(&sb, " · last %s ago", l.Age)
		}
		if l.Last != "" {
			fmt.Fprintf(&sb, " — %s", l.Last)
		}
		sb.WriteByte('\n')
	}
	return strings.TrimRight(sb.String(), "\n")
}
