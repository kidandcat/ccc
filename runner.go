package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"gorm.io/gorm"
)

// runner.go drives the whole turn lifecycle of DESIGN §3: queue an input,
// pick a profile, render the envelope, spawn `claude -p`, stream the events
// into a Telegram progress message, persist the result, and fail over to
// another account when the first one is out of credit or logged out.

// ---------------------------------------------------------------------------
// The `claude -p` flag set (DESIGN §3.1 / §10)
// ---------------------------------------------------------------------------
//
// Every flag below was verified empirically against the installed Claude Code
// (2.1.270, ARM macOS) with a scrubbed environment, because the isolation
// flags do not behave the way their names suggest. The probe used a workspace
// holding a CLAUDE.md with the token ZORBLAX and a user memory
// (~/.claude/CLAUDE.md) holding the token "Litestream", and asked the model
// which tokens were in its context:
//
//	no flags                      -> PROJ=yes USER=yes
//	--system-prompt only          -> PROJ=yes USER=yes   (!)
//	--setting-sources ''          -> PROJ=no  USER=no
//	--setting-sources user        -> PROJ=no  USER=yes
//
// So --system-prompt does NOT suppress CLAUDE.md, and the empty
// --setting-sources is the only combination that loads no CLAUDE.md at all.
//
// Flags, one comment per flag with the reason it is there:
//
//	-p                          non-interactive; the whole runner model.
//	--session-id <uuid>         first turn of a session; ccc mints the UUID so
//	                            it can resume later (verified: minting works).
//	--resume <uuid>             later turns. Verified: a resumed turn sees the
//	                            earlier turn's content (NUM=4242 came back).
//	--output-format stream-json  event stream for the progress message.
//	--verbose                   stream-json is refused under -p without it.
//	--permission-mode bypassPermissions
//	                            DESIGN's decision: all bots bypass. Verified to
//	                            work together with --setting-sources '' (a Bash
//	                            call ran unprompted), i.e. the once-per-config-dir
//	                            disclaimer acceptance is not read from the user
//	                            settings file that flag suppresses.
//	--setting-sources ''        loads no user/project/local settings AND no
//	                            CLAUDE.md at any level (see probe above). This
//	                            is a deliberate deviation from DESIGN §3.1's
//	                            `--setting-sources user`, which leaks the
//	                            owner's personal ~/.claude/CLAUDE.md into every
//	                            bot. Auth is unaffected (OAuth lives in the
//	                            keychain/credentials, not in settings.json).
//	--disable-slash-commands    "Disable all skills": no user/plugin skills, so
//	                            a bot cannot be steered by whatever the owner
//	                            has installed. Accepted under -p.
//	--strict-mcp-config         ignore every MCP server except ours.
//	--mcp-config <inline json>  the ccc MCP server for this bot+turn (§6).
//	--system-prompt <text>      replaces Claude Code's own prompt (§9).
//	--model <name>              instance model; omitted to accept claude's default.
//
// Deliberately NOT used:
//
//	--bare                      disables OAuth entirely (would need an API key).
//	--safe-mode                 also disables MCP servers, which kills our tools;
//	                            and the probe showed it still loaded CLAUDE.md.
//	--append-system-prompt      the system prompt is snapshotted per conversation
//	                            (see below), so per-turn text must go in the
//	                            envelope, not here.
//	--system-prompt-snapshot off  verified NOT to help: a resumed session whose
//	                            launch passed a different --system-prompt still
//	                            answered with the ORIGINAL prompt's secret word
//	                            with the flag set to off. DESIGN §9's envelope
//	                            is therefore load-bearing, and /role rotates the
//	                            session.
//	--include-partial-messages  per-message granularity is enough for progress.
//
// All flags are passed on EVERY turn: --mcp-config, --settings and friends are
// not restored on resume.
func claudeTurnArgs(model, systemPrompt, mcpConfig, sessionID string, resume bool) []string {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", "bypassPermissions",
		"--setting-sources", "",
		"--disable-slash-commands",
		"--strict-mcp-config",
		"--mcp-config", mcpConfig,
		"--system-prompt", systemPrompt,
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if resume {
		args = append(args, "--resume", sessionID)
	} else {
		args = append(args, "--session-id", sessionID)
	}
	return args
}

// claudePlainArgs is the flag set for a turn that belongs to no bot: the memory
// compaction of DESIGN §7. It is deliberately much smaller than a bot's turn —
// a fresh session nobody resumes, no MCP server, no tools worth reaching for,
// and plain text instead of an event stream, because the caller wants one
// string back and there is no progress message to feed.
//
//	--session-id <uuid>         a fresh conversation every time; never resumed.
//	--output-format text        one string on stdout; no stream to consume.
//	--setting-sources ''        same isolation as a bot's turn: no CLAUDE.md,
//	                            no settings, no auto-memory (see above).
//	--disable-slash-commands    no installed skill can steer it.
//	--strict-mcp-config +
//	  --mcp-config {mcpServers:{}}   no MCP servers at all, ccc's included: a
//	                            compaction turn must not touch the database it
//	                            is being run to compact.
//	--system-prompt <text>      replaces Claude Code's own (long) prompt with
//	                            one line, which is all this job needs.
//	--model <name>              the cheap compaction model (setting), or the
//	                            instance model when that one is not known.
//
// No --permission-mode: this turn is a text transform, so the default (which
// refuses tools under -p rather than running them) is exactly right.
func claudePlainArgs(model, sessionID string) []string {
	args := []string{
		"-p",
		"--output-format", "text",
		"--session-id", sessionID,
		"--setting-sources", "",
		"--disable-slash-commands",
		"--strict-mcp-config",
		"--mcp-config", `{"mcpServers":{}}`,
		"--system-prompt", plainTurnSystemPrompt,
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	return args
}

const plainTurnSystemPrompt = "You are a text-processing tool. Follow the instructions in the message exactly " +
	"and output only what they ask for, with no preamble and no commentary."

// plainTurnTimeout caps a bot-less turn. Compaction is one pass over a few
// hundred short lines; anything slower than this is stuck.
const plainTurnTimeout = 10 * time.Minute

// ---------------------------------------------------------------------------
// Runner
// ---------------------------------------------------------------------------

// botUI is the Telegram surface the runner needs. It is an interface so the
// conversation tests can drive the runner (or replace it) without a network.
type botUI interface {
	Post(topicID int64, html string) (int64, error)
	Edit(topicID, msgID int64, html string) error
	React(messageID int64, emoji string)
}

// turnRunner is what the Telegram layer sees of the runner, so the flow tests
// can inject a fake that records calls instead of spawning claude.
type turnRunner interface {
	Enqueue(botID int64, source, text string, triggerMessageID int64) (*Turn, error)
	Stop(botID int64) bool
}

// activeTurn is a turn with a live `claude` process behind it.
type activeTurn struct {
	cmd     *exec.Cmd
	stopped bool
}

// Runner owns the per-bot turn queues.
type Runner struct {
	db *gorm.DB
	ui botUI

	mu     sync.Mutex
	cfg    *Config
	active map[int64]*activeTurn
	wake   map[int64]chan struct{}
	done   chan struct{}
	// needsLogin holds profiles a turn found logged out; they are skipped until
	// the owner re-logs in (the doctor loop in Phase 2b clears them).
	needsLogin map[string]bool
}

func newRunner(db *gorm.DB, cfg *Config, ui botUI) *Runner {
	return &Runner{
		db:         db,
		ui:         ui,
		cfg:        cfg,
		active:     map[int64]*activeTurn{},
		wake:       map[int64]chan struct{}{},
		done:       make(chan struct{}),
		needsLogin: map[string]bool{},
	}
}

func (r *Runner) config() *Config {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cfg
}

// Close stops the per-bot loops. Running turns are left to finish.
func (r *Runner) Close() {
	r.mu.Lock()
	select {
	case <-r.done:
	default:
		close(r.done)
	}
	r.mu.Unlock()
}

// Enqueue records an input for a bot and wakes its loop.
func (r *Runner) Enqueue(botID int64, source, text string, triggerMessageID int64) (*Turn, error) {
	t := &Turn{
		BotID:            botID,
		Source:           source,
		Input:            text,
		Status:           turnQueued,
		TriggerMessageID: triggerMessageID,
	}
	if err := r.db.Create(t).Error; err != nil {
		return nil, err
	}
	r.kick(botID)
	return t, nil
}

// Stop SIGTERMs the bot's running turn and drops its queue (/stop, DESIGN §8).
// The child is started in its own process group, so the signal reaches the
// tools it spawned (a long `go test`, say) and not just the claude wrapper.
func (r *Runner) Stop(botID int64) bool {
	r.mu.Lock()
	at := r.active[botID]
	if at != nil {
		at.stopped = true
	}
	r.mu.Unlock()

	r.db.Model(&Turn{}).Where("bot_id = ? AND status = ?", botID, turnQueued).
		Updates(map[string]any{"status": turnFailed, "stop_reason": "dropped by /stop"})

	if at == nil || at.cmd == nil || at.cmd.Process == nil {
		return false
	}
	pid := at.cmd.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		_ = at.cmd.Process.Signal(syscall.SIGTERM) // safe-ignore: best-effort fallback when the group kill fails
	}
	return true
}

// PlainTurn runs one model call outside any bot (DESIGN §7's memory
// compaction): a fresh session, no tools, plain text in and out. It picks a
// profile the same way a bot's turn does, so a logged-out or rate-limited
// account is skipped here too, but it does not fail over: maintenance can wait
// for tomorrow.
func (r *Runner) PlainTurn(model, prompt string) (string, error) {
	p, ok := r.pickAccount(engineClaude, nil)
	if !ok {
		return "", errors.New("no healthy Claude profile available")
	}
	cfg := r.config()
	// The transcript of this turn lands in the shared projects/ dir keyed by
	// the working directory; the data dir keeps it out of any bot's workspace.
	cwd := dataDir(cfg)
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		return "", fmt.Errorf("create data dir: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), plainTurnTimeout)
	defer cancel()
	args := append(claudePlainArgs(model, newUUID()), prompt)
	cmd := exec.CommandContext(ctx, claudeBin(), args...)
	cmd.Dir = cwd
	cmd.Env = botEnv(cfg, p)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		return "", fmt.Errorf("%w: %s", err, truncate(detail, 500))
	}
	out := strings.TrimSpace(stdout.String())
	// `claude -p` reports some refusals on stdout with a zero exit code (an
	// unknown --model is one), so a plausible-looking failure is an error here
	// rather than a "result" the caller would try to parse.
	if out == "" {
		return "", fmt.Errorf("claude returned nothing: %s", truncate(strings.TrimSpace(stderr.String()), 300))
	}
	if isUnknownModelError(out) {
		return "", errors.New(truncate(out, 500))
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Input debounce (DESIGN §14.18)
// ---------------------------------------------------------------------------

// debounceDuration is how long an idle bot waits for more messages before it
// starts a turn. Chat arrives in bursts — a sentence, then the correction, then
// the link — and each one becoming its own `claude -p` run is the single most
// wasteful thing ccc can do with the owner's tokens.
func (r *Runner) debounceDuration() time.Duration {
	return time.Duration(debounceMS(r.config())) * time.Millisecond
}

// settleQueue waits until the bot's queue has been quiet for one debounce
// window, so everything the owner is still typing lands in the same turn.
//
// Three things keep it from parking a bot:
//   - only a queue whose newest input came from the owner waits; a watch, a
//     schedule or another bot is delivering one thing, not a burst;
//   - inputs that queued while the previous turn ran are already older than the
//     window, so the wait is zero and the next turn starts immediately;
//   - the total wait is capped, so a stream of messages still gets an answer.
func (r *Runner) settleQueue(botID int64) {
	window := r.debounceDuration()
	if window <= 0 {
		return
	}
	deadline := time.Now().Add(4 * window)
	for {
		var newest Turn
		err := r.db.Where("bot_id = ? AND status = ?", botID, turnQueued).
			Order("id DESC").First(&newest).Error
		if err != nil || newest.Source != sourceUser {
			return
		}
		wait := window - time.Since(newest.CreatedAt)
		if wait <= 0 || time.Now().After(deadline) {
			return
		}
		select {
		case <-r.done:
			return
		case <-time.After(wait):
		}
	}
}

// kick starts (once) and wakes the bot's turn loop.
func (r *Runner) kick(botID int64) {
	r.mu.Lock()
	ch, ok := r.wake[botID]
	if !ok {
		ch = make(chan struct{}, 1)
		r.wake[botID] = ch
		go r.loop(botID, ch)
	}
	r.mu.Unlock()
	select {
	case ch <- struct{}{}:
	default: // safe-ignore: a pending wake already covers this input
	}
}

func (r *Runner) loop(botID int64, wake chan struct{}) {
	for {
		select {
		case <-r.done:
			return
		case <-wake:
		}
		for r.runNext(botID) {
			select {
			case <-r.done:
				return
			default:
			}
		}
	}
}

// runNext runs at most one turn for a bot and reports whether it did. All
// inputs queued at this moment are folded into that single turn (DESIGN §2:
// "further inputs queue (FIFO) and are delivered together on the next turn").
func (r *Runner) runNext(botID int64) bool {
	b, err := botByID(r.db, botID)
	if err != nil {
		return false
	}
	if b.Status == botWaiting || b.Status == botDisabled || b.ArchivedAt != nil {
		return false
	}
	// Give a burst of chat messages the chance to arrive before the turn that
	// will carry all of them starts.
	r.settleQueue(botID)
	head, input, triggers, ok := foldQueue(r.db, botID)
	if !ok {
		return false
	}
	r.execute(b, head, input, triggers)
	return true
}

// foldQueue takes every input queued for a bot right now and folds it into the
// oldest one, which becomes the turn that runs. The others are closed as
// "merged" so a burst of chat messages costs one `claude -p` run, not one per
// message (DESIGN §2). It returns the carrier turn, the combined input and
// every Telegram message that should get a ✅ when the turn lands.
func foldQueue(db *gorm.DB, botID int64) (*Turn, string, []int64, bool) {
	var queued []Turn
	if err := db.Where("bot_id = ? AND status = ?", botID, turnQueued).Order("id").Find(&queued).Error; err != nil {
		return nil, "", nil, false
	}
	if len(queued) == 0 {
		return nil, "", nil, false
	}
	head := queued[0]
	inputs := []string{head.Input}
	var triggers []int64
	if head.TriggerMessageID != 0 {
		triggers = append(triggers, head.TriggerMessageID)
	}
	for _, extra := range queued[1:] {
		inputs = append(inputs, extra.Input)
		if extra.TriggerMessageID != 0 {
			triggers = append(triggers, extra.TriggerMessageID)
		}
		db.Model(&Turn{}).Where("id = ?", extra.ID).
			Updates(map[string]any{"status": turnDone, "stop_reason": "merged into turn " + fmt.Sprint(head.ID)})
	}
	return &head, strings.Join(inputs, "\n\n"), triggers, true
}

// execute runs one turn end to end, including profile failover (DESIGN §3.4).
func (r *Runner) execute(b *Bot, t *Turn, input string, triggers []int64) {
	now := time.Now()
	roleAtStart := b.Role
	nameAtStart := b.Name
	r.db.Model(&Turn{}).Where("id = ?", t.ID).
		Updates(map[string]any{"status": turnRunning, "started_at": now, "input": input})
	setBotStatus(r.db, b.ID, botRunning)

	prog := newProgress(r.ui, b.TopicID, now)
	prog.set("thinking")

	envelope := buildEnvelope(r.db, b, t.Source, input, now)
	// Quiet inbox rows (wake=false) are consumed by the envelope's summary, so
	// mark them delivered and stop re-announcing them. Waking ones are NOT
	// touched here: deliverInbox turns each of those into a turn of its own.
	r.db.Model(&InboxMessage{}).Where("to_bot_id = ? AND delivered_at IS NULL AND wake = ?", b.ID, false).
		Updates(map[string]any{"delivered_at": now, "turn_id": t.ID})

	tried := map[string]bool{}
	var res *streamResult
	var class string
	var lastErr string
	engine := botEngine(b)

	for attempt := 0; attempt < 3; attempt++ {
		p, ok := r.pickAccount(engine, tried)
		if !ok {
			class = errFatal
			lastErr = "no healthy " + engineLabel(engine) + " account available"
			break
		}
		tried[p.Name] = true
		r.db.Model(&Turn{}).Where("id = ?", t.ID).Update("profile", p.Name)

		sessionID, resume := r.sessionFor(b)
		res = r.spawn(p, b, t, sessionID, resume, envelope, prog)
		if res.ok() {
			r.persistSession(b, sessionID, resume, res)
			class = ""
			break
		}
		if res.stopped {
			class = "stopped"
			lastErr = "stopped by /stop"
			break
		}
		lastErr = res.failureText()
		class = classifyFailure(lastErr, res.exitCode)
		who := p.Name
		if who == "" {
			who = engine
		}
		hookLog("turn %d on %s failed (%s): %s", t.ID, who, class, truncate(lastErr, 300))

		switch class {
		case errSessionLost:
			// The transcript is not in this profile's projects/ (or was
			// deleted). Start a fresh conversation rather than losing the turn.
			r.db.Model(&Bot{}).Where("id = ?", b.ID).Update("session_id", "")
			b.SessionID = ""
			delete(tried, p.Name)
			continue
		case errAuthStale:
			r.markNeedsLogin(p)
			continue
		case errRateLimited:
			noteProfileLimit(p, time.Now())
			continue
		case errTransient:
			time.Sleep(10 * time.Second)
			delete(tried, p.Name)
			continue
		default:
			class = errFatal
		}
		break
	}

	end := time.Now()
	if class == "" && res != nil {
		r.db.Model(&Turn{}).Where("id = ?", t.ID).Updates(map[string]any{
			"status": turnDone, "output": res.Text, "ended_at": end,
			"stop_reason": res.Subtype, "usage_json": res.UsageJSON, "session_id": b.SessionID,
		})
		prog.finish(res.Text)
		for _, m := range triggers {
			r.ui.React(m, "✅")
		}
	} else {
		r.db.Model(&Turn{}).Where("id = ?", t.ID).Updates(map[string]any{
			"status": turnFailed, "ended_at": end, "error_class": class,
			"stop_reason": truncate(lastErr, 500), "session_id": b.SessionID,
		})
		prog.finish(failureMessage(class, lastErr))
	}

	// update_instructions or set_name may have rewritten the role or the name
	// mid-turn. Both are in the system prompt, which is recorded per
	// conversation, so they can only take effect in a new one — rotate the
	// session now that the turn has written its id. Doing it here also repairs
	// the session id a mid-turn rename cleared and the fresh-session write
	// above put back.
	if after, err := botByID(r.db, b.ID); err == nil && (after.Role != roleAtStart || after.Name != nameAtStart) {
		r.db.Model(&Bot{}).Where("id = ?", b.ID).Update("session_id", "")
	}

	// A turn that asked the owner something leaves the bot waiting; otherwise
	// it goes back to idle and the loop drains whatever queued meanwhile.
	if r.hasPendingQuestion(b.ID) {
		setBotStatus(r.db, b.ID, botWaiting)
	} else {
		setBotStatus(r.db, b.ID, botIdle)
	}
	r.deliverInbox(b.ID)
}

func (r *Runner) hasPendingQuestion(botID int64) bool {
	var n int64
	r.db.Model(&Question{}).Where("bot_id = ? AND answered_at IS NULL", botID).Count(&n)
	return n > 0
}

// deliverInbox turns every waking message produced by a turn into a queued
// turn on its target bot (DESIGN §3.3: delivery happens post-turn). A
// fromBotID of 0 means "everything still pending", which is how a restarted
// listener picks up messages whose sender's turn ended as the process died.
func (r *Runner) deliverInbox(fromBotID int64) {
	q := r.db.Where("delivered_at IS NULL AND wake = ?", true)
	if fromBotID > 0 {
		q = q.Where("from_bot_id = ?", fromBotID)
	}
	var pending []InboxMessage
	if err := q.Order("id").Find(&pending).Error; err != nil {
		return
	}
	now := time.Now()
	for _, m := range pending {
		turn := &Turn{
			BotID:  m.ToBotID,
			Source: sourceBot,
			Input:  inboxInput(r.db, m),
			Status: turnQueued,
		}
		if err := r.db.Create(turn).Error; err != nil {
			hookLog("inbox delivery: %v", err)
			continue
		}
		r.db.Model(&InboxMessage{}).Where("id = ?", m.ID).
			Updates(map[string]any{"delivered_at": now, "turn_id": turn.ID})
		r.kick(m.ToBotID)
	}
}

// inboxInput labels a delivered message with who sent it, so the receiving bot
// can tell a teammate's request from the owner's.
func inboxInput(db *gorm.DB, m InboxMessage) string {
	sender := "the owner"
	if m.FromBotID != nil {
		if from, err := botByID(db, *m.FromBotID); err == nil {
			sender = from.Name
		} else {
			sender = "another bot"
		}
	}
	return fmt.Sprintf("Message from %s:\n%s", sender, m.Text)
}

// sessionFor returns the session id for the next turn and whether it is a
// resume. Claude and Grok get a UUID minted by ccc. Antigravity mints its
// own conversation_id on the first turn (captured from the stream), so an
// empty id is left empty rather than inventing one agy would ignore.
func (r *Runner) sessionFor(b *Bot) (string, bool) {
	if strings.TrimSpace(b.SessionID) != "" {
		return b.SessionID, true
	}
	if botEngine(b) == engineAntigravity {
		return "", false
	}
	return newUUID(), false
}

// persistSession writes the conversation id after a successful turn. Claude
// keeps the historical "mint on first turn, leave it on resume" write. Grok
// is the same (ccc-minted UUID). Antigravity mints its own conversation_id,
// which the stream parser puts on res.SessionID.
func (r *Runner) persistSession(b *Bot, sessionID string, resume bool, res *streamResult) {
	if botEngine(b) == engineClaude {
		if !resume {
			r.db.Model(&Bot{}).Where("id = ?", b.ID).Update("session_id", sessionID)
			b.SessionID = sessionID
		}
		return
	}
	sid := sessionID
	if res != nil && strings.TrimSpace(res.SessionID) != "" {
		sid = res.SessionID
	}
	if sid == "" || (resume && strings.TrimSpace(b.SessionID) != "") {
		return
	}
	r.db.Model(&Bot{}).Where("id = ?", b.ID).Update("session_id", sid)
	b.SessionID = sid
}

// ---------------------------------------------------------------------------
// Spawning and stream parsing
// ---------------------------------------------------------------------------

type streamResult struct {
	Text      string
	Subtype   string
	IsError   bool
	UsageJSON string
	// SessionID is captured from a CLI that mints its own conversation id
	// (Antigravity). Claude and Grok use the UUID ccc passed in.
	SessionID string
	exitCode  int
	stderr    string
	spawnErr  error
	stopped   bool
}

func (s *streamResult) ok() bool {
	return s != nil && s.spawnErr == nil && !s.IsError && s.exitCode == 0
}

func (s *streamResult) failureText() string {
	parts := []string{}
	if s.spawnErr != nil {
		parts = append(parts, s.spawnErr.Error())
	}
	if s.stderr != "" {
		parts = append(parts, s.stderr)
	}
	if s.IsError && s.Text != "" {
		parts = append(parts, s.Text)
	}
	if len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("engine exited %d with no output", s.exitCode))
	}
	return strings.Join(parts, "\n")
}

// spawn runs one engine process (`claude -p`, `grok --single`, or `agy --print`)
// and consumes its event stream.
func (r *Runner) spawn(p Profile, b *Bot, t *Turn, sessionID string, resume bool, envelope string, prog *progress) *streamResult {
	res := &streamResult{}
	cfg := r.config()
	cwd := b.Cwd
	if cwd == "" {
		cwd = botWorkspace(cfg, b.Name)
	}
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		res.spawnErr = fmt.Errorf("create workspace: %w", err)
		return res
	}
	sysPrompt := renderSystemPrompt(
		promptBot{Name: b.Name, Role: b.Role, Cwd: cwd, Engine: botEngine(b)}, hostnameOrUnknown(), botRoster(r.db, b.ID),
		topicIconEmoji(topicIcons(r.db, r.config())))
	mcpCfg := ""
	if botEngine(b) == engineClaude {
		mcpCfg = r.mcpConfigJSON(b.ID, t.ID)
	}
	spec, err := buildTurn(botEngine(b), p, cfg, mcpCfg, sessionID, sysPrompt, envelope, resume)
	if err != nil {
		res.spawnErr = err
		return res
	}

	cmd := exec.Command(spec.Bin, spec.Args...)
	cmd.Dir = cwd
	cmd.Env = spec.Env
	// Own process group: /stop must reach the whole tool tree, not just claude.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		res.spawnErr = err
		return res
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		res.spawnErr = err
		return res
	}

	r.mu.Lock()
	r.active[b.ID] = &activeTurn{cmd: cmd}
	r.mu.Unlock()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	sawJSON := false
	var raw strings.Builder
	for scanner.Scan() {
		line := scanner.Bytes()
		if consumeTurnEvent(spec.Stream, line, res, prog) {
			sawJSON = true
			continue
		}
		if raw.Len() > 0 {
			raw.WriteByte('\n')
		}
		raw.Write(line)
	}
	// An engine that only prints the final answer still delivers it.
	if res.Text == "" && !sawJSON {
		res.Text = strings.TrimSpace(raw.String())
	}
	waitErr := cmd.Wait()

	r.mu.Lock()
	at := r.active[b.ID]
	if at != nil {
		res.stopped = at.stopped
	}
	delete(r.active, b.ID)
	r.mu.Unlock()

	res.stderr = strings.TrimSpace(stderr.String())
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			res.exitCode = ee.ExitCode()
		} else {
			res.spawnErr = waitErr
		}
	}
	return res
}

// streamEvent is the subset of the stream-json protocol ccc reads.
type streamEvent struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	Result    string          `json:"result"`
	IsError   bool            `json:"is_error"`
	Usage     json.RawMessage `json:"usage"`
	// TotalCostUSD sits beside usage on the result event, not inside it. /usage
	// wants both, so it is folded into the stored usage object (mergeUsage).
	TotalCostUSD float64 `json:"total_cost_usd"`
	Message      struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	} `json:"message"`
}

func consumeTurnEvent(kind streamKind, line []byte, res *streamResult, prog *progress) bool {
	switch kind {
	case streamAgy:
		return consumeAgyEvent(line, res, prog)
	case streamText:
		return false
	default:
		return consumeClaudeEvent(line, res, prog)
	}
}

func (r *Runner) consumeEvent(line []byte, res *streamResult, prog *progress) {
	consumeClaudeEvent(line, res, prog) // safe-ignore: tests and the Claude stream treat a bad line as noise
}

func consumeClaudeEvent(line []byte, res *streamResult, prog *progress) bool {
	var ev streamEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return false // safe-ignore: non-JSON noise on stdout is not fatal to the turn
	}
	if ev.Type == "" {
		return false
	}
	if ev.SessionID != "" && res.SessionID == "" {
		res.SessionID = ev.SessionID
	}
	switch ev.Type {
	case "assistant":
		for _, c := range ev.Message.Content {
			switch c.Type {
			case "tool_use":
				prog.set(summarizeTool(c.Name, c.Input))
			case "text":
				if strings.TrimSpace(c.Text) != "" {
					prog.set("writing a reply")
				}
			}
			// "thinking" blocks are deliberately never surfaced (DESIGN §3.2).
		}
	case "result":
		res.Text = ev.Result
		res.Subtype = ev.Subtype
		res.IsError = ev.IsError
		res.UsageJSON = mergeUsage(ev.Usage, ev.TotalCostUSD)
	}
	return true
}

// mergeUsage stores the result event's usage object with the run's cost folded
// in under cost_usd. The cost is reported next to usage rather than inside it,
// and /usage wants one blob per turn: keeping them together means the
// aggregation reads one column and old rows (which have no cost) still parse.
func mergeUsage(usage json.RawMessage, costUSD float64) string {
	if len(usage) == 0 {
		if costUSD <= 0 {
			return ""
		}
		usage = json.RawMessage("{}")
	}
	if costUSD <= 0 {
		return string(usage)
	}
	var fields map[string]any
	if err := json.Unmarshal(usage, &fields); err != nil || fields == nil {
		return string(usage) // safe-ignore: an unreadable usage blob is stored as-is rather than dropped
	}
	fields["cost_usd"] = costUSD
	merged, err := json.Marshal(fields)
	if err != nil {
		return string(usage) // safe-ignore: same
	}
	return string(merged)
}

// summarizeTool turns a tool_use event into the one-line "what is it doing
// right now" string shown in the progress message. Tool payloads are never
// dumped into the topic (DESIGN §3.2), so only a short, safe label is built.
func summarizeTool(name string, input json.RawMessage) string {
	var in struct {
		Command     string `json:"command"`
		Description string `json:"description"`
		FilePath    string `json:"file_path"`
		Path        string `json:"path"`
		Pattern     string `json:"pattern"`
		Query       string `json:"query"`
		URL         string `json:"url"`
		Key         string `json:"key"`
		Bot         string `json:"bot"`
		Prompt      string `json:"prompt"`
	}
	if len(input) > 0 {
		_ = json.Unmarshal(input, &in) // safe-ignore: a payload we cannot read just yields a generic label
	}
	base := func(p string) string {
		if p == "" {
			return ""
		}
		return filepath.Base(p)
	}
	switch name {
	case "Bash", "BashOutput":
		if in.Description != "" {
			return lowerFirst(truncate(in.Description, 60))
		}
		return "running " + truncate(collapseWhitespace(in.Command), 60)
	case "Read", "NotebookRead":
		return "reading " + base(in.FilePath)
	case "Edit", "Write", "NotebookEdit":
		return "editing " + base(in.FilePath)
	case "Glob":
		return "looking for " + truncate(in.Pattern, 40)
	case "Grep":
		return "searching for " + truncate(in.Pattern, 40)
	case "WebFetch":
		return "fetching " + truncate(in.URL, 60)
	case "WebSearch":
		return "searching the web for " + truncate(in.Query, 40)
	case "Task", "Agent":
		return "delegating to a subagent"
	case "TodoWrite":
		return "planning"
	case "mcp__ccc__remember":
		return "remembering " + truncate(in.Key, 40)
	case "mcp__ccc__recall":
		return "recalling " + truncate(in.Query, 40)
	case "mcp__ccc__send_to_bot":
		return "messaging " + truncate(in.Bot, 30)
	case "mcp__ccc__ask_owner":
		return "asking you a question"
	case "mcp__ccc__notify_owner":
		return "notifying you"
	case "mcp__ccc__send_file":
		return "sending a file"
	}
	if strings.HasPrefix(name, "mcp__ccc__") {
		return strings.TrimPrefix(name, "mcp__ccc__")
	}
	return "running " + name
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// ---------------------------------------------------------------------------
// Failure classification (DESIGN §3.4)
// ---------------------------------------------------------------------------

const (
	errAuthStale   = "auth_stale"
	errRateLimited = "rate_limited"
	errTransient   = "transient"
	errSessionLost = "session_lost"
	errFatal       = "fatal"
)

func classifyFailure(text string, exitCode int) string {
	l := strings.ToLower(text)
	switch {
	case isStaleTokenError(text) ||
		strings.Contains(l, "not logged in") ||
		strings.Contains(l, "please run `claude login`") ||
		strings.Contains(l, "please run `grok login`") ||
		strings.Contains(l, "authentication required") ||
		strings.Contains(l, "not authenticated") ||
		strings.Contains(l, "invalid api key") ||
		strings.Contains(l, "oauth token has expired") ||
		strings.Contains(l, "authentication_error"):
		return errAuthStale
	case strings.Contains(l, "usage limit") || strings.Contains(l, "rate limit") ||
		strings.Contains(l, "rate_limit") || strings.Contains(l, "limit reached") ||
		strings.Contains(l, "429"):
		return errRateLimited
	case strings.Contains(l, "no conversation found") || strings.Contains(l, "session not found") ||
		strings.Contains(l, "no such session") || strings.Contains(l, "could not find session"):
		return errSessionLost
	case strings.Contains(l, "econnreset") || strings.Contains(l, "etimedout") ||
		strings.Contains(l, "enotfound") || strings.Contains(l, "socket hang up") ||
		strings.Contains(l, "network") || strings.Contains(l, "fetch failed") ||
		strings.Contains(l, "internal server error") || strings.Contains(l, "502") ||
		strings.Contains(l, "503") || strings.Contains(l, "overloaded"):
		return errTransient
	}
	if exitCode == 0 {
		return errFatal
	}
	return errFatal
}

func failureMessage(class, detail string) string {
	switch class {
	case "stopped":
		return "🛑 Stopped."
	case errAuthStale:
		return "🔑 That account needs a new login and no other account could take the turn.\n<code>" +
			htmlEscape(truncate(detail, 400)) + "</code>"
	case errRateLimited:
		return "⏳ Every account is rate limited right now. Try again later.\n<code>" +
			htmlEscape(truncate(detail, 400)) + "</code>"
	default:
		return "❌ Turn failed.\n<code>" + htmlEscape(truncate(detail, 800)) + "</code>"
	}
}

// ---------------------------------------------------------------------------
// Profiles
// ---------------------------------------------------------------------------

func (r *Runner) markNeedsLogin(p Profile) {
	r.mu.Lock()
	r.needsLogin[p.Name] = true
	r.mu.Unlock()
	cfg := r.config()
	if cfg == nil || cfg.ChatID == 0 || cfg.BotToken == "" {
		return
	}
	// The button runs the PTY login flow in account.go, so the owner never has
	// to reach the machine to fix an account.
	shown := accountDisplay(p)
	msg := fmt.Sprintf("🔑 %s account <b>%s</b> needs a new login (a turn was refused).", engineLabel(profileEngine(p)), htmlEscape(shown))
	buttons := [][]InlineKeyboardButton{{{Text: "🔑 Relogin " + shown, CallbackData: "account:login:" + accountTarget(p.Name)}}}
	_, _ = sendMessageKeyboardGetID(cfg, cfg.ChatID, 0, msg, buttons) // safe-ignore: a failed notification must not fail the turn
}

// pickProfileExcluding is pickAccount for Claude. Kept so existing tests and
// callers that mean "the Claude pool" keep compiling.
func (r *Runner) pickProfileExcluding(exclude map[string]bool) (Profile, bool) {
	return r.pickAccount(engineClaude, exclude)
}

// pickAccount chooses a healthy account whose engine matches. Failover stays
// inside that engine: Claude↔Claude, Grok↔Grok. When no account of that
// engine is registered, the machine-default implicit account is used so a
// bot assigned with `/engine` still has somewhere to run.
func (r *Runner) pickAccount(engine string, exclude map[string]bool) (Profile, bool) {
	engine, err := parseEngine(engine)
	if err != nil {
		engine = engineClaude
	}
	cfg := r.config()
	r.mu.Lock()
	needs := make(map[string]bool, len(r.needsLogin))
	for k, v := range r.needsLogin {
		needs[k] = v
	}
	r.mu.Unlock()

	now := time.Now()
	stats := collectProfileStats(cfg, nil, now)
	open := stats[:0:0]
	for _, s := range stats {
		if s.Engine != engine {
			continue
		}
		if exclude[s.Name] || needs[s.Name] {
			continue
		}
		open = append(open, s)
	}
	if len(open) == 0 {
		// Everything of this engine is excluded. If the only problem is a
		// stale needs_login flag and there is literally nothing else, try it
		// anyway rather than dropping the turn.
		for _, s := range stats {
			if s.Engine == engine && !exclude[s.Name] {
				open = append(open, s)
			}
		}
	}
	if len(open) == 0 {
		if len(listProfilesForEngine(cfg, engine)) == 0 && (exclude == nil || !exclude[engine]) {
			return implicitAccount(engine), true
		}
		return Profile{}, false
	}
	name := chooseProfile(open, now)
	p, ok := profileByName(cfg, name)
	return p, ok
}

// botEnv is claudeEnv plus the instance's env_passthrough list (DESIGN §3.1):
// the only channel by which a secret reaches a bot.
func botEnv(config *Config, p Profile) []string {
	env := claudeEnv(p)
	for _, name := range config.EnvPassthrough {
		name = strings.TrimSpace(name)
		if name == "" || strings.HasPrefix(name, "CLAUDE") || strings.HasPrefix(name, "ANTHROPIC") {
			continue
		}
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}

func instanceModel(config *Config) string {
	if config == nil {
		return ""
	}
	return strings.TrimSpace(config.Model)
}

// mcpConfigJSON is the inline --mcp-config value: one stdio server, this
// binary, scoped to the calling bot and turn (DESIGN §6).
func (r *Runner) mcpConfigJSON(botID, turnID int64) string {
	cfg := r.config()
	spec := map[string]any{
		"mcpServers": map[string]any{
			"ccc": map[string]any{
				"type":    "stdio",
				"command": cccPath,
				"args":    []string{"mcp", "--bot", fmt.Sprint(botID), "--turn", fmt.Sprint(turnID)},
				"env": map[string]string{
					"PATH":       os.Getenv("PATH"),
					"HOME":       os.Getenv("HOME"),
					"CCC_DB":     dbPath(cfg),
					"CCC_CONFIG": getConfigPath(),
				},
			},
		},
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return "{}" // safe-ignore: unreachable for this literal map; an empty config still runs the turn without tools
	}
	return string(b)
}

func hostnameOrUnknown() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}

// newUUID mints a v4 UUID for a session. crypto/rand via os is not needed here
// (a collision only means a resume conflict), but the format must be exact:
// claude validates --session-id.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fall back to time-based bytes; still a syntactically valid UUID.
		n := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(n >> (uint(i%8) * 8))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// setConfig swaps the configuration used by the next turn (/model, /account).
func (r *Runner) setConfig(cfg *Config) {
	r.mu.Lock()
	r.cfg = cfg
	r.mu.Unlock()
}

// needsLoginSnapshot copies the set of profiles a turn found logged out.
func (r *Runner) needsLoginSnapshot() map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]bool, len(r.needsLogin))
	for k, v := range r.needsLogin {
		out[k] = v
	}
	return out
}

// clearNeedsLogin puts a profile back in the running after a successful login.
func (r *Runner) clearNeedsLogin(name string) {
	r.mu.Lock()
	delete(r.needsLogin, name)
	r.mu.Unlock()
}

// markProfileNeedsLogin is markNeedsLogin without the notification, for the
// doctor loop (which does its own, with a Relogin button).
func (r *Runner) markProfileNeedsLogin(name string) {
	r.mu.Lock()
	r.needsLogin[name] = true
	r.mu.Unlock()
}
