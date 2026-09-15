package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
		return fmt.Errorf("ccc mcp requires --bot <id>")
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
	return server.Run(context.Background(), &mcp.StdioTransport{})
}

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
	Scope       string `json:"scope" jsonschema:"where the memory belongs: user (about the owner, shared by all bots), project (about one code base) or bot (private to you)"`
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
	Bot  string `json:"bot" jsonschema:"name of the target bot (see list_bots)"`
	Text string `json:"text" jsonschema:"the message"`
	Wake *bool  `json:"wake,omitempty" jsonschema:"run the target bot now instead of waiting for its next turn (default true)"`
}

type notifyOwnerIn struct {
	Text    string `json:"text" jsonschema:"what to tell the owner"`
	Urgency string `json:"urgency,omitempty" jsonschema:"normal (default) or urgent; urgent also sends a direct message"`
}

type askOwnerIn struct {
	Question string   `json:"question" jsonschema:"the question, one sentence"`
	Options  []string `json:"options,omitempty" jsonschema:"up to 4 answers to offer as buttons; omit for a free-text answer"`
}

type updateInstructionsIn struct {
	Role string `json:"role" jsonschema:"your new role description, replacing the current one"`
}

type setNameIn struct {
	Name  string `json:"name" jsonschema:"your new name: a short, unique handle the owner and the other bots address you by"`
	Emoji string `json:"emoji,omitempty" jsonschema:"icon for your Telegram topic; must be one of the emoji listed in this tool's description"`
}

type sendFileIn struct {
	Path    string `json:"path" jsonschema:"absolute path of the file to send"`
	Caption string `json:"caption,omitempty" jsonschema:"optional caption"`
}

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

func (s *mcpServer) register(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "remember",
		Description: "Store a durable fact so you and the other bots still know it in future conversations.",
	}, s.remember)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "recall",
		Description: "Search the memories you can see: all user memories, all project memories and your own.",
	}, s.recall)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "forget",
		Description: "Delete one memory by scope and key.",
	}, s.forget)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_bots",
		Description: "List the other bots in the team with their roles and status.",
	}, s.listBots)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "send_to_bot",
		Description: "Send a message to another bot. It is mirrored into both Telegram topics so the owner sees it.",
	}, s.sendToBot)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "notify_owner",
		Description: "Tell the owner something in your Telegram topic. Use urgency=urgent only when it is worth an interruption.",
	}, s.notifyOwner)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "ask_owner",
		Description: "Ask the owner a question and END YOUR TURN. The answer arrives as your next message.",
	}, s.askOwner)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "update_instructions",
		Description: "Replace your own role description. This starts a fresh conversation on your next message.",
	}, s.updateInstructions)
	mcp.AddTool(server, &mcp.Tool{
		Name: "set_name",
		Description: "Rename yourself: the name is how the owner and the other bots address you, and the title of your " +
			"Telegram topic, so keep it short and unique. This starts a fresh conversation on your next message. " +
			s.iconEmojiHint(),
	}, s.setName)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "send_file",
		Description: "Send a file from this machine into your Telegram topic (max 50 MB).",
	}, s.sendFile)
	s.registerAutomation(server)
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
	if _, _, err := queueBotMessage(s.db, s.config, self, target.Name, body, wake); err != nil {
		return toolErr("%s", err.Error()), nil, nil
	}
	return text("message queued for %s", target.Name), nil, nil
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
	msg := fmt.Sprintf("%s %s", prefix, renderTelegramHTML(truncate(body, 3000)))
	s.post(b.TopicID, msg)
	if strings.EqualFold(in.Urgency, "urgent") && s.config.ChatID != 0 && s.config.BotToken != "" {
		_, _ = sendMessageHTMLGetID(s.config, s.config.ChatID, 0,
			fmt.Sprintf("%s <b>%s</b>: %s", prefix, htmlEscape(b.Name), renderTelegramHTML(truncate(body, 3000)))) // safe-ignore: the topic message already went out
	}
	return text("owner notified"), nil, nil
}

// maxQuestionOptions is Telegram-friendly and matches DESIGN §6 (≤4 options).
const maxQuestionOptions = 4

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
	msgID := s.postQuestion(b.TopicID, row.ID, q, opts)
	if msgID != 0 {
		s.db.Model(&Question{}).Where("id = ?", row.ID).Update("asked_message_id", msgID)
	}
	return text(`{"status":"asked","question_id":%d} — end your turn now; the answer arrives as your next message`, row.ID), nil, nil
}

func (s *mcpServer) updateInstructions(_ context.Context, _ *mcp.CallToolRequest, in updateInstructionsIn) (*mcp.CallToolResult, any, error) {
	role := strings.TrimSpace(in.Role)
	if role == "" {
		return toolErr("update_instructions needs a role"), nil, nil
	}
	if len(role) > 4000 {
		role = role[:4000]
	}
	b, err := s.bot()
	if err != nil {
		return toolErr("unknown bot"), nil, nil
	}
	if err := s.db.Model(&Bot{}).Where("id = ?", b.ID).Update("role", role).Error; err != nil {
		return toolErr("could not update the role: %v", err), nil, nil
	}
	s.post(b.TopicID, "📝 <b>New role</b>\n"+renderTelegramHTML(truncate(role, 3000)))
	return text("role updated; your next message starts a fresh conversation with it"), nil, nil
}

// iconEmojiHint names the emoji Telegram accepts as topic icons. It goes into
// the tool descriptions so the model picks one that exists instead of guessing
// and being told no.
func (s *mcpServer) iconEmojiHint() string {
	available := topicIconEmoji(topicIcons(s.db, s.config))
	if len(available) == 0 {
		return "The optional emoji sets your topic icon."
	}
	return "The optional emoji sets your topic icon; it must be one of: " + strings.Join(available, " ")
}

func (s *mcpServer) setName(_ context.Context, _ *mcp.CallToolRequest, in setNameIn) (*mcp.CallToolResult, any, error) {
	b, err := s.bot()
	if err != nil {
		return toolErr("unknown bot"), nil, nil
	}
	name, err := validateBotName(s.db, b.ID, in.Name)
	if err != nil {
		return toolErr("%v", err), nil, nil
	}
	iconID, iconNote := resolveTopicIcon(s.db, s.config, in.Emoji)
	old := b.Name
	if name != old {
		if err := renameBot(s.db, s.config, b, name); err != nil {
			return toolErr("could not rename: %v", err), nil, nil
		}
	}
	if err := editForumTopic(s.config, b.TopicID, name, iconID); err != nil {
		hookLog("edit topic %d: %v", b.TopicID, err)
	}
	if name != old {
		s.post(b.TopicID, fmt.Sprintf("✏️ <b>%s</b> is now <b>%s</b>", htmlEscape(old), htmlEscape(name)))
	}
	out := fmt.Sprintf("renamed to %q; your next message starts a fresh conversation with it", name)
	if name == old {
		out = fmt.Sprintf("you were already called %q", name)
	}
	if iconNote != "" {
		out += ". " + iconNote
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
		return toolErr("%s is %d bytes, over the 50 MB Telegram limit", abs, info.Size()), nil, nil
	}
	b, err := s.bot()
	if err != nil {
		return toolErr("unknown bot"), nil, nil
	}
	if s.config.BotToken == "" || s.config.GroupID == 0 {
		return text("(no Telegram configured; %s not sent)", abs), nil, nil
	}
	if err := sendFile(s.config, s.config.GroupID, b.TopicID, abs, in.Caption); err != nil {
		return toolErr("send failed: %v", err), nil, nil
	}
	return text("sent %s", filepath.Base(abs)), nil, nil
}

// sendFileForbidden implements the DESIGN §12 rule: credentials never leave the
// machine through send_file. Anything inside a Claude config dir, inside
// <data_dir>/profiles, or with a credential-ish name is refused.
func sendFileForbidden(config *Config, abs string) (string, bool) {
	lower := strings.ToLower(filepath.Base(abs))
	for _, bad := range []string{".credentials.json", "credentials.json", ".claude.json", "id_rsa", "id_ed25519", ".env"} {
		if lower == bad {
			return "it looks like a credentials file", true
		}
	}
	guarded := []string{filepath.Join(dataDir(config), "profiles")}
	for _, p := range listProfiles(config) {
		guarded = append(guarded, claudeHome(p))
	}
	home, err := os.UserHomeDir()
	if err == nil {
		guarded = append(guarded, filepath.Join(home, ".ssh"), filepath.Join(home, ".aws"), configDir())
	}
	for _, g := range guarded {
		if g == "" {
			continue
		}
		if abs == g || strings.HasPrefix(abs, strings.TrimRight(g, string(filepath.Separator))+string(filepath.Separator)) {
			return "it is inside a credentials/config directory", true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// Telegram side of the MCP process
// ---------------------------------------------------------------------------

// post writes into a topic, or does nothing when this instance has no Telegram
// configured (which is how the runner is exercised in tests).
func (s *mcpServer) post(topicID int64, html string) {
	if s.config == nil || s.config.BotToken == "" || s.config.GroupID == 0 || topicID == 0 {
		return
	}
	_, _ = sendMessageHTMLGetID(s.config, s.config.GroupID, topicID, html) // safe-ignore: a failed mirror must not fail the tool call
}

// postQuestion renders an ask_owner question with one button per option and
// returns the message id so a tap can be matched back to the question.
func (s *mcpServer) postQuestion(topicID, questionID int64, question string, options []string) int64 {
	if s.config == nil || s.config.BotToken == "" || s.config.GroupID == 0 || topicID == 0 {
		return 0
	}
	body := "❓ " + renderTelegramHTML(question)
	if len(options) == 0 {
		body += "\n<i>Reply to this message with your answer.</i>"
		id, _ := sendMessageHTMLGetID(s.config, s.config.GroupID, topicID, body) // safe-ignore: a question with no message id can still be answered by reply
		return id
	}
	var rows [][]InlineKeyboardButton
	for i, o := range options {
		rows = append(rows, []InlineKeyboardButton{{
			Text:         o,
			CallbackData: fmt.Sprintf("q:%d:%d", questionID, i),
		}})
	}
	id, err := sendMessageKeyboardGetID(s.config, s.config.GroupID, topicID, body, rows)
	if err != nil {
		return 0
	}
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

// answerQuestion records an answer and returns the text to feed back into the
// bot as its next input (DESIGN §6 ask_owner).
func answerQuestion(db *gorm.DB, q *Question, answer string) string {
	now := time.Now()
	db.Model(&Question{}).Where("id = ?", q.ID).
		Updates(map[string]any{"answer": answer, "answered_at": now})
	return fmt.Sprintf("Answer to %q: %s", q.Question, answer)
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
	Name    string `json:"name,omitempty" jsonschema:"short summary shown in list_background, e.g. go test or npm install"`
	Command string `json:"command" jsonschema:"shell command run in your working directory with env_passthrough; returns a job id immediately and does not block this turn"`
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
		Description: "Re-run a command on an interval and wake you ONLY when its output changes. Costs nothing while nothing changes.",
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
		Description: "Ask ccc to start a turn for you later, once or on a cron schedule.",
	}, s.scheduleWakeup)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "cancel_schedule",
		Description: "Cancel one of your pending wakeups.",
	}, s.cancelSchedule)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "set_routine",
		Description: "Create or replace a named recurring routine. It always fires (unlike a watch), in the given timezone, and posts ⏰ in your topic. Upserts by name.",
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
		Description: "Close a bot's topic and retire it. Defaults to yourself; use it when your job is done.",
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

func (s *mcpServer) watch(_ context.Context, _ *mcp.CallToolRequest, in watchIn) (*mcp.CallToolResult, any, error) {
	w, err := upsertWatch(s.db, s.botID, in.Name, in.Command, in.IntervalS)
	if err != nil {
		return toolErr("%v", err), nil, nil
	}
	return text("watching %q every %ds; you will be woken when the output changes", w.Name, w.IntervalS), nil, nil
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
		fmt.Fprintf(&sb, "%s [%s, every %ds, %s]: %s\n", w.Name, state, w.IntervalS, last, w.Command)
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
	job, err := queueBackgroundJob(s.db, s.botID, s.turnID, in.Name, in.Command)
	if err != nil {
		return toolErr("%v", err), nil, nil
	}
	return text("started background job #%d %q; you will be woken with source=background when it finishes", job.ID, job.Name), nil, nil
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
	msg, err := cancelBackgroundJob(s.db, s.botID, in.ID, killProcessGroup)
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
		target, err = botByName(s.db, name)
		if err != nil {
			return toolErr("no live bot named %q", name), nil, nil
		}
	}
	if err := archiveBotRow(s.db, target.ID); err != nil {
		return toolErr("could not archive: %v", err), nil, nil
	}
	s.post(target.TopicID, "📦 <b>"+htmlEscape(target.Name)+"</b> archived. The topic is closed; its memories are kept.")
	if s.config != nil && s.config.BotToken != "" && s.config.GroupID != 0 {
		if err := closeForumTopic(s.config, target.TopicID); err != nil {
			hookLog("close topic %d: %v", target.TopicID, err)
		}
	}
	return text("archived %s", target.Name), nil, nil
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
