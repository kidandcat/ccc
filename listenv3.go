package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gorm.io/gorm"
)

// listenv3.go is the Telegram side of ccc v3 (DESIGN §8): the owner's 1:1 DM
// is General, the dispatcher. Sessions live in the backend; spawn_session
// (and /session) start a worker without a Telegram chat. Group messages are
// ignored.

// instance is one `ccc listen` process: config + database + runner.
type instance struct {
	db      *gorm.DB
	runner  turnRunner
	dataDir string
	// login is the at-most-one in-flight /account login (account.go).
	login loginState
	// secret is the at-most-one /secret add capture (secrets.go).
	secret secretPending
	// pty overrides how PTY flows start a process; nil means the real one.
	// Only the tests set it.
	pty ptyStarter
	// sched is the watch/schedule/doctor loop (nil in tests that do not need it).
	sched *scheduler
	// panel is the live session card in the owner's DM (nil in tests that
	// do not drive Telegram).
	panel *sessionPanel
	// edits remembers the edited messages already dispatched as commands.
	edits editLog

	mu  sync.Mutex
	cfg *Config
}

// editLog is the dedupe for edited commands: an edit is identified by
// (chat, message, edit_date), so editing the same message again runs the new
// text once while Telegram re-delivering the same edit does nothing.
type editLog struct {
	mu   sync.Mutex
	seen map[string]bool
	fifo []string
}

// editLogMax bounds the log: a few hundred edits is far more than a redelivery
// window ever spans, and it keeps a long-running instance from growing.
const editLogMax = 256

// first reports whether this exact edit has not been dispatched yet, and
// records it.
func (e *editLog) first(chatID int64, messageID int, editDate int64) bool {
	key := fmt.Sprintf("%d:%d:%d", chatID, messageID, editDate)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.seen[key] {
		return false
	}
	if e.seen == nil {
		e.seen = map[string]bool{}
	}
	e.seen[key] = true
	e.fifo = append(e.fifo, key)
	if len(e.fifo) > editLogMax {
		delete(e.seen, e.fifo[0])
		e.fifo = e.fifo[1:]
	}
	return true
}

func (in *instance) config() *Config {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.cfg
}

// telegramUI is the runner's view of Telegram. General (topic 0) lands in
// the owner's DM. Any other topic id is a backend worker: no Telegram send.
type telegramUI struct{ in *instance }

func (t telegramUI) Post(topicID int64, html string) (int64, error) {
	cfg := t.in.config()
	chat, thread, ok := destForTopic(cfg, topicID)
	if !ok {
		return 0, nil
	}
	return sendMessageHTMLGetID(cfg, chat, thread, html)
}

func (t telegramUI) PostSilent(topicID int64, html string) (int64, error) {
	cfg := t.in.config()
	chat, thread, ok := destForTopic(cfg, topicID)
	if !ok {
		return 0, nil
	}
	return sendMessageHTMLGetIDSilent(cfg, chat, thread, html)
}

func (t telegramUI) Edit(topicID, msgID int64, html string) error {
	cfg := t.in.config()
	chat, thread, ok := destForTopic(cfg, topicID)
	if !ok || msgID == 0 {
		return nil
	}
	return editMessageHTML(cfg, chat, msgID, thread, html)
}

func (t telegramUI) React(messageID int64, emoji string) {
	cfg := t.in.config()
	if cfg.BotToken == "" || messageID == 0 || cfg.ChatID == 0 {
		return
	}
	if err := setMessageReaction(cfg, cfg.ChatID, messageID, emoji); err != nil {
		hookLog("reaction failed: %v", err)
	}
}

func (t telegramUI) Pin(msgID int64) error {
	if t.in == nil || msgID == 0 {
		return nil
	}
	cfg := t.in.config()
	if cfg == nil || cfg.BotToken == "" || cfg.ChatID == 0 {
		return nil
	}
	return pinChatMessage(cfg, cfg.ChatID, msgID, true)
}

func (t telegramUI) Unpin(msgID int64) error {
	if t.in == nil || msgID == 0 {
		return nil
	}
	cfg := t.in.config()
	if cfg == nil || cfg.BotToken == "" || cfg.ChatID == 0 {
		return nil
	}
	return unpinChatMessage(cfg, cfg.ChatID, msgID)
}

// ---------------------------------------------------------------------------
// Bootstrap
// ---------------------------------------------------------------------------

// listenV3 is `ccc listen`: open the database, start the runner, long-poll
// Telegram. Bootstrap config (token, chat_id, profiles) stays in config.json;
// everything runtime lives in SQLite (DESIGN §5).
func listenV3() error {
	time.Sleep(time.Duration(os.Getpid()%500) * time.Millisecond)

	lockPath := filepath.Join(cacheDir(), "ccc.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("failed to open lock file: %w", err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Println("Another ccc listen instance is already running, exiting quietly")
		return nil
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) // safe-ignore: the lock also dies with the process

	lockFile.Truncate(0) // safe-ignore: the pid line below is a diagnostic, not state ccc reads back
	lockFile.Seek(0, 0)  // safe-ignore: same
	fmt.Fprintf(lockFile, "%d\n", os.Getpid())

	initListenLog()
	if listenLogFile != nil {
		defer listenLogFile.Close()
	}

	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("not configured. Run: ccc setup <bot_token>")
	}
	if cfg.BotToken == "" {
		return fmt.Errorf("no bot token. Run: ccc setup <bot_token>")
	}

	// A service started by systemd --user or launchd has none of the owner's
	// shell environment, so the env_passthrough secrets come from the file
	// `ccc env sync` wrote. A value already in the environment always wins.
	if loaded := loadEnvFile(cfg); len(loaded) > 0 {
		listenLog("env file: loaded %s", strings.Join(loaded, " ")) // names only, never values
	}
	if _, missing := envPassthroughStatus(cfg); len(missing) > 0 {
		listenLog("env file: still missing %s (run `bash -lc 'ccc env sync'`)", strings.Join(missing, " "))
	}

	db, err := openStore(dbPath(cfg))
	if err != nil {
		return err
	}
	in := &instance{db: db, cfg: cfg, dataDir: dataDir(cfg)}
	tg := telegramUI{in}
	runner := newRunner(db, cfg, tg)
	in.runner = runner
	in.panel = runner.panel
	defer runner.Close()

	sched := newScheduler(in)
	in.sched = sched
	defer sched.Close()

	linkSharedProjects(cfg)
	setBotCommandsV3(cfg.BotToken)
	listenLog("ccc v3 listening (dm: %d, db: %s)", cfg.ChatID, dbPath(cfg))

	// Conversational turns that were mid-flight died with us: they are
	// requeued (same row, so they stay oldest) and retried. Background
	// jobs do not die: they are detached, and recoverAfterRestart
	// reattaches them. Both get a Telegram ping so a LaunchAgent restart
	// is not silent.
	in.recoverAfterRestart()
	if _, err := in.ensureGeneralBot(); err != nil {
		listenLog("ensure General: %v", err)
	}
	go sched.Run()
	// Re-arm queues that survived the restart, including bot-to-bot messages
	// whose sender's turn ended as the process was going down.
	runner.deliverInbox(0)
	var bots []Bot
	db.Where("archived_at IS NULL").Find(&bots)
	for i := range bots {
		runner.kick(bots[i].ID)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		listenLog("Shutting down (signal: %v)", sig)
		runner.Close()
		sched.Close()
		os.Exit(0)
	}()

	offset := 0
	client := &http.Client{Timeout: 35 * time.Second}
	for {
		// edited_message is requested too: an edit is an inbound update from a
		// user, so the access gate has to see it rather than have it silently
		// skipped by Telegram's default allowed_updates.
		reqURL := fmt.Sprintf("%s?offset=%d&timeout=30&allowed_updates=%s",
			telegramURL(cfg.BotToken, "getUpdates"), offset, `["message","edited_message","callback_query"]`)
		resp, err := telegramClientGet(client, cfg.BotToken, reqURL)
		if err != nil {
			listenLog("Network error: %v (retrying...)", err)
			time.Sleep(5 * time.Second)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize)) // safe-ignore: a short read is handled as a parse error below
		resp.Body.Close()                                                 // safe-ignore: nothing to do if closing a read body fails

		var updates TelegramUpdate
		if err := json.Unmarshal(body, &updates); err != nil {
			listenLog("Parse error: %v", err)
			time.Sleep(time.Second)
			continue
		}
		if !updates.OK {
			listenLog("Telegram API error: %s", updates.Description)
			time.Sleep(5 * time.Second)
			continue
		}
		for _, u := range updates.Result {
			offset = u.UpdateID + 1
			switch {
			case u.CallbackQuery != nil:
				in.handleCallback(u.CallbackQuery)
			case u.EditedMessage != nil:
				// The gate applies to an edit like to anything else — a
				// stranger's edit must not slip past — hence the explicit call
				// before handleEditedMessage decides what to do with it.
				if in.gate(u.EditedMessage.From.ID) == roleDenied {
					continue
				}
				in.handleEditedMessage(u.EditedMessage)
			default:
				msg := u.Message
				in.handleMessage(&msg)
			}
		}
	}
}

// recoverAfterRestart is what a new listen does with leftover in-flight work.
// Conversational turns cannot resume in-process (the engine CLI died), so
// each status=running row is requeued in place — same id, so it stays older
// than anything that arrived while it was running — and retried. Historical
// failures are left alone. Background jobs may still be running and are
// reattached. A single notifying message in General says ccc is back.
func (in *instance) recoverAfterRestart() {
	var interrupted []Turn
	if err := in.db.Where("status = ?", turnRunning).Find(&interrupted).Error; err != nil {
		hookLog("recover: list running turns: %v", err)
	}

	in.notifyGeneral("🔁 ccc is back")
	now := time.Now()
	for i := range interrupted {
		t := interrupted[i]
		b, err := botByID(in.db, t.BotID)
		if err != nil || b.ArchivedAt != nil {
			in.failInterruptedTurn(t.ID, now)
			continue
		}
		if err := requeueInterruptedTurn(in.db, t.ID); err != nil {
			hookLog("recover: requeue turn %d: %v", t.ID, err)
			in.failInterruptedTurn(t.ID, now)
			continue
		}
		in.notifyBot(b, renderRetriedTurn(&t))
	}
	in.db.Model(&Bot{}).Where("status = ?", botRunning).Update("status", botIdle)
	if in.sched != nil {
		in.sched.reattachBackgroundJobs()
	}
	in.syncPanel(true)
}

func (in *instance) failInterruptedTurn(id int64, now time.Time) {
	in.db.Model(&Turn{}).Where("id = ? AND status = ?", id, turnRunning).
		Updates(map[string]any{
			"status": turnFailed, "error_class": errFatal,
			"stop_reason": "ccc restarted", "ended_at": now,
		})
}

func requeueInterruptedTurn(db *gorm.DB, id int64) error {
	res := db.Model(&Turn{}).Where("id = ? AND status = ?", id, turnRunning).
		Updates(map[string]any{
			"status":      turnQueued,
			"started_at":  gorm.Expr("NULL"),
			"ended_at":    gorm.Expr("NULL"),
			"error_class": "",
			"stop_reason": "",
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("turn %d is no longer running", id)
	}
	return nil
}

func renderRetriedTurn(t *Turn) string {
	// One-liner only: do not dump the turn input (often a General→session prompt).
	_ = t
	return "▶️ Retrying turn interrupted by restart."
}

// linkSharedProjects makes every profile resolve transcripts from the same
// place, which is what lets a retried turn resume the SAME session UUID on a
// different account (DESIGN §4). It only ever CREATES symlinks; an existing
// projects/ directory is left exactly as it is.
func linkSharedProjects(cfg *Config) {
	shared := filepath.Join(dataDir(cfg), "projects")
	profiles := listProfiles(cfg)
	for _, p := range profiles {
		if profileEngine(p) != engineClaude {
			continue
		}
		dir := profileProjectsDir(p)
		if _, err := os.Lstat(shared); err != nil {
			// The implicit ~/.claude profile already owns a real projects/ with
			// history; point the shared path at it rather than the reverse.
			if p.Implicit {
				if _, err := os.Stat(dir); err == nil {
					if err := os.MkdirAll(filepath.Dir(shared), 0o700); err == nil {
						if err := os.Symlink(dir, shared); err != nil {
							hookLog("shared projects link: %v", err)
						}
					}
					continue
				}
			}
			if err := os.MkdirAll(shared, 0o700); err != nil {
				hookLog("shared projects dir: %v", err)
				return
			}
		}
		if _, err := os.Lstat(dir); err == nil {
			continue // never touch an existing projects/
		}
		if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
			continue
		}
		if err := os.Symlink(shared, dir); err != nil {
			hookLog("link %s -> %s: %v", dir, shared, err)
		}
	}
}

func setBotCommandsV3(botToken string) {
	commands := []map[string]string{
		{"command": "name", "description": "Show or set this session's name: /name <name>"},
		{"command": "new", "description": "Start a fresh conversation (memory kept)"},
		{"command": "stop", "description": "Stop the running turn and drop the queue"},
		{"command": "cwd", "description": "Show or set this session's working directory"},
		{"command": "engine", "description": "Show this session's engine pool (set at /account add); assign another pool"},
		{"command": "memory", "description": "Memories: /memory [query] | stats | restore <id>"},
		{"command": "forget", "description": "Forget a memory: /forget <scope> <key>"},
		{"command": "watches", "description": "List this session's watches"},
		{"command": "schedules", "description": "List this session's schedules"},
		{"command": "sessions", "description": "List open sessions"},
		{"command": "status", "description": "Instance health: profiles, queue, running turns"},
		{"command": "usage", "description": "Tokens, cache hit ratio and cost per session"},
		{"command": "account", "description": "Accounts by engine (owner only)"},
		{"command": "access", "description": "Who may talk to ccc (owner only)"},
		{"command": "model", "description": "Each account's model, or set one (owner only)"},
		{"command": "cancel", "description": "Cancel an in-progress account login"},
		{"command": "secret", "description": "Owner vault: /secret add <name> | list | delete <name>"},
	}
	payload := map[string]any{"commands": commands}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Post(telegramURL(botToken, "setMyCommands"), "application/json", strings.NewReader(string(body)))
	if err == nil {
		resp.Body.Close() // safe-ignore: the command list is cosmetic; a failed close changes nothing
	}
}

// ---------------------------------------------------------------------------
// Update handling
// ---------------------------------------------------------------------------

// handleMessage routes one inbound Telegram message (DESIGN §8 "Conversation").
// Nothing happens before the access gate: an update from a user who is neither
// the owner nor on allowed_user_ids is dropped here, group or DM (DESIGN §12).
func (in *instance) handleMessage(msg *TelegramMessage) {
	isDM := msg.Chat.Type == "private"
	role := in.gate(msg.From.ID)
	if role == roleDenied {
		return
	}
	if !isDM {
		return
	}

	// Attachments are saved into the bot's workspace before anything else, so
	// the text path below sees a normal message carrying a path.
	if handled := in.handleAttachment(msg); handled {
		return
	}

	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return
	}

	// A pending /account login owns the owner's next message in its chat: it
	// is the OAuth code, not something to hand to a bot.
	if role == roleOwner && in.takeLoginCode(msg.Chat.ID, msg.MessageThreadID, text) {
		return
	}
	// /secret add captures the next owner message the same way: the value
	// never becomes a turn, never hits the inbox, never reaches the model.
	if role == roleOwner && in.takeSecretValue(msg) {
		return
	}

	if strings.HasPrefix(text, "/") {
		in.handleCommand(msg, text, role)
		return
	}

	// Reply-to a pending ask_owner question: the owner is answering that
	// session, not chatting with General. Free text never answers.
	if b, ok := in.botForQuestionReply(msg); ok {
		in.deliver(b, msg, text)
		return
	}

	b, err := in.ensureGeneralBot()
	if err != nil {
		in.reply(msg, "Could not start General: "+err.Error())
		return
	}
	in.deliver(b, msg, text)
}

func questionAnsweredHTML(q *Question, answer string) string {
	if q == nil {
		return "✓ <b>" + htmlEscape(answer) + "</b>"
	}
	return "❓ " + htmlEscape(q.Question) + "\n✓ <b>" + htmlEscape(answer) + "</b>"
}

func (in *instance) tickAskedMessage(q *Question, answer string) {
	if q == nil || q.AskedMessageID == 0 {
		return
	}
	cfg := in.config()
	chat, thread, ok := destForTopic(cfg, 0)
	if !ok {
		return
	}
	empty := emptyInlineKeyboard()
	_ = editMessageHTMLMarkup(cfg, chat, q.AskedMessageID, thread, questionAnsweredHTML(q, answer), &empty) // safe-ignore: ticking the question message is cosmetic
}

// botForQuestionReply matches a reply-to against any pending ask_owner
// question, so the owner can answer a worker from the DM.
func (in *instance) botForQuestionReply(msg *TelegramMessage) (*Bot, bool) {
	if msg == nil || msg.ReplyToMessage == nil {
		return nil, false
	}
	var q Question
	if err := in.db.Where("asked_message_id = ? AND answered_at IS NULL",
		msg.ReplyToMessage.MessageID).First(&q).Error; err != nil {
		return nil, false
	}
	b, err := botByID(in.db, q.BotID)
	if err != nil || b.ArchivedAt != nil {
		return nil, false
	}
	return b, true
}

// handleEditedMessage decides what an edit does. Plain text still does nothing:
// re-running a turn because somebody fixed a typo is worse than ignoring it.
// A COMMAND is dispatched, because correcting a mistyped command in place is
// how a phone fixes one — an owner who edits `/account add` into
// `/account add me@example.com` means it to run — and because the edit is the
// only version of the message that is left.
func (in *instance) handleEditedMessage(msg *TelegramMessage) {
	if !strings.HasPrefix(strings.TrimSpace(msg.Text), "/") {
		return
	}
	// Telegram re-delivers an edit as a new update, so the command runs once
	// per actual edit and not once per delivery. The gate runs again inside
	// handleMessage, which is free for a sender that already passed it.
	if !in.edits.first(msg.Chat.ID, msg.MessageID, msg.EditDate) {
		return
	}
	in.handleMessage(msg)
}

// deliver turns a plain message into an input for a bot: either the answer to a
// pending question (reply-to only) or a new turn. Free text never answers.
func (in *instance) deliver(b *Bot, msg *TelegramMessage, text string) {
	if q, ok := in.matchQuestion(b, msg); ok {
		if err := in.answerOwnerQuestion(q, text); err != nil {
			in.reply(msg, "Could not queue that: "+err.Error())
		}
		return
	}
	if _, err := in.runner.Enqueue(b.ID, sourceUser, text, int64(msg.MessageID)); err != nil {
		in.reply(msg, "Could not queue that: "+err.Error())
	}
}

// matchQuestion is true only for an explicit reply to that question's message.
// Waiting status is not enough: free text in the DM is always General.
func (in *instance) matchQuestion(b *Bot, msg *TelegramMessage) (*Question, bool) {
	if msg == nil || msg.ReplyToMessage == nil {
		return nil, false
	}
	var q Question
	err := in.db.Where("bot_id = ? AND asked_message_id = ? AND answered_at IS NULL",
		b.ID, msg.ReplyToMessage.MessageID).First(&q).Error
	if err != nil {
		return nil, false
	}
	return &q, true
}

// answerOwnerQuestion records the answer, fans it out to similar pending asks,
// and ticks every original question message.
func (in *instance) answerOwnerQuestion(q *Question, answer string) error {
	answered, err := applyQuestionAnswer(in.db, in.runner, q, answer)
	if err != nil {
		return err
	}
	for i := range answered {
		in.tickAskedMessage(&answered[i], answered[i].Answer)
	}
	return nil
}

// handleCallback processes an inline button tap. Callback data is ccc's own
// (`account:`, `model:`), but the TAP is an inbound update like any other, so
// it goes through the same gate — and the owner-only namespaces are checked
// again here. Leftover ask_owner `q:` taps are acked and ignored.
func (in *instance) handleCallback(cb *CallbackQuery) {
	cfg := in.config()
	role := in.gate(cb.From.ID)
	if role == roleDenied {
		return
	}
	parts := strings.Split(cb.Data, ":")
	if len(parts) == 0 {
		answerCallbackQuery(cfg, cb.ID, "")
		return
	}
	switch parts[0] {
	case "account":
		answerCallbackQuery(cfg, cb.ID, "")
		if role == roleOwner {
			in.handleAccountCallback(cb, parts)
		}
		return
	case "model":
		answerCallbackQuery(cfg, cb.ID, "")
		if role == roleOwner {
			in.handleModelCallback(cb, parts)
		}
		return
	default:
		answerCallbackQuery(cfg, cb.ID, "")
	}
}

// ---------------------------------------------------------------------------
// Bot creation
// ---------------------------------------------------------------------------

// createBotFromText starts a backend worker named after the first line, with
// that same prompt as its first turn. Used by /session (escape hatch around
// General). spawn_session does the same without a Telegram topic.
func (in *instance) createBotFromText(msg *TelegramMessage, text string) {
	b, err := in.createBot(botNameFromText(text), "")
	if err != nil {
		in.reply(msg, "Could not start the session: "+err.Error())
		return
	}
	in.reply(msg, fmt.Sprintf("🧵 Started session <b>%s</b> — it runs in the backend; reports come here.", htmlEscape(b.Name)))
	if _, err := in.runner.Enqueue(b.ID, sourceUser, text, 0); err != nil {
		hookLog("enqueue first message: %v", err)
	}
	in.syncPanel(true)
}

// createBot creates the workspace and the database row. No Telegram topic.
func (in *instance) createBot(name, role string) (*Bot, error) {
	return createBotRow(in.db, in.config(), name, role, "")
}

// botNameFromText derives a session name from the first line of a message.
func botNameFromText(text string) string {
	line := text
	if i := strings.IndexAny(line, "\n\r"); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	if len(line) > 40 {
		if cut := strings.LastIndex(line[:40], " "); cut > 10 {
			line = line[:cut]
		} else {
			line = line[:40]
		}
	}
	return line
}

// sanitizeBotName keeps names usable as directory names.
func sanitizeBotName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\' || r == 0:
			return '-'
		case r < 32:
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" {
		name = fmt.Sprintf("session-%d", time.Now().Unix())
	}
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

// ---------------------------------------------------------------------------
// Attachments
// ---------------------------------------------------------------------------

// handleAttachment saves photos/documents into the bot's workspace inbox/ and
// enqueues a turn carrying the path (DESIGN §8). Voice notes are transcribed
// when the voice build is present.
func (in *instance) handleAttachment(msg *TelegramMessage) bool {
	if msg.Voice == nil && msg.Document == nil && len(msg.Photo) == 0 {
		return false
	}
	if msg.Chat.Type != "private" {
		return false
	}
	cfg := in.config()
	b, err := in.ensureGeneralBot()
	if err != nil {
		return false
	}
	inbox := filepath.Join(botCwd(cfg, b), "inbox")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		in.reply(msg, "Could not create inbox/: "+err.Error())
		return true
	}

	switch {
	case msg.Voice != nil:
		path := filepath.Join(inbox, fmt.Sprintf("voice_%d.ogg", time.Now().UnixNano()))
		if err := downloadTelegramFile(cfg, msg.Voice.FileID, path); err != nil {
			in.reply(msg, "Download failed: "+err.Error())
			return true
		}
		if voiceSupported {
			transcription, err := transcribeAudio(cfg, path)
			if err == nil && strings.TrimSpace(transcription) != "" {
				in.reply(msg, "📝 "+htmlEscape(transcription))
				in.deliver(b, msg, "[voice transcription, may contain errors] "+transcription)
				return true
			}
		}
		in.deliver(b, msg, fmt.Sprintf("The owner sent a voice note, saved at %s (transcribe it yourself if you need the text).", path))
		return true

	case len(msg.Photo) > 0:
		photo := msg.Photo[len(msg.Photo)-1]
		path := filepath.Join(inbox, fmt.Sprintf("photo_%d.jpg", time.Now().UnixNano()))
		if err := downloadTelegramFile(cfg, photo.FileID, path); err != nil {
			in.reply(msg, "Download failed: "+err.Error())
			return true
		}
		caption := strings.TrimSpace(msg.Caption)
		if caption == "" {
			caption = "The owner sent an image."
		}
		in.deliver(b, msg, fmt.Sprintf("%s It is saved at %s", caption, path))
		return true

	default:
		name := sanitizeFileName(msg.Document.FileName)
		path := filepath.Join(inbox, name)
		if err := downloadTelegramFile(cfg, msg.Document.FileID, path); err != nil {
			in.reply(msg, "Download failed: "+err.Error())
			return true
		}
		caption := strings.TrimSpace(msg.Caption)
		if caption == "" {
			caption = "The owner sent a file."
		}
		in.deliver(b, msg, fmt.Sprintf("%s It is saved at %s", caption, path))
		return true
	}
}

// sanitizeFileName strips path separators from a Telegram-provided file name:
// the name comes from the world, and it is used to build a path.
func sanitizeFileName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || name == ".." || name == string(filepath.Separator) {
		return fmt.Sprintf("file_%d", time.Now().UnixNano())
	}
	return name
}

func botCwd(cfg *Config, b *Bot) string {
	if strings.TrimSpace(b.Cwd) != "" {
		return b.Cwd
	}
	return botWorkspace(cfg, b.Name)
}

// ---------------------------------------------------------------------------
// Commands (DESIGN §8)
// ---------------------------------------------------------------------------

// ownerOnlyCommands are the ones that change the instance itself, rather than
// talking to a bot: accounts, access, model and the vault.
var ownerOnlyCommands = map[string]bool{
	"/account": true, "/access": true, "/model": true, "/secret": true,
}

func (in *instance) handleCommand(msg *TelegramMessage, text string, role accessRole) {
	cmd, rest := splitCommand(text)
	if ownerOnlyCommands[cmd] && role != roleOwner {
		in.reply(msg, "That command is owner-only.")
		return
	}
	switch cmd {
	case "/access":
		in.handleAccessCommand(msg, rest)
		return
	case "/account":
		in.handleAccountCommand(msg, rest)
		return
	case "/model":
		in.handleModelCommand(msg, rest, nil)
		return
	case "/secret":
		in.handleSecretCommand(msg, rest)
		return
	case "/cancel":
		in.reply(msg, "Nothing to cancel.")
		return
	case "/sessions", "/bots":
		in.reply(msg, in.renderBots())
		return
	case "/status":
		in.reply(msg, in.renderStatus())
		return
	case "/usage":
		in.reply(msg, renderUsage(in.db, time.Now()))
		return
	case "/session", "/bot":
		if strings.TrimSpace(rest) == "" {
			in.reply(msg, "Usage: /session &lt;prompt&gt; — starts a backend session without going through General.")
			return
		}
		in.createBotFromText(msg, rest)
		return
	}

	b, err := in.ensureGeneralBot()
	if err != nil {
		in.reply(msg, "Could not start General: "+err.Error())
		return
	}

	switch cmd {
	case "/name":
		if isGeneralBot(b) {
			in.reply(msg, "General stays General.")
			return
		}
		in.handleNameCommand(msg, b, rest)

	case "/new":
		in.db.Model(&Bot{}).Where("id = ?", b.ID).Update("session_id", "")
		in.reply(msg, "🆕 Fresh conversation. Memories are kept.")

	case "/stop":
		in.handleStopCommand(msg, rest)
		return

	case "/engine":
		in.handleEngineCommand(msg, b, rest)

	case "/cwd":
		if strings.TrimSpace(rest) == "" {
			in.reply(msg, "<code>"+htmlEscape(botCwd(in.config(), b))+"</code>")
			return
		}
		path := expandPath(strings.TrimSpace(rest))
		if !filepath.IsAbs(path) {
			in.reply(msg, "Give an absolute path.")
			return
		}
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			in.reply(msg, "That directory does not exist.")
			return
		}
		in.db.Model(&Bot{}).Where("id = ?", b.ID).Update("cwd", path)
		in.reply(msg, "📂 Working dir set to <code>"+htmlEscape(path)+"</code>")

	case "/memory":
		// Two subcommands live under /memory because they are about the memory
		// store itself rather than about one bot's notes: what it holds, and
		// how to undo a compaction that went wrong (DESIGN §7).
		sub, arg := splitFirstWord(strings.TrimSpace(rest))
		switch sub {
		case "stats":
			in.reply(msg, renderMemoryStats(in.db))
			return
		case "restore":
			if role != roleOwner {
				in.reply(msg, "Restoring a compaction is owner-only.")
				return
			}
			in.handleMemoryRestore(msg, arg)
			return
		}
		mems, err := searchMemories(in.db, b.ID, strings.TrimSpace(rest), "", 20)
		if err != nil {
			in.reply(msg, "Search failed: "+err.Error())
			return
		}
		if len(mems) == 0 {
			in.reply(msg, "No memories.")
			return
		}
		var sb strings.Builder
		for _, m := range mems {
			where := m.Scope
			if m.Scope == scopeProject {
				where = "project " + filepath.Base(m.ScopeKey)
			}
			fmt.Fprintf(&sb, "• <b>%s</b> <i>[%s]</i>\n%s\n", htmlEscape(m.Key), htmlEscape(where), htmlEscape(truncate(m.Text, 300)))
		}
		in.reply(msg, sb.String())

	case "/watches":
		in.handleWatchesCommand(msg, b, rest)

	case "/schedules":
		in.handleSchedulesCommand(msg, b, rest)

	case "/forget":
		scope, key := splitFirstWord(rest)
		if scope == "" || key == "" {
			in.reply(msg, "Usage: /forget &lt;user|project|bot&gt; &lt;key&gt;")
			return
		}
		scopeKey := ""
		switch scope {
		case scopeUser:
		case scopeBot:
			scopeKey = fmt.Sprint(b.ID)
		case scopeProject:
			scopeKey = botCwd(in.config(), b)
		default:
			in.reply(msg, "Scope must be user, project or bot.")
			return
		}
		res := in.db.Where("scope = ? AND scope_key = ? AND key = ?", scope, scopeKey, key).Delete(&Memory{})
		if res.RowsAffected == 0 {
			in.reply(msg, "No such memory.")
			return
		}
		in.reply(msg, "🗑 Forgot <b>"+htmlEscape(key)+"</b>")

	default:
		in.reply(msg, "Unknown command.")
	}
}

// handleStopCommand implements `/stop [name|all]`. Bare `/stop` kills
// General's turn if one is running (or drops its queue). If that did
// nothing, it stops every live worker — so a hung session is reachable
// without naming it. `/stop <name>` is case-insensitive; `orchestrator`
// is an alias for General. Waiting (ask_owner) sessions stay parked.
func (in *instance) handleStopCommand(msg *TelegramMessage, rest string) {
	arg := strings.TrimSpace(rest)
	bots, err := liveBots(in.db)
	if err != nil {
		in.reply(msg, "Could not list sessions.")
		return
	}
	var targets []int64
	switch {
	case strings.EqualFold(arg, "all"):
		for i := range bots {
			targets = append(targets, bots[i].ID)
		}
	case arg == "":
		if g, err := generalBot(in.db); err == nil {
			killed, dropped := in.stopSessions([]int64{g.ID})
			if len(killed) > 0 || dropped > 0 {
				in.reply(msg, renderStopReply(killed, dropped, stillRunningAfterStop(in.db, killed)))
				return
			}
		}
		for i := range bots {
			if !isGeneralBot(&bots[i]) {
				targets = append(targets, bots[i].ID)
			}
		}
	default:
		b := findLiveSession(bots, arg)
		if b == nil {
			in.reply(msg, "No live session named "+htmlEscape(arg)+".")
			return
		}
		targets = []int64{b.ID}
	}
	if len(targets) == 0 {
		in.reply(msg, "Nothing was running.")
		return
	}
	killed, dropped := in.stopSessions(targets)
	in.reply(msg, renderStopReply(killed, dropped, stillRunningAfterStop(in.db, killed)))
}

func findLiveSession(bots []Bot, name string) *Bot {
	want := strings.ToLower(strings.TrimSpace(name))
	if want == "orchestrator" {
		want = strings.ToLower(generalBotName)
	}
	for i := range bots {
		if strings.ToLower(bots[i].Name) == want {
			return &bots[i]
		}
	}
	return nil
}

func (in *instance) stopSessions(ids []int64) (killed []string, dropped int) {
	for _, id := range ids {
		b, err := botByID(in.db, id)
		if err != nil || b == nil {
			continue
		}
		var queued int64
		in.db.Model(&Turn{}).Where("bot_id = ? AND status = ?", id, turnQueued).Count(&queued)
		dropped += int(queued)
		if in.runner == nil {
			continue
		}
		if in.runner.Stop(id) {
			killed = append(killed, b.Name)
			if b.Status == botRunning {
				setBotStatus(in.db, id, botIdle)
			}
		}
	}
	return killed, dropped
}

func stillRunningAfterStop(db *gorm.DB, killed []string) []string {
	gone := map[string]bool{}
	for _, n := range killed {
		gone[n] = true
	}
	bots, err := liveBots(db)
	if err != nil {
		return nil
	}
	var still []string
	for i := range bots {
		b := &bots[i]
		if gone[b.Name] || b.Status != botRunning {
			continue
		}
		still = append(still, b.Name)
	}
	return still
}

func renderStopReply(killed []string, dropped int, still []string) string {
	if len(killed) == 0 && dropped == 0 {
		return "Nothing was running."
	}
	var sb strings.Builder
	if len(killed) > 0 {
		fmt.Fprintf(&sb, "🛑 Stopped %s.", strings.Join(killed, ", "))
	}
	if dropped > 0 {
		if sb.Len() > 0 {
			sb.WriteByte(' ')
		}
		if dropped == 1 {
			sb.WriteString("Dropped 1 queued.")
		} else {
			fmt.Fprintf(&sb, "Dropped %d queued.", dropped)
		}
	}
	if len(still) > 0 {
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		fmt.Fprintf(&sb, "Still running: %s. /stop %s or /stop all", strings.Join(still, ", "), still[0])
	}
	return sb.String()
}

// handleMemoryRestore implements `/memory restore <compaction_id>`: put back
// the memories one compaction replaced. The archive rows are consumed, so the
// compaction id stops existing once it has been undone.
func (in *instance) handleMemoryRestore(msg *TelegramMessage, arg string) {
	id, err := strconv.ParseInt(strings.TrimSpace(arg), 10, 64)
	if err != nil || id <= 0 {
		in.reply(msg, "Usage: /memory restore &lt;compaction_id&gt; (the id is in the compaction message and in /memory stats)")
		return
	}
	label, n, err := restoreCompaction(in.db, id)
	if err != nil {
		in.reply(msg, "Could not restore: "+htmlEscape(err.Error()))
		return
	}
	in.reply(msg, fmt.Sprintf("↩️ Restored %d entries into <b>%s</b>; compaction %d is undone.",
		n, htmlEscape(label), id))
}

// handleNameCommand implements `/name [<name>]` (DESIGN §8): show or change
// the session name. The name is part of the system prompt, so setting it
// rotates the conversation (DESIGN §14.14). Workers are renamed via set_name
// ; in the DM this only ever hits General, which refuses.
func (in *instance) handleNameCommand(msg *TelegramMessage, b *Bot, rest string) {
	raw := strings.TrimSpace(rest)
	if raw == "" {
		in.reply(msg, "<b>Name</b>\n"+htmlEscape(b.Name))
		return
	}
	name, err := validateBotName(in.db, b.ID, raw)
	if err != nil {
		in.reply(msg, "Cannot rename: "+htmlEscape(err.Error()))
		return
	}
	old := b.Name
	if name != old {
		if err := renameBot(in.db, in.config(), b, name); err != nil {
			in.reply(msg, "Cannot rename: "+htmlEscape(err.Error()))
			return
		}
	}
	var reply string
	if name == old {
		reply = "✏️ Still <b>" + htmlEscape(old) + "</b>."
	} else {
		reply = fmt.Sprintf("✏️ <b>%s</b> is now <b>%s</b>; the next message starts a fresh conversation with the new name.",
			htmlEscape(old), htmlEscape(name))
	}
	in.reply(msg, reply)
}

func splitCommand(text string) (string, string) {
	cmd := text
	rest := ""
	if i := strings.IndexAny(text, " \n"); i >= 0 {
		cmd, rest = text[:i], strings.TrimSpace(text[i+1:])
	}
	// Telegram appends @botname to commands in groups.
	if at := strings.IndexByte(cmd, '@'); at > 0 {
		cmd = cmd[:at]
	}
	return strings.ToLower(cmd), rest
}

func splitFirstWord(s string) (string, string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	if i := strings.IndexAny(s, " \n"); i >= 0 {
		return s[:i], strings.TrimSpace(s[i+1:])
	}
	return s, ""
}

func (in *instance) renderBots() string {
	bots, err := liveBots(in.db)
	if err != nil || len(bots) == 0 {
		return "No sessions yet. Talk to me here, or /session."
	}
	var sb strings.Builder
	sb.WriteString("<b>Sessions</b>\n")
	for i := range bots {
		b := &bots[i]
		var last Turn
		when := "never"
		if err := in.db.Where("bot_id = ?", b.ID).Order("id DESC").First(&last).Error; err == nil {
			when = humanDuration(time.Since(last.CreatedAt)) + " ago"
		}
		engine := botEngine(b)
		label := b.Name
		if isGeneralBot(b) {
			label = b.Name + " (dispatcher)"
		}
		if engine == engineClaude {
			fmt.Fprintf(&sb, "• <b>%s</b> [%s] · last %s\n",
				htmlEscape(label), b.Status, when)
		} else {
			fmt.Fprintf(&sb, "• <b>%s</b> [%s] %s · last %s\n",
				htmlEscape(label), b.Status, htmlEscape(engine), when)
		}
	}
	return sb.String()
}

func (in *instance) renderStatus() string {
	cfg := in.config()
	var sb strings.Builder
	sb.WriteString("<b>ccc status</b>\n")
	var queued, running int64
	in.db.Model(&Turn{}).Where("status = ?", turnQueued).Count(&queued)
	in.db.Model(&Turn{}).Where("status = ?", turnRunning).Count(&running)
	var nBots int64
	in.db.Model(&Bot{}).Where("archived_at IS NULL").Count(&nBots)
	fmt.Fprintf(&sb, "sessions: %d · running: %d · queued: %d\n", nBots, running, queued)
	fmt.Fprintf(&sb, "models: %s\n", htmlEscape(renderInstanceModels(cfg)))
	if e := defaultEngine(cfg); e != engineClaude {
		fmt.Fprintf(&sb, "default engine: %s\n", e)
	}
	fmt.Fprintf(&sb, "data: <code>%s</code>\n", htmlEscape(dataDir(cfg)))
	var watches, schedules int64
	in.db.Model(&Watch{}).Where("enabled = ?", true).Count(&watches)
	in.db.Model(&Schedule{}).Where("fired_at IS NULL").Count(&schedules)
	var bgRun, bgQ int64
	in.db.Model(&BackgroundJob{}).Where("status = ?", jobRunning).Count(&bgRun)
	in.db.Model(&BackgroundJob{}).Where("status = ?", jobQueued).Count(&bgQ)
	fmt.Fprintf(&sb, "watches: %d · schedules: %d · background: %d running / %d queued\n",
		watches, schedules, bgRun, bgQ)

	// Passthrough secrets, by NAME only (DESIGN §12: ccc never posts a value).
	// "missing" here almost always means `ccc env sync` was not run from a
	// login shell, and is the difference between a bot that can push to GitHub
	// and one that cannot.
	if present, missing := envPassthroughStatus(cfg); len(present)+len(missing) > 0 {
		fmt.Fprintf(&sb, "env: %s", htmlEscape(namesOrNone(present)))
		if len(missing) > 0 {
			fmt.Fprintf(&sb, " · <b>missing</b>: %s (run <code>bash -lc 'ccc env sync'</code>)",
				htmlEscape(strings.Join(missing, " ")))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("\n<b>Accounts</b>\n")
	now := time.Now()
	needsLogin := in.needsLoginSet()
	for _, s := range collectProfileStats(cfg, runningTurnsByProfileDB(in.db), now) {
		state := "ok"
		switch {
		case needsLogin[s.Name]:
			state = "needs login"
		case !s.CooledUntil.IsZero() && s.CooledUntil.After(now):
			state = "cooldown until " + s.CooledUntil.Format("15:04")
		}
		// Accounts are named by their email here too; the key is only what
		// profile selection works with.
		shown := s.Name
		if p, ok := profileByKey(cfg, s.Name); ok {
			shown = accountDisplay(p)
		}
		fmt.Fprintf(&sb, "• %s (%s) — %s, %d running (%s)\n",
			htmlEscape(shown), htmlEscape(s.Engine), htmlEscape(profileUsageLine(s.Engine, s.Usage)),
			s.WorkingAgents, state)
	}
	if findings := in.sched.findingsSnapshot(); len(findings) > 0 {
		sb.WriteString("\n<b>Doctor</b>\n")
		for _, f := range findings {
			fmt.Fprintf(&sb, "• %s: %s\n", htmlEscape(f.Profile), htmlEscape(f.Problem))
		}
	}
	return sb.String()
}

// reply answers in the same chat/topic the message came from.
func (in *instance) reply(msg *TelegramMessage, html string) {
	cfg := in.config()
	if cfg.BotToken == "" {
		return
	}
	if _, err := sendMessageHTMLGetID(cfg, msg.Chat.ID, msg.MessageThreadID, html); err != nil {
		hookLog("reply failed: %v", err)
	}
}

// editCallbackMessage rewrites the message a button lived on, which both shows
// the result and retires the buttons.
func (in *instance) editCallbackMessage(cb *CallbackQuery, html string) {
	empty := emptyInlineKeyboard()
	in.editCallbackMarkup(cb, html, &empty)
}

func (in *instance) editCallbackMarkup(cb *CallbackQuery, html string, buttons *[][]InlineKeyboardButton) {
	cfg := in.config()
	if cb == nil || cb.Message == nil || cfg.BotToken == "" {
		return
	}
	_ = editMessageHTMLMarkup(cfg, cb.Message.Chat.ID, int64(cb.Message.MessageID), cb.Message.MessageThreadID, html, buttons) // safe-ignore: cosmetic
}

// handleEngineCommand assigns a bot to an engine's account pool. Engine itself
// is defined when the account is added (`/account add <identity> <engine>`).
// This command is secondary: it is how a bot in a mixed instance picks which
// pool to run on. Switching pools rotates the session — conversation ids are
// per CLI.
func (in *instance) handleEngineCommand(msg *TelegramMessage, b *Bot, rest string) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		engine := botEngine(b)
		n := len(listProfilesForEngine(in.config(), engine))
		in.reply(msg, "<b>Engine</b>\n<code>"+htmlEscape(engine)+"</code> ("+htmlEscape(engineLabel(engine))+") · "+
			fmt.Sprintf("%d account(s) in this pool", n)+
			"\nEngine is set when you add an account (<code>/account add &lt;identity&gt; &lt;engine&gt;</code>). "+
			"/engine assigns this session to another pool.")
		return
	}
	engine, err := parseEngine(rest)
	if err != nil {
		in.reply(msg, htmlEscape(err.Error()))
		return
	}
	if botEngine(b) == engine {
		in.reply(msg, "⚙️ Already on <code>"+htmlEscape(engine)+"</code>.")
		return
	}
	if err := in.db.Model(&Bot{}).Where("id = ?", b.ID).
		Updates(map[string]any{"engine": engine, "session_id": "", "model": ""}).Error; err != nil {
		in.reply(msg, "Could not update the engine: "+htmlEscape(err.Error()))
		return
	}
	reply := "⚙️ This session now uses the <code>" + htmlEscape(engine) + "</code> account pool (" + htmlEscape(engineLabel(engine)) + "); the next message starts a fresh conversation."
	if n := len(listProfilesForEngine(in.config(), engine)); n == 0 {
		reply += "\nNo " + htmlEscape(engine) + " accounts yet. Add one: <code>/account add &lt;identity&gt; " + htmlEscape(engine) + "</code>."
	}
	if !engineHasMCP(engine) {
		reply += "\nNo ccc MCP tools on this engine."
	}
	in.reply(msg, reply)
}

// handleModelCommand sets a model without rotating the session: --model is
// passed on every turn, including resumes.
//
//	/model                         each account and the model it is on (picker)
//	/model <slug>                  Claude's instance default
//	/model <engine> <slug>         instance default for that engine
//	/model default                 clear Claude's instance default
//	/model <engine> default        clear that engine's instance default
//
// Engine is a property of the account; model is not. A slug that happens to
// be an engine name in a one-arg /model is treated as the engine, not a model.
func (in *instance) handleModelCommand(msg *TelegramMessage, rest string, b *Bot) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		in.postModelStatus(msg.Chat.ID, msg.MessageThreadID, b)
		return
	}
	first, more := splitFirstWord(rest)
	second, extra := splitFirstWord(more)
	if strings.TrimSpace(extra) != "" {
		in.reply(msg, "Usage: /model [&lt;engine&gt;] &lt;slug|default&gt;")
		return
	}

	if second != "" {
		engine, err := parseEngine(first)
		if err != nil {
			in.reply(msg, htmlEscape(err.Error()))
			return
		}
		in.reply(msg, in.applyEngineModel(engine, second))
		return
	}

	if strings.EqualFold(first, "default") {
		if b != nil {
			if err := in.db.Model(&Bot{}).Where("id = ?", b.ID).Update("model", "").Error; err != nil {
				in.reply(msg, "Could not update the session: "+htmlEscape(err.Error()))
				return
			}
			in.reply(msg, "🧠 This session now uses the instance default (<code>"+
				htmlEscape(firstNonEmpty(resolveModel(in.config(), botEngine(b), ""), "(engine default)"))+
				"</code>).")
			return
		}
		in.reply(msg, in.applyEngineModel(engineClaude, ""))
		return
	}

	if engine, err := parseEngine(first); err == nil && first != "" {
		slug := resolveModel(in.config(), engine, "")
		in.reply(msg, "<b>"+htmlEscape(engineLabel(engine))+"</b>\n<code>"+
			htmlEscape(firstNonEmpty(slug, "(engine default)"))+"</code>\nSet it with /model "+
			htmlEscape(engine)+" &lt;slug&gt;.")
		return
	}

	if b != nil {
		if err := in.db.Model(&Bot{}).Where("id = ?", b.ID).Update("model", first).Error; err != nil {
			in.reply(msg, "Could not update the session: "+htmlEscape(err.Error()))
			return
		}
		in.reply(msg, "🧠 This session's model set to <code>"+htmlEscape(first)+"</code>")
		return
	}

	in.reply(msg, in.applyEngineModel(engineClaude, first))
}

// applyEngineModel writes the instance default for one engine and returns the
// confirmation HTML. slug "default" (or empty) clears it.
func (in *instance) applyEngineModel(engine, slug string) string {
	if strings.EqualFold(slug, "default") {
		slug = ""
	}
	updated := updateConfig(func(c *Config) bool {
		setEngineModel(c, engine, slug)
		return true
	})
	if updated == nil {
		return "Could not write the configuration."
	}
	in.setConfig(updated)
	if engine == engineClaude {
		if slug == "" {
			return "🧠 Claude model reset to <code>(engine default)</code>"
		}
		return "🧠 Model set to <code>" + htmlEscape(slug) + "</code>"
	}
	return "🧠 " + htmlEscape(engineLabel(engine)) + " model set to <code>" +
		htmlEscape(firstNonEmpty(slug, "(engine default)")) + "</code>"
}

func (in *instance) postModelStatus(chatID, topicID int64, b *Bot) {
	cfg := in.config()
	body, buttons := renderModelStatus(cfg, b)
	if cfg.BotToken == "" {
		return
	}
	if len(buttons) == 0 {
		_, _ = sendMessageHTMLGetID(cfg, chatID, topicID, body) // safe-ignore: a failed status card is not worth failing the command
		return
	}
	_, _ = sendMessageKeyboardGetID(cfg, chatID, topicID, body, buttons) // safe-ignore: same
}

func (in *instance) postModelPicker(chatID, topicID int64, p Profile) {
	cfg := in.config()
	body, buttons := renderModelPicker(cfg, p)
	if cfg.BotToken == "" {
		return
	}
	if len(buttons) == 0 {
		_, _ = sendMessageHTMLGetID(cfg, chatID, topicID, body) // safe-ignore: cosmetic
		return
	}
	_, _ = sendMessageKeyboardGetID(cfg, chatID, topicID, body, buttons) // safe-ignore: same
}

// handleModelCallback answers the /model picker: pick an account, then a slug
// for that account's engine (storage stays per-engine, DESIGN §14.27).
func (in *instance) handleModelCallback(cb *CallbackQuery, parts []string) {
	if len(parts) < 2 || cb.Message == nil {
		return
	}
	chatID, topicID := cb.Message.Chat.ID, cb.Message.MessageThreadID
	switch parts[1] {
	case "pick":
		if len(parts) < 3 {
			return
		}
		p, ok := resolveAccountTarget(in.config(), parts[2])
		if !ok {
			in.editCallbackMessage(cb, "That account is gone.")
			return
		}
		in.postModelPicker(chatID, topicID, p)
	case "set":
		if len(parts) < 4 {
			return
		}
		engine, err := parseEngine(parts[2])
		if err != nil {
			in.editCallbackMessage(cb, htmlEscape(err.Error()))
			return
		}
		in.post(chatID, topicID, in.applyEngineModel(engine, parts[3]))
	case "clear":
		if len(parts) < 3 {
			return
		}
		engine, err := parseEngine(parts[2])
		if err != nil {
			in.editCallbackMessage(cb, htmlEscape(err.Error()))
			return
		}
		in.post(chatID, topicID, in.applyEngineModel(engine, ""))
	}
}

func renderModelStatus(cfg *Config, b *Bot) (string, [][]InlineKeyboardButton) {
	var sb strings.Builder
	sb.WriteString("<b>Model</b>\n")
	if b != nil {
		engine := botEngine(b)
		effective := resolveModel(cfg, engine, b.Model)
		fmt.Fprintf(&sb, "this session (%s): <code>%s</code>", htmlEscape(engineLabel(engine)),
			htmlEscape(firstNonEmpty(effective, "(engine default)")))
		if strings.TrimSpace(b.Model) != "" {
			sb.WriteString(" (override)")
		}
		sb.WriteByte('\n')
	}
	profiles := listProfiles(cfg)
	if len(profiles) == 0 {
		sb.WriteString("No accounts. <code>/account add &lt;identity&gt; &lt;engine&gt;</code>")
		return sb.String(), nil
	}
	var buttons [][]InlineKeyboardButton
	for _, p := range profiles {
		engine := profileEngine(p)
		slug := profileEffectiveModel(cfg, p)
		shown := firstNonEmpty(slug, "default")
		name := accountDisplay(p)
		fmt.Fprintf(&sb, "• %s · <code>%s</code>", htmlEscape(engine), htmlEscape(shown))
		if name != "" && !strings.EqualFold(name, engine) {
			fmt.Fprintf(&sb, " — %s", htmlEscape(name))
		}
		sb.WriteByte('\n')
		label := engine + " · " + shown
		if name != "" && !strings.EqualFold(name, engine) {
			label = name + " · " + shown
		}
		buttons = append(buttons, []InlineKeyboardButton{{
			Text:         clipButtonText(label),
			CallbackData: "model:pick:" + accountButtonTarget(p),
		}})
	}
	sb.WriteString("Tap an account to pick its model. /model &lt;slug&gt; sets Claude's instance default. ")
	sb.WriteString("/model &lt;engine&gt; &lt;slug&gt; sets that engine. /model default clears.")
	return sb.String(), buttons
}

func renderModelPicker(cfg *Config, p Profile) (string, [][]InlineKeyboardButton) {
	engine := profileEngine(p)
	current := profileEffectiveModel(cfg, p)
	shown := firstNonEmpty(current, "(engine default)")
	var sb strings.Builder
	fmt.Fprintf(&sb, "<b>%s</b> — %s\nCurrent: <code>%s</code>\n",
		htmlEscape(accountDisplay(p)), htmlEscape(engine), htmlEscape(shown))
	sb.WriteString("Tap a slug, or /model " + htmlEscape(engine) + " &lt;slug&gt;.")

	var buttons [][]InlineKeyboardButton
	row := []InlineKeyboardButton{}
	flush := func() {
		if len(row) == 0 {
			return
		}
		buttons = append(buttons, row)
		row = nil
	}
	for _, slug := range engineModelChoices(engine, p, current) {
		data := "model:set:" + engine + ":" + slug
		if len(data) > 64 {
			continue
		}
		text := slug
		if slug == current {
			text = "✓ " + slug
		}
		row = append(row, InlineKeyboardButton{Text: clipButtonText(text), CallbackData: data})
		if len(row) == 2 {
			flush()
		}
	}
	flush()
	buttons = append(buttons, []InlineKeyboardButton{{
		Text: "engine default", CallbackData: "model:clear:" + engine,
	}})
	return sb.String(), buttons
}

func clipButtonText(s string) string {
	if len(s) <= 64 {
		return s
	}
	return s[:64]
}

func renderInstanceModels(cfg *Config) string {
	parts := []string{}
	for _, e := range []string{engineClaude, engineGrok, engineAntigravity, engineCodex} {
		slug := resolveModel(cfg, e, "")
		if slug == "" && e != engineClaude && (cfg == nil || cfg.Models == nil || cfg.Models[e] == "") {
			continue
		}
		label := e
		if slug == "" {
			slug = "default"
		}
		parts = append(parts, label+"="+slug)
	}
	if len(parts) == 0 {
		return "claude=default"
	}
	return strings.Join(parts, " · ")
}

// setConfig swaps the instance's configuration and hands the new one to the
// runner, so the next turn already uses it.
func (in *instance) setConfig(cfg *Config) {
	in.mu.Lock()
	in.cfg = cfg
	in.mu.Unlock()
	if r, ok := in.runner.(*Runner); ok {
		r.setConfig(cfg)
	}
}

// handleWatchesCommand lists or cancels this bot's watches (DESIGN §8).
func (in *instance) handleWatchesCommand(msg *TelegramMessage, b *Bot, rest string) {
	action, name := splitFirstWord(rest)
	if strings.EqualFold(action, "cancel") || strings.EqualFold(action, "remove") {
		if strings.TrimSpace(name) == "" {
			in.reply(msg, "Usage: /watches cancel &lt;name&gt;")
			return
		}
		removed, err := deleteWatch(in.db, b.ID, name)
		if err != nil {
			in.reply(msg, "Could not remove it: "+htmlEscape(err.Error()))
			return
		}
		if !removed {
			in.reply(msg, "No watch by that name.")
			return
		}
		in.reply(msg, "🗑 Removed watch <b>"+htmlEscape(name)+"</b>")
		return
	}

	watches, err := listWatches(in.db, b.ID)
	if err != nil {
		in.reply(msg, "Could not list watches: "+htmlEscape(err.Error()))
		return
	}
	if len(watches) == 0 {
		in.reply(msg, "No watches. The bot creates them itself with the <code>watch</code> tool.")
		return
	}
	var sb strings.Builder
	sb.WriteString("<b>Watches</b>\n")
	for _, w := range watches {
		last := "never run"
		if w.LastRunAt != nil {
			last = humanDuration(time.Since(*w.LastRunAt)) + " ago"
		}
		state := ""
		if !w.Enabled {
			state = " (disabled)"
		}
		fmt.Fprintf(&sb, "• <b>%s</b>%s — every %ds, last %s, %s\n  <code>%s</code>\n",
			htmlEscape(w.Name), state, w.IntervalS, last, watchExpiryLabel(w, time.Now(), watchTTL(in.config())),
			htmlEscape(truncate(w.Command, 200)))
	}
	sb.WriteString("\nCancel one with /watches cancel &lt;name&gt;")
	in.reply(msg, sb.String())
}

// handleSchedulesCommand lists or cancels this bot's pending wakeups.
func (in *instance) handleSchedulesCommand(msg *TelegramMessage, b *Bot, rest string) {
	action, idText := splitFirstWord(rest)
	if strings.EqualFold(action, "cancel") || strings.EqualFold(action, "remove") {
		id, err := strconv.ParseInt(strings.TrimSpace(idText), 10, 64)
		if err != nil {
			in.reply(msg, "Usage: /schedules cancel &lt;id&gt;")
			return
		}
		res := in.db.Where("id = ? AND bot_id = ?", id, b.ID).Delete(&Schedule{})
		if res.RowsAffected == 0 {
			in.reply(msg, "No schedule with that id on this bot.")
			return
		}
		in.reply(msg, fmt.Sprintf("🗑 Cancelled schedule #%d", id))
		return
	}

	schedules, err := listSchedules(in.db, b.ID)
	if err != nil {
		in.reply(msg, "Could not list schedules: "+htmlEscape(err.Error()))
		return
	}
	if len(schedules) == 0 {
		in.reply(msg, "No pending wakeups. The bot schedules its own with the <code>schedule_wakeup</code> tool.")
		return
	}
	var sb strings.Builder
	sb.WriteString("<b>Schedules</b>\n")
	for _, s := range schedules {
		repeat := ""
		if s.RecurringCron != "" {
			repeat = " (repeats: " + htmlEscape(s.RecurringCron)
			if s.Timezone != "" {
				repeat += " " + htmlEscape(s.Timezone)
			}
			repeat += ")"
		}
		label := fmt.Sprintf("#%d", s.ID)
		if s.Name != "" {
			label = htmlEscape(s.Name) + " " + label
		}
		fmt.Fprintf(&sb, "• <b>%s</b> %s%s\n  %s\n",
			label, s.FireAt.Format("2006-01-02 15:04"), repeat, htmlEscape(truncate(s.Note, 200)))
	}
	sb.WriteString("\nCancel one with /schedules cancel &lt;id&gt;")
	in.reply(msg, sb.String())
}
