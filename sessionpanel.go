package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
)

// The live session panel is one silent Telegram message in the owner's DM.
// Each working session is a card (name, status, current line). The message is
// pinned while any worker is running or has a background job, and unpinned
// when that work ends. A session parked on ask_owner is not a card: the
// question is already one message in the DM, and it is not repeated here.
// /sessions stays the full list. Workers still do not dump transcripts into
// the DM (DESIGN §3.2).

const (
	settingSessionPanelMsgID = "session_panel_msg_id"
	panelMaxCards            = 8
	panelLineMax             = 80
)

// panelSurface is the Telegram half the live card needs. Production uses
// telegramUI. Tests use fakeUI.
type panelSurface interface {
	PostSilent(topicID int64, html string) (int64, error)
	Edit(topicID, msgID int64, html string) error
	Pin(msgID int64) error
	Unpin(msgID int64) error
}

type sessionPanel struct {
	db *gorm.DB
	ui panelSurface

	mu       sync.Mutex
	activity map[int64]string

	drawMu   sync.Mutex
	lastHTML string
	lastEdit time.Time
	msgID    int64
	pinned   bool
}

type sessionCard struct {
	Name    string
	Status  string
	Engine  string
	Line    string
	Elapsed string
}

func newSessionPanel(db *gorm.DB, ui panelSurface) *sessionPanel {
	p := &sessionPanel{db: db, ui: ui, activity: map[int64]string{}}
	if db != nil {
		p.msgID = int64(getSettingInt(db, settingSessionPanelMsgID, 0))
		// A previous listen may have left this message pinned.
		p.pinned = p.msgID != 0
	}
	return p
}

func panelSurfaceOf(ui botUI) panelSurface {
	switch v := ui.(type) {
	case panelSurface:
		return v
	default:
		return nil
	}
}

func (p *sessionPanel) setActivity(botID int64, activity string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.activity == nil {
		p.activity = map[int64]string{}
	}
	p.activity[botID] = activity
	p.mu.Unlock()
	p.sync(false)
}

func (p *sessionPanel) clearActivity(botID int64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	delete(p.activity, botID)
	p.mu.Unlock()
}

// sync redraws the pinned panel from live workers. force skips the progress
// rate limit (status changes); activity ticks stay at progressInterval.
func (p *sessionPanel) sync(force bool) {
	if p == nil || p.ui == nil || p.db == nil {
		return
	}
	cards := p.snapshot()
	html := renderSessionPanel(cards)
	working := panelHasWork(cards)

	p.drawMu.Lock()
	defer p.drawMu.Unlock()
	if !force && p.msgID != 0 && html == p.lastHTML {
		return
	}
	if !force && p.msgID != 0 && !p.lastEdit.IsZero() && time.Since(p.lastEdit) < progressInterval {
		return
	}
	msgID := p.msgID
	if html == "" {
		p.clear()
		p.lastEdit = time.Now()
		return
	}
	p.lastHTML = html
	p.lastEdit = time.Now()
	if err := p.upsert(msgID, html); err != nil {
		hookLog("session panel: %v", err)
		return
	}
	if working {
		p.pin()
	} else {
		p.unpin()
	}
}

func (p *sessionPanel) upsert(msgID int64, html string) error {
	if p == nil || p.ui == nil {
		return nil
	}
	if msgID != 0 {
		err := p.ui.Edit(0, msgID, html)
		if err == nil {
			return nil
		}
		// Deleted or too old to edit: post a replacement.
		hookLog("session panel: edit %d failed (%v); posting a new card", msgID, err)
		p.msgID = 0
		p.pinned = false
	}
	id, err := p.ui.PostSilent(0, html)
	if err != nil {
		return err
	}
	if id == 0 {
		return nil
	}
	p.msgID = id
	if p.db != nil {
		_ = setSetting(p.db, settingSessionPanelMsgID, strconv.FormatInt(id, 10)) // safe-ignore: a lost id just posts a new card next time
	}
	return nil
}

func (p *sessionPanel) pin() {
	id := p.msgID
	if id == 0 || p.pinned {
		return
	}
	if err := p.ui.Pin(id); err != nil {
		hookLog("session panel: pin %d: %v", id, err)
		return
	}
	p.pinned = true
}

func (p *sessionPanel) unpin() {
	id := p.msgID
	if id == 0 || !p.pinned {
		return
	}
	if err := p.ui.Unpin(id); err != nil {
		hookLog("session panel: unpin %d: %v", id, err)
	}
	p.pinned = false
}

// clear drops a card that has nothing left to show, so a previous ask_owner
// line cannot stay in the DM after the session is no longer working.
func (p *sessionPanel) clear() {
	id := p.msgID
	if id != 0 {
		if err := p.ui.Edit(0, id, "·"); err != nil {
			hookLog("session panel: clear %d: %v", id, err)
		}
	}
	p.lastHTML = ""
	p.unpin()
}

func (p *sessionPanel) snapshot() []sessionCard {
	bots, err := liveBots(p.db)
	if err != nil {
		return nil
	}
	p.mu.Lock()
	acts := make(map[int64]string, len(p.activity))
	for k, v := range p.activity {
		acts[k] = v
	}
	p.mu.Unlock()

	var jobs []BackgroundJob
	p.db.Where("status = ?", jobRunning).Find(&jobs)
	jobsByBot := map[int64][]BackgroundJob{}
	for i := range jobs {
		jobsByBot[jobs[i].BotID] = append(jobsByBot[jobs[i].BotID], jobs[i])
	}

	now := time.Now()
	var cards []sessionCard
	for i := range bots {
		b := &bots[i]
		if isGeneralBot(b) {
			continue
		}
		c := sessionCard{Name: b.Name, Status: b.Status, Engine: botEngine(b)}
		hot := false
		switch b.Status {
		case botRunning:
			hot = true
			c.Line = acts[b.ID]
			if c.Line == "" {
				c.Line = "working"
			}
			c.Elapsed = runningElapsed(p.db, b.ID, now)
		case botWaiting:
			// The question message is the ask. Do not pin a second copy.
		default:
			if act := strings.TrimSpace(acts[b.ID]); act != "" {
				hot = true
				c.Status = botRunning
				c.Line = act
			}
		}
		if js := jobsByBot[b.ID]; len(js) > 0 {
			hot = true
			if c.Status == botIdle || c.Status == botWaiting {
				c.Status = "job"
			}
			if c.Line == "" {
				j := js[0]
				c.Line = strings.TrimSpace(j.Name)
				if c.Line == "" {
					c.Line = j.Command
				}
				if j.StartedAt != nil {
					c.Elapsed = humanDuration(now.Sub(*j.StartedAt))
				}
			}
		}
		if !hot {
			continue
		}
		cards = append(cards, c)
	}
	sort.Slice(cards, func(i, j int) bool {
		ri, rj := cardRank(cards[i].Status), cardRank(cards[j].Status)
		if ri != rj {
			return ri < rj
		}
		return cards[i].Name < cards[j].Name
	})
	if len(cards) > panelMaxCards {
		cards = cards[:panelMaxCards]
	}
	return cards
}

func runningElapsed(db *gorm.DB, botID int64, now time.Time) string {
	var t Turn
	if err := db.Where("bot_id = ? AND status = ?", botID, turnRunning).Order("id DESC").First(&t).Error; err != nil {
		return ""
	}
	if t.StartedAt == nil {
		return ""
	}
	return humanDuration(now.Sub(*t.StartedAt))
}

func cardRank(status string) int {
	switch status {
	case botWaiting:
		return 0
	case botRunning, "started":
		return 1
	case "job":
		return 2
	case "error", "timed out":
		return 3
	default:
		return 4
	}
}

func panelHasWork(cards []sessionCard) bool {
	for _, c := range cards {
		switch c.Status {
		case botRunning, botWaiting, "job", "started":
			return true
		}
	}
	return false
}

func renderSessionPanel(cards []sessionCard) string {
	if len(cards) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, c := range cards {
		if i > 0 {
			sb.WriteByte('\n')
		}
		fmt.Fprintf(&sb, "%s <b>%s</b>", cardGlyph(c.Status), htmlEscape(c.Name))
		if c.Engine != "" && c.Engine != engineClaude {
			fmt.Fprintf(&sb, " · %s", htmlEscape(c.Engine))
		}
		fmt.Fprintf(&sb, " · %s", htmlEscape(cardStatusLabel(c.Status)))
		if c.Elapsed != "" {
			fmt.Fprintf(&sb, " · %s", htmlEscape(c.Elapsed))
		}
		if line := strings.TrimSpace(c.Line); line != "" {
			fmt.Fprintf(&sb, "\n<i>%s</i>", htmlEscape(truncate(collapseWhitespace(line), panelLineMax)))
		}
		if i+1 < len(cards) {
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

func cardGlyph(status string) string {
	switch status {
	case botWaiting:
		return "❓"
	case "job":
		return "⚙️"
	case "done":
		return "✅"
	case "error":
		return "❌"
	case "timed out":
		return "⏱"
	default:
		return "⏳"
	}
}

func cardStatusLabel(status string) string {
	switch status {
	case botRunning:
		return "running"
	case botWaiting:
		return "waiting"
	case "job":
		return "job"
	case "done":
		return "done"
	case "error":
		return "error"
	case "timed out":
		return "timed out"
	case "started":
		return "started"
	default:
		return status
	}
}

// terminalCard is the leftover line after a worker turn ends with no other
// work still going, so the unpinned message is not stuck on "running".
func terminalCard(b *Bot, status string) sessionCard {
	if b == nil {
		return sessionCard{}
	}
	return sessionCard{Name: b.Name, Status: status, Engine: botEngine(b)}
}

func (p *sessionPanel) showTerminal(b *Bot, status string) {
	if p == nil || p.ui == nil || b == nil {
		return
	}
	p.clearActivity(b.ID)
	cards := p.snapshot()
	if panelHasWork(cards) {
		p.sync(true)
		return
	}
	p.drawMu.Lock()
	defer p.drawMu.Unlock()
	html := renderSessionPanel([]sessionCard{terminalCard(b, status)})
	p.lastHTML = html
	p.lastEdit = time.Now()
	if html == "" {
		p.clear()
		return
	}
	if err := p.upsert(p.msgID, html); err != nil {
		hookLog("session panel: %v", err)
		return
	}
	p.unpin()
}

func (r *Runner) syncPanel(force bool) {
	if r == nil || r.panel == nil {
		return
	}
	r.panel.sync(force)
}

func (in *instance) syncPanel(force bool) {
	if in == nil || in.panel == nil {
		return
	}
	in.panel.sync(force)
}
