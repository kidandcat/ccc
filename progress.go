package main

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// progress is the single per-turn Telegram message that is edited in place
// while the turn runs (DESIGN §3.2): current activity plus elapsed time, at
// most one edit every progressInterval. It is posted silently; when the turn
// ends it is deleted and the final text is a new notifying message.

const progressInterval = 3 * time.Second

// telegramTextLimit is Telegram's hard per-message cap; ccc sends with the
// smaller telegramChunkLimit so a split never brushes against it.
const telegramTextLimit = 4096

// messageDeleter is the optional half of botUI: a surface that can retract one
// of its own messages. Telegram can; the test doubles need not.
type messageDeleter interface {
	Delete(topicID, msgID int64) error
}

// silentPoster posts without a Telegram notification. Progress uses it so the
// "⏳ working" message does not ping; the final answer goes through Post.
type silentPoster interface {
	PostSilent(topicID int64, html string) (int64, error)
}

func postProgress(ui botUI, topicID int64, html string) (int64, error) {
	if s, ok := ui.(silentPoster); ok {
		return s.PostSilent(topicID, html)
	}
	return ui.Post(topicID, html)
}

func postOverflow(ui botUI, topicID int64, html string) (int64, error) {
	return postProgress(ui, topicID, html)
}

// Delete retracts a message from the bot's topic. It lives next to the only
// caller rather than with the rest of telegramUI.
func (t telegramUI) Delete(_ int64, msgID int64) error {
	cfg := t.in.config()
	if cfg.BotToken == "" || cfg.GroupID == 0 || msgID == 0 {
		return nil
	}
	return deleteMessage(cfg, cfg.GroupID, msgID)
}

type progress struct {
	ui      botUI
	topicID int64
	started time.Time

	mu       sync.Mutex
	msgID    int64
	activity string
	lastEdit time.Time
	shown    string
	created  bool
	finished bool
}

func newProgress(ui botUI, topicID int64, started time.Time) *progress {
	return &progress{ui: ui, topicID: topicID, started: started}
}

// set records the current activity and flushes it if the rate limit allows.
func (p *progress) set(activity string) {
	if p == nil || p.ui == nil {
		return
	}
	p.mu.Lock()
	p.activity = activity
	p.mu.Unlock()
	p.flush(false)
}

func (p *progress) flush(force bool) {
	p.mu.Lock()
	if p.finished {
		p.mu.Unlock()
		return
	}
	now := time.Now()
	if !force && p.created && now.Sub(p.lastEdit) < progressInterval {
		p.mu.Unlock()
		return
	}
	text := renderProgress(p.activity, now.Sub(p.started))
	if text == p.shown {
		p.mu.Unlock()
		return
	}
	p.shown = text
	p.lastEdit = now
	msgID := p.msgID
	created := p.created
	p.mu.Unlock()

	if !created {
		id, err := postProgress(p.ui, p.topicID, text)
		if err != nil {
			return
		}
		p.mu.Lock()
		p.msgID = id
		p.created = true
		p.mu.Unlock()
		return
	}
	_ = p.ui.Edit(p.topicID, msgID, text) // safe-ignore: a failed progress edit is cosmetic
}

// finish replaces the progress message with the turn's final text, sending any
// overflow beyond one Telegram message as follow-ups.
//
// The model writes markdown-ish plain text, so it is rendered into Telegram's
// HTML subset first: handing the raw text to a parse_mode=HTML send is what
// used to make Telegram reject — and ccc silently drop — every reply that
// happened to contain a "<".
func (p *progress) finish(final string) {
	if p == nil || p.ui == nil {
		return
	}
	final = strings.TrimSpace(final)
	if final == "" {
		final = "(no reply)"
	}
	chunks := splitTelegramHTML(renderTelegramHTML(final), telegramChunkLimit)

	p.mu.Lock()
	p.finished = true
	msgID := p.msgID
	created := p.created
	p.mu.Unlock()

	// Telegram does not notify on editMessageText, so the final answer is a
	// new sendMessage (the one ping the owner gets). The silent progress
	// message is then deleted. Overflow chunks stay silent.
	if _, err := p.ui.Post(p.topicID, chunks[0]); err != nil {
		log.Printf("progress: reply chunk 1/%d (%d bytes) could not be delivered: %v", len(chunks), len(chunks[0]), err)
	}
	for i, c := range chunks[1:] {
		if _, err := postOverflow(p.ui, p.topicID, c); err != nil {
			log.Printf("progress: reply chunk %d/%d (%d bytes) could not be delivered: %v", i+2, len(chunks), len(c), err)
		}
	}
	if created {
		p.retireProgress(msgID)
	}
}

// retireProgress clears the stale "⏳ working" message left behind when the
// final reply had to be posted as a new message instead of edited in.
func (p *progress) retireProgress(msgID int64) {
	if d, ok := p.ui.(messageDeleter); ok {
		err := d.Delete(p.topicID, msgID)
		if err == nil {
			return
		}
		log.Printf("progress: could not delete the stale progress message %d: %v", msgID, err)
	}
	if err := p.ui.Edit(p.topicID, msgID, "✅ replied below"); err != nil {
		log.Printf("progress: could not retire the stale progress message %d: %v", msgID, err)
	}
}

func renderProgress(activity string, elapsed time.Duration) string {
	if activity == "" {
		activity = "working"
	}
	return fmt.Sprintf("⏳ <i>%s</i> · %s", htmlEscape(activity), humanDuration(elapsed))
}

func humanDuration(d time.Duration) string {
	s := int(d.Round(time.Second).Seconds())
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	if s < 3600 {
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh%02dm", s/3600, (s%3600)/60)
}
