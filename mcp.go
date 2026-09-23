package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gorm.io/gorm"
)

// `ccc mcp --bot <id> --turn <id>` is the stdio MCP server Claude Code spawns
// for each turn from the inline --mcp-config (DESIGN §6). It is the same
// binary as the listener and opens the same SQLite file; the bot identity
// comes from the flags, never from the model, so a bot cannot act as another.
//
// Everything a tool receives is data written by the model. It is validated and
// stored; it is never treated as an instruction to ccc.

// mcpServer is the per-turn server state.
type mcpServer struct {
	db     *gorm.DB
	config *Config
	botID  int64
	turnID int64
}

// runMCPServer is the entry point for the `mcp` subcommand.
func runMCPServer(args []string) error {
	var botID, turnID int64
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--bot":
			if i+1 < len(args) {
				botID, _ = strconv.ParseInt(args[i+1], 10, 64) // safe-ignore: a bad id stays 0 and is rejected below
				i++
			}
		case "--turn":
			if i+1 < len(args) {
				turnID, _ = strconv.ParseInt(args[i+1], 10, 64) // safe-ignore: an unknown turn id only loses the question<->turn link
				i++
			}
		}
	}
	if botID == 0 {
		botID, _ = strconv.ParseInt(os.Getenv("CCC_BOT_ID"), 10, 64) // safe-ignore: 0 still rejected below
	}
	if turnID == 0 {
		turnID, _ = strconv.ParseInt(os.Getenv("CCC_TURN_ID"), 10, 64) // safe-ignore: missing turn id only loses the question link
	}
	if botID == 0 {
		return fmt.Errorf("ccc mcp requires --bot <id> (or CCC_BOT_ID)")
	}

	// The listener passes the database and config paths through the MCP env
	// block; falling back to the defaults keeps manual invocation working.
	config, err := loadConfig()
	if err != nil {
		config = &Config{}
	}
	path := os.Getenv("CCC_DB")
	if path == "" {
		path = dbPath(config)
	}
	db, err := openStore(path)
	if err != nil {
		return err
	}
	s := &mcpServer{db: db, config: config, botID: botID, turnID: turnID}

	server := mcp.NewServer(&mcp.Implementation{Name: "ccc", Version: version}, nil)
	s.register(server)
	mcpProcess = true
	return server.Run(context.Background(), &mcp.StdioTransport{})
}

// mcpProcess is true only inside `ccc mcp` (listen's per-turn subprocess).
// archive_bot tests construct mcpServer directly; they must not SIGTERM the
// test process group.
var mcpProcess bool

// text is the standard single-text-block tool result.
func text(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}}}
}

func toolErr(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}},
	}
}

// ---------------------------------------------------------------------------
// Tool inputs
// ---------------------------------------------------------------------------

type rememberIn struct {
	Scope       string `json:"scope" jsonschema:"where the memory belongs: user (about the owner, shared across sessions), project (about one code base) or bot (private to this session)"`
	Key         string `json:"key" jsonschema:"short stable identifier, e.g. deploy-target or prefers-spanish"`
	Text        string `json:"text" jsonschema:"the fact to remember, one or two sentences"`
	ProjectPath string `json:"project_path,omitempty" jsonschema:"absolute path of the project, required for scope=project"`
}

type recallIn struct {
	Query string `json:"query" jsonschema:"words to search for; empty lists the most recent memories"`
	Scope string `json:"scope,omitempty" jsonschema:"restrict to user, project or bot"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum results (default 10)"`
}

type forgetIn struct {
	Scope       string `json:"scope" jsonschema:"user, project or bot"`
	Key         string `json:"key" jsonschema:"the key to delete"`
	ProjectPath string `json:"project_path,omitempty" jsonschema:"absolute path, required for scope=project"`
}

type emptyIn struct{}

type sendToBotIn struct {
	Bot  string `json:"bot" jsonschema:"name of the target session"`
	Text string `json:"text" jsonschema:"the message"`
	Wake *bool  `json:"wake,omitempty" jsonschema:"run the target session now instead of waiting for its next turn (default true)"`
}

type notifyOwnerIn struct {
	Text    string `json:"text" jsonschema:"what to tell the owner"`
	Urgency string `json:"urgency,omitempty" jsonschema:"normal (default) or urgent; urgent also sends a direct message"`
}

type askOwnerIn struct {
	Question string   `json:"question" jsonschema:"the question, one sentence"`
	Options  []string `json:"options,omitempty" jsonschema:"optional listed choices shown in the Telegram message; put the recommended answer first. Omit when the answer cannot be a short pick"`
}

type updateInstructionsIn struct {
	Role string `json:"role" jsonschema:"ignored; role is no longer a product concept"`
}

type setNameIn struct {
	Name string `json:"name" jsonschema:"the new title of this session"`
}

type sendFileIn struct {
	Path    string `json:"path" jsonschema:"absolute path of the file to send"`
	Caption string `json:"caption,omitempty" jsonschema:"optional caption"`
}

type spawnSessionIn struct {
	Prompt string `json:"prompt" jsonschema:"first message the new session should run"`
	Name   string `json:"name,omitempty" jsonschema:"session name; default is the first line of prompt"`
}

type tellSessionIn struct {
	Session string `json:"session" jsonschema:"name of the live session to message"`
	Text    string `json:"text" jsonschema:"the message"`
}

type reportToGeneralIn struct {
	Text string `json:"text" jsonschema:"status the dispatcher should see; not posted to the owner"`
}

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

func (s *mcpServer) register(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "remember",
		Description: "Store a durable fact so later turns in this session (and other sessions, for user/project scope) still know it.",
	}, s.remember)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "recall",
		Description: "Search the memories you can see: all user memories, all project memories and this session's own.",
	}, s.recall)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "forget",
		Description: "Delete one memory by scope and key.",
	}, s.forget)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "notify_owner",
		Description: "Tell the owner something worth an interruption. Do not dump a session report or transcript. Urgent also sends a direct message.",
	}, s.notifyOwner)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "ask_owner",
		Description: "General only: ask the owner a question via Telegram and END YOUR TURN. Pass optional listed options, recommended first. The owner answers by replying to the question in the DM. Workers must not call this. A worker call does not wait: it archives the session and hands the question to General, which decides and starts a new session. Workers that need a decision use report_to_general (question, options, resume state) and then archive_bot.",
	}, s.askOwner)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "set_name",
		Description: "Rename this session. Keep it short and unique. This starts a fresh conversation on your next message.",
	}, s.setName)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "send_file",
		Description: "Send a file from this machine to the owner via Telegram (max 50 MB).",
	}, s.sendFile)
	s.registerAutomation(server)
	s.registerSecrets(server)
	s.registerCrew(server)
}

func (s *mcpServer) isChief() bool {
	b, err := s.bot()
	return err == nil && isGeneralBot(b)
}

func (s *mcpServer) registerCrew(server *mcp.Server) {
	if s.isChief() {
		mcp.AddTool(server, &mcp.Tool{
			Name:        "list_sessions",
			Description: "List live sessions (not including General): name, status, engine, last output.",
		}, s.listSessions)
		mcp.AddTool(server, &mcp.Tool{
			Name:        "spawn_session",
			Description: "Start a backend worker session (no Telegram topic) and give it a first prompt. The session starts when this turn ends. ccc assigns it to the account/engine with the most usage headroom. Use this instead of doing long work yourself. Reports come back here.",
		}, s.spawnSession)
		mcp.AddTool(server, &mcp.Tool{
			Name:        "tell_session",
			Description: "Message an existing live session and wake it. Sessions cannot message each other; only General can tell them.",
		}, s.tellSession)
		return
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "report_to_general",
		Description: "Send a status update to General (the dispatcher). It posts the owner-facing result; do not dump a transcript. For owner-facing results write a readable digest (short sections + bullets), not one paragraph. Use it for finished work, a blocker, or a question for the dispatcher. You cannot message other sessions.",
	}, s.reportToGeneral)
}

// ---------------------------------------------------------------------------
// Tool implementations
// ---------------------------------------------------------------------------

func (s *mcpServer) bot() (*Bot, error) { return botByID(s.db, s.botID) }

func (s *mcpServer) remember(_ context.Context, _ *mcp.CallToolRequest, in rememberIn) (*mcp.CallToolResult, any, error) {
	key := strings.TrimSpace(in.Key)
	body := strings.TrimSpace(in.Text)
	if key == "" || body == "" {
		return toolErr("remember needs a non-empty key and text"), nil, nil
	}
	if len(key) > 120 {
		return toolErr("key is too long (max 120 characters)"), nil, nil
	}
	if len(body) > 4000 {
		body = body[:4000]
	}
	scope, scopeKey, err := memoryScopeKey(strings.TrimSpace(in.Scope), s.botID, in.ProjectPath)
	if err != nil {
		return toolErr("%v", err), nil, nil
	}
	if err := upsertMemory(s.db, scope, scopeKey, key, body, s.botID); err != nil {
		return toolErr("could not store the memory: %v", err), nil, nil
	}
	return text("remembered %q in scope %s", key, scope), nil, nil
}

func (s *mcpServer) recall(_ context.Context, _ *mcp.CallToolRequest, in recallIn) (*mcp.CallToolResult, any, error) {
	scope := strings.TrimSpace(in.Scope)
	if scope != "" && scope != scopeUser && scope != scopeProject && scope != scopeBot {
		return toolErr("unknown scope %q (use user, project or bot)", scope), nil, nil
	}
	mems, err := searchMemories(s.db, s.botID, in.Query, scope, in.Limit)
	if err != nil {
		return toolErr("search failed: %v", err), nil, nil
	}
	if len(mems) == 0 {
		return text("no memories matched"), nil, nil
	}
	var sb strings.Builder
	for _, m := range mems {
		where := m.Scope
		if m.Scope == scopeProject {
			where = "project " + m.ScopeKey
		}
		fmt.Fprintf(&sb, "[%s] %s: %s\n", where, m.Key, m.Text)
	}
	return text("%s", strings.TrimRight(sb.String(), "\n")), nil, nil
}

func (s *mcpServer) forget(_ context.Context, _ *mcp.CallToolRequest, in forgetIn) (*mcp.CallToolResult, any, error) {
	scope, scopeKey, err := memoryScopeKey(strings.TrimSpace(in.Scope), s.botID, in.ProjectPath)
	if err != nil {
		return toolErr("%v", err), nil, nil
	}
	key := strings.TrimSpace(in.Key)
	res := s.db.Where("scope = ? AND scope_key = ? AND key = ?", scope, scopeKey, key).Delete(&Memory{})
	if res.Error != nil {
		return toolErr("could not delete: %v", res.Error), nil, nil
	}
	if res.RowsAffected == 0 {
		return text("no memory %q in scope %s", key, scope), nil, nil
	}
	return text("forgot %q", key), nil, nil
}

func (s *mcpServer) listBots(_ context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
	bots, err := liveBots(s.db)
	if err != nil {
		return toolErr("could not list bots: %v", err), nil, nil
	}
	var sb strings.Builder
	for i := range bots {
		b := &bots[i]
		marker := ""
		if b.ID == s.botID {
			marker = " (you)"
		}
		role := strings.TrimSpace(b.Role)
		if role == "" {
			role = "(no role set)"
		}
		fmt.Fprintf(&sb, "%s%s [%s] — %s\n", b.Name, marker, b.Status, role)
	}
	if sb.Len() == 0 {
		return text("no bots"), nil, nil
	}
	return text("%s", strings.TrimRight(sb.String(), "\n")), nil, nil
}

func (s *mcpServer) sendToBot(_ context.Context, _ *mcp.CallToolRequest, in sendToBotIn) (*mcp.CallToolResult, any, error) {
	body := strings.TrimSpace(in.Text)
	if body == "" {
		return toolErr("send_to_bot needs text"), nil, nil
	}
	target, err := botByName(s.db, strings.TrimSpace(in.Bot))
	if err != nil {
		return toolErr("no live bot named %q (use list_bots)", in.Bot), nil, nil
	}
	if target.ID == s.botID {
		return toolErr("you cannot send a message to yourself"), nil, nil
	}
	wake := true
	if in.Wake != nil {
		wake = *in.Wake
	}
	self, err := s.bot()
	if err != nil {
		return toolErr("unknown bot"), nil, nil
	}
	if _, _, err := queueBotMessage(s.db, self, target.Name, body, wake); err != nil {
		return toolErr("%s", err.Error()), nil, nil
	}
	return text("message queued for %s", target.Name), nil, nil
}

func (s *mcpServer) listSessions(_ context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
	if !s.isChief() {
		return toolErr("only General can list sessions"), nil, nil
	}
	return text("%s", formatSessionRoster(sessionRoster(s.db, s.botID))), nil, nil
}

func (s *mcpServer) spawnSession(_ context.Context, _ *mcp.CallToolRequest, in spawnSessionIn) (*mcp.CallToolResult, any, error) {
	if !s.isChief() {
		return toolErr("only General can spawn sessions"), nil, nil
	}
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		return toolErr("spawn_session needs a prompt"), nil, nil
	}
	self, err := s.bot()
	if err != nil {
		return toolErr("unknown bot"), nil, nil
	}
	b, err := startBackendSession(s.db, s.config, self, strings.TrimSpace(in.Name), prompt)
	if err != nil {
		return toolErr("could not start the session: %v", err), nil, nil
	}
	return text("started session %q; it will run when this turn ends. Reports come back here; tell_session to message it.", b.Name), nil, nil
}

func (s *mcpServer) tellSession(_ context.Context, _ *mcp.CallToolRequest, in tellSessionIn) (*mcp.CallToolResult, any, error) {
	if !s.isChief() {
		return toolErr("only General can message sessions"), nil, nil
	}
	body := strings.TrimSpace(in.Text)
	if body == "" {
		return toolErr("tell_session needs text"), nil, nil
	}
	target, err := botByName(s.db, strings.TrimSpace(in.Session))
	if err != nil {
		return toolErr("no live session named %q", in.Session), nil, nil
	}
	if isGeneralBot(target) || target.ID == s.botID {
		return toolErr("tell_session is for worker sessions, not General"), nil, nil
	}
	self, err := s.bot()
	if err != nil {
		return toolErr("unknown bot"), nil, nil
	}
	if _, _, err := queueBotMessage(s.db, self, target.Name, body, true); err != nil {
		return toolErr("%s", err.Error()), nil, nil
	}
	return text("queued for %s; it will run when this turn ends", target.Name), nil, nil
}

func (s *mcpServer) reportToGeneral(_ context.Context, _ *mcp.CallToolRequest, in reportToGeneralIn) (*mcp.CallToolResult, any, error) {
	if s.isChief() {
		return toolErr("you are General; talk to the owner here"), nil, nil
	}
	body := strings.TrimSpace(in.Text)
	if body == "" {
		return toolErr("report_to_general needs text"), nil, nil
	}
	if _, err := generalBot(s.db); err != nil {
		return toolErr("General dispatcher is not running"), nil, nil
	}
	self, err := s.bot()
	if err != nil {
		return toolErr("unknown bot"), nil, nil
	}
	if _, _, err := queueOwnerRelay(s.db, self, body); err != nil {
		return toolErr("%s", err.Error()), nil, nil
	}
	return text("reported to General; it will run when this turn ends"), nil, nil
}

func (s *mcpServer) notifyOwner(_ context.Context, _ *mcp.CallToolRequest, in notifyOwnerIn) (*mcp.CallToolResult, any, error) {
	body := strings.TrimSpace(in.Text)
	if body == "" {
		return toolErr("notify_owner needs text"), nil, nil
	}
	b, err := s.bot()
	if err != nil {
		return toolErr("unknown bot"), nil, nil
	}
	prefix := "🔔"
	if strings.EqualFold(in.Urgency, "urgent") {
		prefix = "🚨"
	}
	label := ""
	if !isGeneralBot(b) {
		label = fmt.Sprintf(" <b>%s</b>:", htmlEscape(b.Name))
	}
	msg := fmt.Sprintf("%s%s %s", prefix, label, renderTelegramHTML(truncate(body, 3000)))
	s.post(0, msg)
	return text("owner notified"), nil, nil
}

// maxQuestionOptions caps listed choices in an ask_owner Telegram message.
const maxQuestionOptions = 4

// skipOptionLabel is the existing text convention that skips without choosing.
// Replying Omitir or SKIP to a question unblocks the worker; it is not a button.
const skipOptionLabel = "Omitir"

func isSkipOption(s string) bool {
	s = strings.TrimSpace(s)
	return strings.EqualFold(s, skipOptionLabel) || strings.EqualFold(s, "Skip")
}

func (s *mcpServer) askOwner(_ context.Context, _ *mcp.CallToolRequest, in askOwnerIn) (*mcp.CallToolResult, any, error) {
	q := strings.TrimSpace(in.Question)
	if q == "" {
		return toolErr("ask_owner needs a question"), nil, nil
	}
	if len(in.Options) > maxQuestionOptions {
		return toolErr("at most %d options are allowed", maxQuestionOptions), nil, nil
	}
	b, err := s.bot()
	if err != nil {
		return toolErr("unknown bot"), nil, nil
	}
	opts := make([]string, 0, len(in.Options))
	for _, o := range in.Options {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		opts = append(opts, truncate(o, 60))
	}
	// Workers are unattended. Asking the owner used to park the session in
	// waiting until a reply; that blocked the work. Hand the question to
	// General and end the session instead. General asks the owner if it
	// cannot decide, then starts a new session that continues.
	if !isGeneralBot(b) {
		return s.handOffWorkerDecision(b, q, opts)
	}
	optionsJSON, err := json.Marshal(opts)
	if err != nil {
		return toolErr("invalid options"), nil, nil
	}
	row := Question{BotID: b.ID, Question: truncate(q, 2000), OptionsJSON: string(optionsJSON)}
	if s.turnID != 0 {
		row.TurnID = &s.turnID
	}
	if err := s.db.Create(&row).Error; err != nil {
		return toolErr("could not record the question: %v", err), nil, nil
	}
	asked := q
	if !isGeneralBot(b) {
		asked = fmt.Sprintf("[%s] %s", b.Name, q)
	}
	msgID := s.postQuestion(0, asked, opts)
	if msgID != 0 {
		s.db.Model(&Question{}).Where("id = ?", row.ID).Update("asked_message_id", msgID)
	}
	return text(`{"status":"asked","question_id":%d} — end your turn now; the answer arrives as your next message`, row.ID), nil, nil
}

// handOffWorkerDecision ends a worker that tried to ask the owner. The
// question goes to General (relay). This session is archived so it cannot
// sit in waiting. No Question row: nothing stays parked on a reply.
func (s *mcpServer) handOffWorkerDecision(b *Bot, question string, opts []string) (*mcp.CallToolResult, any, error) {
	if _, err := ensureGeneralBotRow(s.db, s.config); err != nil {
		return toolErr("General dispatcher is not running"), nil, nil
	}
	body := workerDecisionHandoff(b, question, opts, lastTurnOutput(s.db, b.ID))
	if _, _, err := queueOwnerRelay(s.db, b, body); err != nil {
		return toolErr("%s", err.Error()), nil, nil
	}
	if err := archiveBotRow(s.db, b.ID); err != nil {
		return toolErr("could not end the session: %v", err), nil, nil
	}
	s.post(0, "📦 Session <b>"+htmlEscape(b.Name)+"</b> ended. The decision went to General.")
	if mcpProcess {
		go terminateSelfProcessGroup()
	}
	return text("handed to General and archived this session. Stop now. Do not call more tools. A new session continues after General decides."), nil, nil
}

// workerDecisionHandoff is the inbox General gets when a worker stops for a
// decision. It is an instruction to the dispatcher, not a digest for the owner.
func workerDecisionHandoff(b *Bot, question string, options []string, last string) string {
	var sb strings.Builder
	name := "session"
	if b != nil && strings.TrimSpace(b.Name) != "" {
		name = b.Name
	}
	fmt.Fprintf(&sb, "Session %q stopped. Workers do not wait on the owner.\n", name)
	if b != nil && strings.TrimSpace(b.Cwd) != "" {
		fmt.Fprintf(&sb, "cwd: %s\n", b.Cwd)
	}
	if q := strings.TrimSpace(question); q != "" {
		fmt.Fprintf(&sb, "Question: %s\n", q)
	}
	if len(options) > 0 {
		sb.WriteString("Options (recommended first):\n")
		for i, o := range options {
			fmt.Fprintf(&sb, "%d. %s\n", i+1, o)
		}
	}
	if last = strings.TrimSpace(last); last != "" {
		fmt.Fprintf(&sb, "\nLast output:\n%s\n", truncate(last, 1500))
	}
	sb.WriteString("\nThis session is archived. Decide and spawn_session a new one that continues from this cwd with the decision. ")
	sb.WriteString("If you need the owner, ask_owner yourself, then spawn_session on the answer. ")
	sb.WriteString("Do not tell this session. Do not leave the work parked.\n")
	return sb.String()
}

func parkedWorkerHandoff(b *Bot, qs []Question, last string) string {
	if len(qs) == 0 {
		return workerDecisionHandoff(b, "stuck waiting with no question recorded", nil, last)
	}
	if len(qs) == 1 {
		return workerDecisionHandoff(b, qs[0].Question, questionOptions(&qs[0]), last)
	}
	var sb strings.Builder
	for i := range qs {
		if i > 0 {
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "Question: %s\n", qs[i].Question)
		for j, o := range questionOptions(&qs[i]) {
			fmt.Fprintf(&sb, "%d. %s\n", j+1, o)
		}
	}
	return workerDecisionHandoff(b, strings.TrimRight(sb.String(), "\n"), nil, last)
}

func lastTurnOutput(db *gorm.DB, botID int64) string {
	if db == nil || botID == 0 {
		return ""
	}
	var t Turn
	if err := db.Where("bot_id = ?", botID).Order("id desc").First(&t).Error; err != nil {
		return ""
	}
	return t.Output
}

// releaseParkedWorkers archives workers that are sitting on an owner question
// (or stuck in waiting) and wakes General with the handoff. Runs once at
// listen boot so a session parked by the old ask_owner path does not stay
// blocked after upgrade. General's own questions are left open.
func (in *instance) releaseParkedWorkers() int {
	if in == nil || in.db == nil {
		return 0
	}
	var bots []Bot
	if err := in.db.Where("archived_at IS NULL AND topic_id <> 0 AND status <> ?", botRunning).Find(&bots).Error; err != nil {
		return 0
	}
	n := 0
	for i := range bots {
		b := &bots[i]
		if isGeneralBot(b) {
			continue
		}
		var qs []Question
		if err := in.db.Where("bot_id = ? AND answered_at IS NULL", b.ID).Order("id").Find(&qs).Error; err != nil {
			continue
		}
		if len(qs) == 0 && b.Status != botWaiting {
			continue
		}
		if _, err := ensureGeneralBotRow(in.db, in.config()); err != nil {
			hookLog("parked handoff: %v", err)
			return n
		}
		body := parkedWorkerHandoff(b, qs, lastTurnOutput(in.db, b.ID))
		if _, _, err := queueOwnerRelay(in.db, b, body); err != nil {
			hookLog("parked handoff %s: %v", b.Name, err)
			continue
		}
		now := time.Now()
		for j := range qs {
			q := qs[j]
			in.db.Model(&Question{}).Where("id = ? AND answered_at IS NULL", q.ID).
				Updates(map[string]any{"answer": "handed to General", "answered_at": now})
			in.noteQuestionHandedOff(&q)
		}
		if err := archiveBotRow(in.db, b.ID); err != nil {
			hookLog("archive parked %s: %v", b.Name, err)
			continue
		}
		n++
	}
	if n > 0 {
		if r, ok := in.runner.(*Runner); ok {
			r.deliverInbox(0)
		}
	}
	return n
}

func (in *instance) noteQuestionHandedOff(q *Question) {
	if in == nil || q == nil || q.AskedMessageID == 0 {
		return
	}
	cfg := in.config()
	chat, thread, ok := destForTopic(cfg, 0)
	if !ok {
		return
	}
	body := "❓ " + renderTelegramHTML(q.Question) + "\n<i>This session stopped waiting. General has it.</i>"
	_ = editMessageHTML(cfg, chat, q.AskedMessageID, thread, body) // safe-ignore: the handoff already woke General
}

func (s *mcpServer) updateInstructions(_ context.Context, _ *mcp.CallToolRequest, _ updateInstructionsIn) (*mcp.CallToolResult, any, error) {
	return toolErr("roles are gone; this session has no job description to update"), nil, nil
}

func (s *mcpServer) setName(_ context.Context, _ *mcp.CallToolRequest, in setNameIn) (*mcp.CallToolResult, any, error) {
	b, err := s.bot()
	if err != nil {
		return toolErr("unknown bot"), nil, nil
	}
	if isGeneralBot(b) {
		return toolErr("General stays General"), nil, nil
	}
	name, err := validateBotName(s.db, b.ID, in.Name)
	if err != nil {
		return toolErr("%v", err), nil, nil
	}
	old := b.Name
	if name != old {
		if err := renameBot(s.db, s.config, b, name); err != nil {
			return toolErr("could not rename: %v", err), nil, nil
		}
	}
	out := fmt.Sprintf("renamed to %q; your next message starts a fresh conversation with it", name)
	if name == old {
		out = fmt.Sprintf("you were already called %q", name)
	}
	return text("%s", out), nil, nil
}

// sendFileMaxBytes is Telegram's bot upload limit.
const sendFileMaxBytes = 50 * 1024 * 1024

func (s *mcpServer) sendFile(_ context.Context, _ *mcp.CallToolRequest, in sendFileIn) (*mcp.CallToolResult, any, error) {
	path := expandPath(strings.TrimSpace(in.Path))
	if path == "" {
		return toolErr("send_file needs a path"), nil, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return toolErr("bad path: %v", err), nil, nil
	}
	if reason, ok := sendFileForbidden(s.config, abs); ok {
		return toolErr("refusing to send that file: %s", reason), nil, nil
	}
	info, err := os.Stat(abs)
	if err != nil {
		return toolErr("cannot read %s: %v", abs, err), nil, nil
	}
	if info.IsDir() {
		return toolErr("%s is a directory", abs), nil, nil
	}
	if info.Size() > sendFileMaxBytes {
		return toolErr("%s is %d bytes, over the 50 MB limit", abs, info.Size()), nil, nil
	}
	if _, err := s.bot(); err != nil {
		return toolErr("unknown bot"), nil, nil
	}
	chat, thread, ok := destForTopic(s.config, 0)
	if !ok {
		return toolErr("no Telegram destination (set chat_id)"), nil, nil
	}
	if err := sendFile(s.config, chat, thread, abs, in.Caption); err != nil {
		return toolErr("Telegram send failed: %v", err), nil, nil
	}
	return text("sent %s", filepath.Base(abs)), nil, nil
}

// sendFileForbidden implements the DESIGN §12 rule: credentials never leave the
// machine through send_file. Anything inside an engine home (Claude, Codex,
// Grok, Gemini), inside <data_dir>/profiles, or with a credential-ish name
// (including auth.json) is refused. ~/.codex/auth.json is the hole this
// originally missed: the name list had .credentials.json but not auth.json,
// and the directory list covered ~/.claude but not ~/.codex / ~/.grok.
func sendFileForbidden(config *Config, abs string) (string, bool) {
	abs = filepath.Clean(abs)
	lower := strings.ToLower(filepath.Base(abs))
	for _, bad := range credentialFileNames {
		if lower == bad {
			return "it looks like a credentials file", true
		}
	}
	home, _ := os.UserHomeDir()
	home = filepath.Clean(home)
	guarded := []string{filepath.Join(dataDir(config), "profiles")}
	if home != "" && home != "." {
		guarded = append(guarded,
			filepath.Join(home, ".ssh"),
			filepath.Join(home, ".aws"),
			filepath.Join(home, ".claude"),
			filepath.Join(home, ".codex"),
			filepath.Join(home, ".grok"),
			filepath.Join(home, ".gemini"),
			configDir(),
		)
	}
	for _, p := range listProfiles(config) {
		guarded = append(guarded, claudeHome(p))
		if h := engineHome(p); h != "" && filepath.Clean(h) != home {
			// Never refuse the entire user home (implicit Antigravity HOME).
			guarded = append(guarded, h)
		}
	}
	for _, g := range guarded {
		if pathInside(abs, g) {
			return "it is inside a credentials/config directory", true
		}
	}
	return "", false
}

// credentialFileNames are basenames send_file will not ship, wherever they
// sit. auth.json is Grok/Codex OAuth; mcp_credentials.json is Grok MCP OAuth.
var credentialFileNames = []string{
	".credentials.json", "credentials.json", ".claude.json",
	"auth.json", ".auth.json", "mcp_credentials.json",
	"antigravity-oauth-token",
	"id_rsa", "id_ed25519", ".env",
}

func pathInside(abs, dir string) bool {
	if dir == "" {
		return false
	}
	abs = filepath.Clean(abs)
	dir = filepath.Clean(dir)
	if abs == dir {
		return true
	}
	sep := string(filepath.Separator)
	return strings.HasPrefix(abs, strings.TrimRight(dir, sep)+sep)
}

// ---------------------------------------------------------------------------
// Telegram side of the MCP process
// ---------------------------------------------------------------------------

// post writes into a Telegram destination, or does nothing when this instance
// has nowhere to send (which is how the runner is exercised in tests).
func (s *mcpServer) post(topicID int64, html string) {
	chat, thread, ok := destForTopic(s.config, topicID)
	if !ok {
		return
	}
	_, _ = sendMessageHTMLGetID(s.config, chat, thread, html) // safe-ignore: a failed mirror must not fail the tool call
}

// postQuestion renders an ask_owner question as text (options listed in the
// body) and returns the message id so a reply-to can be matched back to it.
func (s *mcpServer) postQuestion(topicID int64, question string, options []string) int64 {
	chat, thread, ok := destForTopic(s.config, topicID)
	if !ok {
		return 0
	}
	body := "❓ " + renderTelegramHTML(question)
	n := 0
	for _, o := range options {
		if strings.TrimSpace(o) == "" {
			continue
		}
		n++
		body += fmt.Sprintf("\n%d. %s", n, htmlEscape(o))
	}
	body += "\n<i>Reply to this message with your answer.</i>"
	id, _ := sendMessageHTMLGetID(s.config, chat, thread, body) // safe-ignore: a question with no message id can still be answered by reply
	return id
}

// pendingQuestion returns the bot's oldest unanswered question, if any.
func pendingQuestion(db *gorm.DB, botID int64) (*Question, bool) {
	var q Question
	if err := db.Where("bot_id = ? AND answered_at IS NULL", botID).Order("id").First(&q).Error; err != nil {
		return nil, false
	}
	return &q, true
}

// normalizeQuestionText is the similarity key: case-fold, collapse space, drop
// trailing ?¿!. Two pending asks match when this is equal and non-empty.
func normalizeQuestionText(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimRight(s, "?¿!.")
}

func questionsSimilar(a, b *Question) bool {
	if a == nil || b == nil || a.ID == b.ID {
		return false
	}
	na := normalizeQuestionText(a.Question)
	nb := normalizeQuestionText(b.Question)
	return na != "" && na == nb
}

// mapQuestionAnswer returns the answer to store on q. Empty-option questions
// take the text as-is. Questions with options only accept a choice they already
// have (case-insensitive), so answering "Yes" does not invent a choice on a
// sibling that asked with different options.
func mapQuestionAnswer(q *Question, answer string) (string, bool) {
	answer = strings.TrimSpace(answer)
	if answer == "" || q == nil {
		return "", false
	}
	opts := questionOptions(q)
	if len(opts) == 0 {
		return answer, true
	}
	if isSkipOption(answer) {
		for _, o := range opts {
			if isSkipOption(o) {
				return o, true
			}
		}
		return skipOptionLabel, true
	}
	for _, o := range opts {
		if strings.EqualFold(strings.TrimSpace(o), answer) {
			return o, true
		}
	}
	return "", false
}

// applyQuestionAnswer records the answer on q, then the same answer on every
// similar pending ask whose options can take it. Each resolved row is returned
// so the caller can tick Telegram. The primary error is
// the only hard failure; siblings are best-effort.
func applyQuestionAnswer(db *gorm.DB, runner turnRunner, q *Question, answer string) ([]Question, error) {
	if err := resolveQuestionAnswer(db, runner, q, answer); err != nil {
		return nil, err
	}
	out := []Question{*q}
	var rows []Question
	if err := db.Where("answered_at IS NULL AND id != ?", q.ID).Order("id").Find(&rows).Error; err != nil {
		return out, nil
	}
	for i := range rows {
		other := rows[i]
		if !questionsSimilar(q, &other) {
			continue
		}
		mapped, ok := mapQuestionAnswer(&other, answer)
		if !ok {
			continue
		}
		b, err := botByID(db, other.BotID)
		if err != nil || b.ArchivedAt != nil {
			continue
		}
		if err := resolveQuestionAnswer(db, runner, &other, mapped); err != nil {
			hookLog("similar ask %d: %v", other.ID, err)
			continue
		}
		out = append(out, other)
	}
	return out, nil
}

// answerQuestion records an answer and returns the text to feed back into the
// bot as its next input (DESIGN §6 ask_owner).
func answerQuestion(db *gorm.DB, q *Question, answer string) string {
	now := time.Now()
	db.Model(&Question{}).Where("id = ?", q.ID).
		Updates(map[string]any{"answer": answer, "answered_at": now})
	if isSkipOption(answer) {
		return fmt.Sprintf("The owner skipped the question %q without choosing. Continue without that decision; do not re-ask the same question.", q.Question)
	}
	return fmt.Sprintf("Answer to %q: %s", q.Question, answer)
}

// resolveQuestionAnswer records the answer, clears waiting, and enqueues the
// follow-up turn. Shared by Telegram replies.
func resolveQuestionAnswer(db *gorm.DB, runner turnRunner, q *Question, answer string) error {
	if q == nil {
		return fmt.Errorf("unknown question")
	}
	if q.AnsweredAt != nil {
		return fmt.Errorf("already answered")
	}
	text := answerQuestion(db, q, answer)
	q.Answer = answer
	now := time.Now()
	q.AnsweredAt = &now
	setBotStatus(db, q.BotID, botIdle)
	if runner == nil {
		return fmt.Errorf("runner not running")
	}
	_, err := runner.Enqueue(q.BotID, sourceUser, text, 0)
	return err
}

// questionOptions decodes the stored options list.
func questionOptions(q *Question) []string {
	var opts []string
	if q.OptionsJSON == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(q.OptionsJSON), &opts); err != nil {
		return nil
	}
	return opts
}

// ---------------------------------------------------------------------------
// Automation tools (DESIGN §6/§7): watches, schedules, background jobs, projects
// ---------------------------------------------------------------------------

type watchIn struct {
	Name      string `json:"name" jsonschema:"short name for this watch, e.g. ci or inbox"`
	Command   string `json:"command" jsonschema:"shell command run in your working directory; its output is compared run to run"`
	IntervalS int    `json:"interval_s" jsonschema:"how often to run it, in seconds (minimum 60)"`
}

type unwatchIn struct {
	Name string `json:"name" jsonschema:"the watch to remove"`
}

type scheduleIn struct {
	InSeconds int    `json:"in_seconds,omitempty" jsonschema:"wake me up this many seconds from now"`
	At        string `json:"at,omitempty" jsonschema:"wake me up at this RFC3339 time instead"`
	Note      string `json:"note" jsonschema:"what to do when you wake up"`
	Cron      string `json:"cron,omitempty" jsonschema:"repeat on this cron expression (5 fields, or @daily/@hourly)"`
}

type cancelScheduleIn struct {
	ID int64 `json:"id" jsonschema:"the schedule id from schedule_wakeup or list output"`
}

type setRoutineIn struct {
	Name     string `json:"name" jsonschema:"short stable id, e.g. morning-ventas or weekly-review"`
	Prompt   string `json:"prompt" jsonschema:"what to do each time it fires; this becomes the turn input"`
	Cron     string `json:"cron" jsonschema:"5-field cron or @daily/@hourly/@every 1h"`
	Timezone string `json:"timezone,omitempty" jsonschema:"IANA timezone (default Europe/Madrid). Always set this; the work VM is UTC."`
}

type cancelRoutineIn struct {
	Name string `json:"name" jsonschema:"the routine to remove"`
}

type runBackgroundIn struct {
	Name        string            `json:"name,omitempty" jsonschema:"short summary shown in list_background, e.g. go test or npm install"`
	Command     string            `json:"command" jsonschema:"shell command run in your working directory with env_passthrough; returns a job id immediately and does not block this turn"`
	Env         map[string]string `json:"env,omitempty" jsonschema:"optional vault inject: each key is an env var name, each value is a secret name (not the secret itself). Never put a secret in argv."`
	StdinSecret string            `json:"stdin_secret,omitempty" jsonschema:"optional vault secret name whose value is written to the child's stdin"`
}

type runIn struct {
	Command     string            `json:"command" jsonschema:"shell command. Secret values are set on the child env/stdin, never argv."`
	Env         map[string]string `json:"env,omitempty" jsonschema:"env var name → vault secret name (not the secret itself)"`
	StdinSecret string            `json:"stdin_secret,omitempty" jsonschema:"vault secret name whose value is written to the child's stdin"`
}

type secretNameIn struct {
	Name string `json:"name" jsonschema:"the vault secret name"`
}

type backgroundIDIn struct {
	ID int64 `json:"id" jsonschema:"the job id from run_background or list_background"`
}

type archiveBotIn struct {
	Bot string `json:"bot,omitempty" jsonschema:"name of the bot to archive (default: yourself)"`
}

type getProjectIn struct {
	Path string `json:"path" jsonschema:"absolute path of the project"`
}

type setProjectIn struct {
	Path        string `json:"path" jsonschema:"absolute path of the project"`
	Name        string `json:"name,omitempty" jsonschema:"short name"`
	Description string `json:"description,omitempty" jsonschema:"what it is"`
	Stack       string `json:"stack,omitempty" jsonschema:"languages, frameworks, database"`
	DeployNotes string `json:"deploy_notes,omitempty" jsonschema:"how it is deployed"`
}

// registerAutomation adds the Phase 2b tools. Split from register() only to
// keep each function readable; every bot gets all of them.
func (s *mcpServer) registerAutomation(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "watch",
		Description: "Poll a command on an interval and wake you ONLY when its output changes. Costs nothing while nothing changes. Use this instead of schedule_wakeup for CI, PR state, or any poll. Lasts 4 hours, then it is cancelled and you are woken to re-set it. For standing jobs use set_routine.",
	}, s.watch)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "unwatch",
		Description: "Remove one of your watches by name.",
	}, s.unwatch)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_watches",
		Description: "List your watches and when they last ran.",
	}, s.listWatchesTool)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "schedule_wakeup",
		Description: "Wake this session at a time (in_seconds, at, or a one-off cron). Each fire is a full turn, even if nothing changed. For polling a command use watch. For standing recurring work use set_routine.",
	}, s.scheduleWakeup)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "cancel_schedule",
		Description: "Cancel one of your pending wakeups.",
	}, s.cancelSchedule)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "set_routine",
		Description: "Create or replace a named recurring routine. Each fire starts a fresh isolated worker with a short prompt (not a turn on General). Always fires, timezone-aware, ⏰ in General. Upserts by name.",
	}, s.setRoutine)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_routines",
		Description: "List your named routines and when they fire next.",
	}, s.listRoutinesTool)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "cancel_routine",
		Description: "Remove one of your named routines.",
	}, s.cancelRoutineTool)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "run_background",
		Description: "Start a long-running shell command without blocking this turn. Use it when Bash (or any tool) is expected to take more than about 60 seconds: builds, installs, waits. You are woken with source=background when it finishes.",
	}, s.runBackground)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_background",
		Description: "List your recent and active background jobs with status and a short summary.",
	}, s.listBackground)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_background",
		Description: "Status and truncated output for one of your background jobs.",
	}, s.getBackground)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "cancel_background",
		Description: "Best-effort kill of one of your background jobs.",
	}, s.cancelBackground)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "archive_bot",
		Description: "End this session. Defaults to yourself; use it when the work is done.",
	}, s.archiveBot)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_project",
		Description: "Read the team's notes about a code base: what it is, its stack and how it is deployed.",
	}, s.getProject)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "set_project",
		Description: "Record or update the team's notes about a code base. Only the fields you pass are changed.",
	}, s.setProject)
}

func (s *mcpServer) registerSecrets(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "secrets_list",
		Description: "List owner vault secret names. Values are never returned. There is no secrets_get.",
	}, s.secretsList)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "secrets_delete",
		Description: "Delete one owner vault secret by name. Values are never returned.",
	}, s.secretsDelete)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "run",
		Description: "Run a shell command with vault secrets injected into env and/or stdin. Pass env as env-var-name → secret-name. The value never appears in argv or in the tool result (output is redacted). Use this instead of Bash when a secret is needed.",
	}, s.runSecret)
}

func (s *mcpServer) watch(_ context.Context, _ *mcp.CallToolRequest, in watchIn) (*mcp.CallToolResult, any, error) {
	w, err := upsertWatch(s.db, s.botID, in.Name, in.Command, in.IntervalS)
	if err != nil {
		return toolErr("%v", err), nil, nil
	}
	ttl := watchTTL(s.config)
	if ttl <= 0 {
		return text("watching %q every %ds; you will be woken when the output changes", w.Name, w.IntervalS), nil, nil
	}
	return text("watching %q every %ds for up to %s; you will be woken when the output changes, and again if it expires so you can re-set it. For standing jobs use set_routine",
		w.Name, w.IntervalS, humanDuration(ttl)), nil, nil
}

func (s *mcpServer) unwatch(_ context.Context, _ *mcp.CallToolRequest, in unwatchIn) (*mcp.CallToolResult, any, error) {
	removed, err := deleteWatch(s.db, s.botID, in.Name)
	if err != nil {
		return toolErr("could not remove the watch: %v", err), nil, nil
	}
	if !removed {
		return text("you have no watch named %q", in.Name), nil, nil
	}
	return text("removed watch %q", in.Name), nil, nil
}

func (s *mcpServer) listWatchesTool(_ context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
	watches, err := listWatches(s.db, s.botID)
	if err != nil {
		return toolErr("could not list watches: %v", err), nil, nil
	}
	if len(watches) == 0 {
		return text("no watches"), nil, nil
	}
	var sb strings.Builder
	for _, w := range watches {
		last := "never run"
		if w.LastRunAt != nil {
			last = "last run " + w.LastRunAt.Format(time.RFC3339)
		}
		state := "enabled"
		if !w.Enabled {
			state = "disabled"
		}
		fmt.Fprintf(&sb, "%s [%s, every %ds, %s, %s]: %s\n", w.Name, state, w.IntervalS, last, watchExpiryLabel(w, time.Now(), watchTTL(s.config)), w.Command)
	}
	return text("%s", strings.TrimRight(sb.String(), "\n")), nil, nil
}

// maxScheduleHorizon stops a bot scheduling itself past any useful future.
const maxScheduleHorizon = 365 * 24 * time.Hour

func (s *mcpServer) scheduleWakeup(_ context.Context, _ *mcp.CallToolRequest, in scheduleIn) (*mcp.CallToolResult, any, error) {
	now := time.Now()
	var fireAt time.Time
	switch {
	case strings.TrimSpace(in.Cron) != "":
		schedule, err := parseCron(in.Cron)
		if err != nil {
			return toolErr("cannot parse the cron expression %q: %v", in.Cron, err), nil, nil
		}
		fireAt = schedule.Next(now)
	case strings.TrimSpace(in.At) != "":
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(in.At))
		if err != nil {
			return toolErr("`at` must be RFC3339 (e.g. 2026-01-02T15:04:05Z): %v", err), nil, nil
		}
		fireAt = t
	case in.InSeconds > 0:
		fireAt = now.Add(time.Duration(in.InSeconds) * time.Second)
	default:
		return toolErr("give in_seconds, at, or cron"), nil, nil
	}
	fireAt = fireAt.UTC()
	if fireAt.After(now.Add(maxScheduleHorizon)) {
		return toolErr("that is more than a year away"), nil, nil
	}
	row := Schedule{BotID: s.botID, FireAt: fireAt, Note: truncate(strings.TrimSpace(in.Note), 2000), RecurringCron: strings.TrimSpace(in.Cron)}
	if err := s.db.Create(&row).Error; err != nil {
		return toolErr("could not record the wakeup: %v", err), nil, nil
	}
	if row.RecurringCron != "" {
		return text("scheduled #%d, next at %s, repeating on %q", row.ID, fireAt.Format(time.RFC3339), row.RecurringCron), nil, nil
	}
	return text("scheduled #%d for %s", row.ID, fireAt.Format(time.RFC3339)), nil, nil
}

func (s *mcpServer) cancelSchedule(_ context.Context, _ *mcp.CallToolRequest, in cancelScheduleIn) (*mcp.CallToolResult, any, error) {
	// Scoped to the calling bot: a bot can only cancel its own wakeups.
	res := s.db.Where("id = ? AND bot_id = ?", in.ID, s.botID).Delete(&Schedule{})
	if res.Error != nil {
		return toolErr("could not cancel: %v", res.Error), nil, nil
	}
	if res.RowsAffected == 0 {
		return text("you have no schedule #%d", in.ID), nil, nil
	}
	return text("cancelled schedule #%d", in.ID), nil, nil
}

func (s *mcpServer) setRoutine(_ context.Context, _ *mcp.CallToolRequest, in setRoutineIn) (*mcp.CallToolResult, any, error) {
	row, err := upsertRoutine(s.db, s.botID, in.Name, in.Prompt, in.Cron, in.Timezone, time.Now())
	if err != nil {
		return toolErr("%v", err), nil, nil
	}
	return text("routine %q next at %s (%s, %s)", row.Name, row.FireAt.Format(time.RFC3339), row.RecurringCron, row.Timezone), nil, nil
}

func (s *mcpServer) listRoutinesTool(_ context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
	rows, err := listRoutines(s.db, s.botID)
	if err != nil {
		return toolErr("could not list routines: %v", err), nil, nil
	}
	if len(rows) == 0 {
		return text("no routines"), nil, nil
	}
	var sb strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&sb, "%s  next %s  %s %s\n  %s\n", r.Name, r.FireAt.Format(time.RFC3339), r.RecurringCron, r.Timezone, r.Note)
	}
	return text("%s", strings.TrimRight(sb.String(), "\n")), nil, nil
}

func (s *mcpServer) cancelRoutineTool(_ context.Context, _ *mcp.CallToolRequest, in cancelRoutineIn) (*mcp.CallToolResult, any, error) {
	ok, err := cancelRoutine(s.db, s.botID, in.Name)
	if err != nil {
		return toolErr("%v", err), nil, nil
	}
	if !ok {
		return text("you have no routine named %q", sanitizeRoutineName(in.Name)), nil, nil
	}
	return text("cancelled routine %q", sanitizeRoutineName(in.Name)), nil, nil
}

func (s *mcpServer) runBackground(_ context.Context, _ *mcp.CallToolRequest, in runBackgroundIn) (*mcp.CallToolResult, any, error) {
	job, err := queueBackgroundJob(s.db, s.botID, s.turnID, in.Name, in.Command, bgSecrets{Env: in.Env, StdinSecret: in.StdinSecret})
	if err != nil {
		return toolErr("%v", err), nil, nil
	}
	return text("started background job #%d %q; you will be woken with source=background when it finishes", job.ID, job.Name), nil, nil
}

func (s *mcpServer) secretsList(_ context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
	names, err := listSecretNames()
	if err != nil {
		return toolErr("could not list secrets: %v", err), nil, nil
	}
	if len(names) == 0 {
		return text("no secrets"), nil, nil
	}
	return text("%s", strings.Join(names, "\n")), nil, nil
}

func (s *mcpServer) secretsDelete(_ context.Context, _ *mcp.CallToolRequest, in secretNameIn) (*mcp.CallToolResult, any, error) {
	name := strings.TrimSpace(in.Name)
	ok, err := deleteSecret(name)
	if err != nil {
		return toolErr("%v", err), nil, nil
	}
	if !ok {
		return text("no secret named %s", name), nil, nil
	}
	return text("deleted %s", name), nil, nil
}

func (s *mcpServer) runSecret(ctx context.Context, _ *mcp.CallToolRequest, in runIn) (*mcp.CallToolResult, any, error) {
	b, err := s.bot()
	if err != nil {
		return toolErr("unknown bot"), nil, nil
	}
	exit, output, err := runWithSecrets(ctx, s.config, botCwd(s.config, b), in.Command, in.Env, in.StdinSecret)
	if err != nil {
		return toolErr("%v", err), nil, nil
	}
	return text("%s", formatRunResult(exit, output)), nil, nil
}

func (s *mcpServer) listBackground(_ context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
	jobs, err := listBackgroundJobs(s.db, s.botID)
	if err != nil {
		return toolErr("could not list background jobs: %v", err), nil, nil
	}
	return text("%s", formatBackgroundList(jobs)), nil, nil
}

func (s *mcpServer) getBackground(_ context.Context, _ *mcp.CallToolRequest, in backgroundIDIn) (*mcp.CallToolResult, any, error) {
	j, err := getBackgroundJob(s.db, s.botID, in.ID)
	if err != nil {
		return toolErr("%v", err), nil, nil
	}
	return text("%s", formatBackgroundJob(j)), nil, nil
}

func (s *mcpServer) cancelBackground(_ context.Context, _ *mcp.CallToolRequest, in backgroundIDIn) (*mcp.CallToolResult, any, error) {
	msg, err := cancelBackgroundJob(s.db, s.botID, in.ID, func(pid int) {
		var j BackgroundJob
		if err := s.db.Where("id = ? AND bot_id = ?", in.ID, s.botID).First(&j).Error; err != nil {
			return
		}
		// ccc mcp is a different process from listen, so the PID in the row
		// is only safe to signal when it is still the wrapper we started.
		killOwnedJobPID(pid, j.StartedAt, backgroundJobDir(s.config, j.ID))
	})
	if err != nil {
		return toolErr("%v", err), nil, nil
	}
	return text("%s", msg), nil, nil
}

func (s *mcpServer) archiveBot(_ context.Context, _ *mcp.CallToolRequest, in archiveBotIn) (*mcp.CallToolResult, any, error) {
	target, err := s.bot()
	if err != nil {
		return toolErr("unknown bot"), nil, nil
	}
	if name := strings.TrimSpace(in.Bot); name != "" && name != target.Name {
		if !s.isChief() {
			return toolErr("you can only archive yourself"), nil, nil
		}
		target, err = botByName(s.db, name)
		if err != nil {
			return toolErr("no live bot named %q", name), nil, nil
		}
	}
	if isGeneralBot(target) {
		return toolErr("General cannot be archived"), nil, nil
	}
	if err := archiveBotRow(s.db, target.ID); err != nil {
		return toolErr("could not archive: %v", err), nil, nil
	}
	s.post(0, "📦 Session <b>"+htmlEscape(target.Name)+"</b> ended. Memories are kept.")
	if target.ID == s.botID && mcpProcess {
		// Stop the engine (and its tools) so this turn actually ends and
		// report_to_general can wake General. listen also watches archived_at.
		go terminateSelfProcessGroup()
	}
	return text("archived %s", target.Name), nil, nil
}

// terminateSelfProcessGroup SIGTERMs the engine process group this MCP
// server lives in (listen starts each turn with Setpgid). A short delay lets
// the tool result reach the model and the Telegram "session ended" post
// finish; listen's archive watcher is the backup if this does not fire.
func terminateSelfProcessGroup() {
	time.Sleep(150 * time.Millisecond)
	pgid, err := syscall.Getpgid(0)
	if err != nil || pgid <= 1 {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM) // safe-ignore: listen's watcher also kills
}

func (s *mcpServer) getProject(_ context.Context, _ *mcp.CallToolRequest, in getProjectIn) (*mcp.CallToolResult, any, error) {
	path := expandPath(strings.TrimSpace(in.Path))
	if path == "" {
		return toolErr("get_project needs a path"), nil, nil
	}
	var p Project
	if err := s.db.Where("path = ?", path).First(&p).Error; err != nil {
		return text("nothing recorded for %s yet — use set_project when you learn something durable about it", path), nil, nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "path: %s\n", p.Path)
	for _, f := range [][2]string{{"name", p.Name}, {"description", p.Description}, {"stack", p.Stack}, {"deploy", p.DeployNotes}} {
		if strings.TrimSpace(f[1]) != "" {
			fmt.Fprintf(&sb, "%s: %s\n", f[0], f[1])
		}
	}
	return text("%s", strings.TrimRight(sb.String(), "\n")), nil, nil
}

func (s *mcpServer) setProject(_ context.Context, _ *mcp.CallToolRequest, in setProjectIn) (*mcp.CallToolResult, any, error) {
	path := expandPath(strings.TrimSpace(in.Path))
	if path == "" {
		return toolErr("set_project needs a path"), nil, nil
	}
	updates := map[string]any{"updated_at": time.Now()}
	for key, value := range map[string]string{
		"name": in.Name, "description": in.Description, "stack": in.Stack, "deploy_notes": in.DeployNotes,
	} {
		if strings.TrimSpace(value) != "" {
			updates[key] = truncate(value, 4000)
		}
	}
	var p Project
	err := s.db.Where("path = ?", path).First(&p).Error
	if err != nil {
		p = Project{Path: path, Name: in.Name, Description: in.Description, Stack: in.Stack, DeployNotes: in.DeployNotes, UpdatedAt: time.Now()}
		if err := s.db.Create(&p).Error; err != nil {
			return toolErr("could not record the project: %v", err), nil, nil
		}
		return text("recorded %s", path), nil, nil
	}
	if err := s.db.Model(&Project{}).Where("id = ?", p.ID).Updates(updates).Error; err != nil {
		return toolErr("could not update the project: %v", err), nil, nil
	}
	return text("updated %s", path), nil, nil
}
