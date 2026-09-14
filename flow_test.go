package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"
)

// fakeBotAPI is a stand-in Telegram Bot API: it answers the handful of methods
// ccc uses and records every call so the conversation tests can assert on what
// the user would have seen.
type fakeBotAPI struct {
	*httptest.Server

	mu      sync.Mutex
	calls   []apiCall
	nextMsg int64
	nextTop int64
}

type apiCall struct {
	Method string
	Params url.Values
}

func newFakeBotAPI(t *testing.T) *fakeBotAPI {
	t.Helper()
	f := &fakeBotAPI{nextMsg: 1000, nextTop: 500}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// File downloads are served from a different path shape than methods.
		if strings.HasPrefix(r.URL.Path, "/file/") {
			fmt.Fprint(w, "fake file contents")
			return
		}
		_ = r.ParseForm() // safe-ignore: a malformed body simply yields empty params
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		f.mu.Lock()
		f.calls = append(f.calls, apiCall{Method: method, Params: r.Form})
		var body string
		switch method {
		case "createForumTopic":
			f.nextTop++
			body = fmt.Sprintf(`{"ok":true,"result":{"message_thread_id":%d,"name":%q}}`,
				f.nextTop, r.Form.Get("name"))
		case "getForumTopicIconStickers":
			// The real set is ~30 stickers; two is enough to exercise the
			// lookup, the fallback and the "not available" path.
			body = `{"ok":true,"result":[{"custom_emoji_id":"icon-rocket","emoji":"🚀"},` +
				`{"custom_emoji_id":"icon-memo","emoji":"📝"}]}`
		case "getFile":
			body = `{"ok":true,"result":{"file_path":"documents/blob.bin"}}`
		case "sendMessage", "editMessageText":
			f.nextMsg++
			body = fmt.Sprintf(`{"ok":true,"result":{"message_id":%d}}`, f.nextMsg)
		default:
			body = `{"ok":true,"result":true}`
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	old := telegramBaseURL
	telegramBaseURL = f.URL
	t.Cleanup(func() {
		telegramBaseURL = old
		f.Close()
	})
	return f
}

func (f *fakeBotAPI) since(method string) []apiCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []apiCall
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// texts returns every message body sent to a thread ("" = any thread).
func (f *fakeBotAPI) texts(thread string) []string {
	var out []string
	for _, c := range f.since("sendMessage") {
		if thread != "" && c.Params.Get("message_thread_id") != thread {
			continue
		}
		out = append(out, c.Params.Get("text"))
	}
	return out
}

// fakeRunner records what the conversation layer asks the runner to do,
// so the flow tests never spawn claude.
type fakeRunner struct {
	db *gorm.DB

	mu       sync.Mutex
	enqueued []fakeTurn
	stops    []int64
}

type fakeTurn struct {
	BotID   int64
	Source  string
	Text    string
	Trigger int64
}

func (f *fakeRunner) Enqueue(botID int64, source, text string, trigger int64) (*Turn, error) {
	f.mu.Lock()
	f.enqueued = append(f.enqueued, fakeTurn{botID, source, text, trigger})
	f.mu.Unlock()
	t := &Turn{BotID: botID, Source: source, Input: text, Status: turnQueued, TriggerMessageID: trigger}
	if f.db != nil {
		if err := f.db.Create(t).Error; err != nil {
			return nil, err
		}
	}
	return t, nil
}

func (f *fakeRunner) Stop(botID int64) bool {
	f.mu.Lock()
	f.stops = append(f.stops, botID)
	f.mu.Unlock()
	return true
}

func (f *fakeRunner) last() (fakeTurn, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.enqueued) == 0 {
		return fakeTurn{}, false
	}
	return f.enqueued[len(f.enqueued)-1], true
}

// testInstance wires an instance against a temp database and the fake API.
func testInstance(t *testing.T) (*instance, *fakeRunner, *fakeBotAPI) {
	t.Helper()
	// Commands that write the configuration (/model, /setgroup, /account) go
	// through loadConfig/saveConfig, so the tests get their own HOME.
	t.Setenv("HOME", t.TempDir())
	// The topic-icon cache is process-wide; each test gets its own fake API.
	resetTopicIconCache()
	api := newFakeBotAPI(t)
	dir := t.TempDir()
	cfg := &Config{BotToken: "TESTTOKEN", ChatID: 42, GroupID: -100777, DataDir: dir}
	db, err := openStore(dbPath(cfg))
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	runner := &fakeRunner{db: db}
	return &instance{db: db, cfg: cfg, runner: runner, dataDir: dir}, runner, api
}

// ownerMessage builds an inbound group message from the owner.
func ownerMessage(threadID int64, text string) *TelegramMessage {
	m := &TelegramMessage{MessageThreadID: threadID, Text: text, MessageID: 7}
	m.Chat.ID = -100777
	m.Chat.Type = "supergroup"
	m.From.ID = 42
	return m
}

// An edited message is how a mistyped command gets fixed on a phone, so a
// command edit runs — exactly once per edit, however often Telegram redelivers
// it — while an edited plain message still does nothing.
func TestEditedCommandIsDispatchedOnce(t *testing.T) {
	in, runner, api := testInstance(t)
	if err := saveConfig(in.config()); err != nil {
		t.Fatal(err)
	}

	edit := ownerMessage(0, "/model sonnet")
	edit.EditDate = 1700000000
	in.handleEditedMessage(edit)
	in.handleEditedMessage(edit) // Telegram redelivers the same edit

	if got := in.config().Model; got != "sonnet" {
		t.Errorf("the edited command did not run: model = %q", got)
	}
	if n := strings.Count(strings.Join(api.texts(""), "\n"), "Model set to"); n != 1 {
		t.Errorf("the edited command ran %d times, want once", n)
	}

	// Editing the same message again is a new edit, and runs.
	again := ownerMessage(0, "/model opus")
	again.EditDate = 1700000060
	in.handleEditedMessage(again)
	if got := in.config().Model; got != "opus" {
		t.Errorf("a second edit of the same message did not run: model = %q", got)
	}

	// Plain text is still ignored: fixing a typo must not re-run a turn.
	plain := ownerMessage(0, "watch the deploy")
	plain.EditDate = 1700000120
	in.handleEditedMessage(plain)
	if _, ok := runner.last(); ok {
		t.Error("an edited plain message enqueued a turn")
	}
	if got := len(api.since("createForumTopic")); got != 0 {
		t.Errorf("an edited plain message created %d bots", got)
	}
}

func TestTextInGeneralCreatesBotAndTopic(t *testing.T) {
	in, runner, api := testInstance(t)

	in.handleMessage(ownerMessage(0, "watch the deploy\nand tell me when it is green"))

	topics := api.since("createForumTopic")
	if len(topics) != 1 {
		t.Fatalf("expected one topic created, got %d", len(topics))
	}
	if got := topics[0].Params.Get("name"); got != "watch the deploy" {
		t.Errorf("topic name = %q, want the first line of the message", got)
	}

	bots, err := liveBots(in.db)
	if err != nil || len(bots) != 1 {
		t.Fatalf("expected one bot, got %d (%v)", len(bots), err)
	}
	b := bots[0]
	if b.TopicID == 0 {
		t.Error("bot has no topic id")
	}
	if want := filepath.Join(in.dataDir, "bots", b.Name, "workspace"); b.Cwd != want {
		t.Errorf("cwd = %q, want %q", b.Cwd, want)
	}
	last, ok := runner.last()
	if !ok || last.BotID != b.ID || !strings.HasPrefix(last.Text, "watch the deploy") {
		t.Errorf("first message was not dispatched to the new bot: %+v", last)
	}
}

func TestTextInTopicEnqueuesTurnForThatBot(t *testing.T) {
	in, runner, _ := testInstance(t)
	b, err := in.createBot("deployer", "ships things")
	if err != nil {
		t.Fatalf("createBot: %v", err)
	}

	msg := ownerMessage(b.TopicID, "status?")
	msg.MessageID = 99
	in.handleMessage(msg)

	last, ok := runner.last()
	if !ok {
		t.Fatal("nothing enqueued")
	}
	if last.BotID != b.ID || last.Text != "status?" || last.Source != sourceUser {
		t.Errorf("unexpected turn: %+v", last)
	}
	if last.Trigger != 99 {
		t.Errorf("trigger message = %d, want 99 (needed for the ✅ reaction)", last.Trigger)
	}
}

func TestTextInUnknownTopicIsNotDispatched(t *testing.T) {
	in, runner, _ := testInstance(t)
	in.handleMessage(ownerMessage(31337, "hello?"))
	if _, ok := runner.last(); ok {
		t.Error("a message in a topic with no bot should not enqueue anything")
	}
}

func TestNonOwnerIsIgnored(t *testing.T) {
	in, runner, api := testInstance(t)
	msg := ownerMessage(0, "let me in")
	msg.From.ID = 999
	in.handleMessage(msg)
	if _, ok := runner.last(); ok {
		t.Error("a stranger's message must not create a bot")
	}
	if len(api.since("createForumTopic")) != 0 {
		t.Error("a stranger's message must not create a topic")
	}
}

func TestQuestionCallbackAnswersAndResumesTheBot(t *testing.T) {
	in, runner, _ := testInstance(t)
	b, err := in.createBot("asker", "")
	if err != nil {
		t.Fatalf("createBot: %v", err)
	}
	setBotStatus(in.db, b.ID, botWaiting)
	opts, err := json.Marshal([]string{"ship it", "hold"})
	if err != nil {
		t.Fatal(err)
	}
	q := Question{BotID: b.ID, Question: "Deploy to prod?", OptionsJSON: string(opts), AskedMessageID: 555}
	if err := in.db.Create(&q).Error; err != nil {
		t.Fatal(err)
	}

	cb := &CallbackQuery{ID: "cb1", Data: fmt.Sprintf("q:%d:0", q.ID)}
	cb.From.ID = 42
	cb.Message = ownerMessage(b.TopicID, "")
	cb.Message.MessageID = 555
	in.handleCallback(cb)

	var stored Question
	if err := in.db.First(&stored, q.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.AnsweredAt == nil || stored.Answer != "ship it" {
		t.Fatalf("question not answered: %+v", stored)
	}
	last, ok := runner.last()
	if !ok || last.BotID != b.ID || !strings.Contains(last.Text, "ship it") {
		t.Errorf("the answer was not fed back to the bot: %+v", last)
	}
	after, err := botByID(in.db, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != botIdle {
		t.Errorf("bot status = %q, want idle after answering", after.Status)
	}
}

func TestCallbackFromStrangerIsIgnored(t *testing.T) {
	in, runner, _ := testInstance(t)
	b, _ := in.createBot("asker", "")
	q := Question{BotID: b.ID, Question: "?", OptionsJSON: `["a"]`}
	in.db.Create(&q)

	cb := &CallbackQuery{ID: "cb", Data: fmt.Sprintf("q:%d:0", q.ID)}
	cb.From.ID = 999
	in.handleCallback(cb)

	var stored Question
	in.db.First(&stored, q.ID)
	if stored.AnsweredAt != nil {
		t.Error("a stranger must not be able to answer a question")
	}
	if _, ok := runner.last(); ok {
		t.Error("a stranger's tap must not enqueue a turn")
	}
}

func TestReplyToQuestionCountsAsAnswer(t *testing.T) {
	in, runner, _ := testInstance(t)
	b, _ := in.createBot("asker", "")
	q := Question{BotID: b.ID, Question: "Which branch?", AskedMessageID: 777}
	in.db.Create(&q)

	msg := ownerMessage(b.TopicID, "main")
	msg.ReplyToMessage = &TelegramMessage{MessageID: 777}
	in.handleMessage(msg)

	var stored Question
	in.db.First(&stored, q.ID)
	if stored.Answer != "main" {
		t.Fatalf("answer = %q, want main", stored.Answer)
	}
	last, _ := runner.last()
	if !strings.Contains(last.Text, `Answer to "Which branch?"`) {
		t.Errorf("answer envelope = %q", last.Text)
	}
}

func TestSendToBotMirrorsIntoBothTopics(t *testing.T) {
	in, _, api := testInstance(t)
	a, err := in.createBot("alpha", "")
	if err != nil {
		t.Fatal(err)
	}
	bb, err := in.createBot("beta", "")
	if err != nil {
		t.Fatal(err)
	}

	s := &mcpServer{db: in.db, config: in.cfg, botID: a.ID}
	res, _, err := s.sendToBot(t.Context(), nil, sendToBotIn{Bot: "beta", Text: "please review PR 12"})
	if err != nil {
		t.Fatalf("sendToBot: %v", err)
	}
	if res.IsError {
		t.Fatalf("sendToBot reported an error: %+v", res.Content)
	}

	var queued []InboxMessage
	if err := in.db.Where("to_bot_id = ?", bb.ID).Find(&queued).Error; err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0].FromBotID == nil || *queued[0].FromBotID != a.ID {
		t.Fatalf("inbox row not written correctly: %+v", queued)
	}

	fromTopic := fmt.Sprint(a.TopicID)
	toTopic := fmt.Sprint(bb.TopicID)
	seenFrom, seenTo := false, false
	for _, c := range api.since("sendMessage") {
		if !strings.Contains(c.Params.Get("text"), "please review PR 12") {
			continue
		}
		switch c.Params.Get("message_thread_id") {
		case fromTopic:
			seenFrom = true
		case toTopic:
			seenTo = true
		}
	}
	if !seenFrom || !seenTo {
		t.Errorf("mirror missing (sender topic: %v, target topic: %v)", seenFrom, seenTo)
	}
}

func TestSendToBotRejectsUnknownTarget(t *testing.T) {
	in, _, _ := testInstance(t)
	a, _ := in.createBot("alpha", "")
	s := &mcpServer{db: in.db, config: in.cfg, botID: a.ID}
	res, _, err := s.sendToBot(t.Context(), nil, sendToBotIn{Bot: "nobody", Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("sending to a bot that does not exist should be a tool error")
	}
}

func TestCommandsInTopic(t *testing.T) {
	in, runner, api := testInstance(t)
	b, _ := in.createBot("worker", "")
	in.db.Model(&Bot{}).Where("id = ?", b.ID).Update("session_id", "abc-123")

	in.handleMessage(ownerMessage(b.TopicID, "/role ships the deploy"))
	after, _ := botByID(in.db, b.ID)
	if after.Role != "ships the deploy" {
		t.Errorf("role = %q", after.Role)
	}
	if after.SessionID != "" {
		t.Error("/role must rotate the session: the system prompt is recorded per conversation")
	}

	in.db.Model(&Bot{}).Where("id = ?", b.ID).Update("session_id", "def-456")
	in.handleMessage(ownerMessage(b.TopicID, "/new"))
	after, _ = botByID(in.db, b.ID)
	if after.SessionID != "" {
		t.Error("/new must clear the session id")
	}

	in.handleMessage(ownerMessage(b.TopicID, "/stop"))
	if len(runner.stops) != 1 || runner.stops[0] != b.ID {
		t.Errorf("/stop did not reach the runner: %v", runner.stops)
	}

	in.handleMessage(ownerMessage(b.TopicID, "/cwd /definitely/not/here"))
	after, _ = botByID(in.db, b.ID)
	if after.Cwd == "/definitely/not/here" {
		t.Error("/cwd accepted a directory that does not exist")
	}
	dir := t.TempDir()
	in.handleMessage(ownerMessage(b.TopicID, "/cwd "+dir))
	after, _ = botByID(in.db, b.ID)
	if after.Cwd != dir {
		t.Errorf("cwd = %q, want %q", after.Cwd, dir)
	}

	if err := upsertMemory(in.db, scopeUser, "", "deploy-target", "vps3", b.ID); err != nil {
		t.Fatal(err)
	}
	in.handleMessage(ownerMessage(b.TopicID, "/memory deploy"))
	found := false
	for _, txt := range api.texts(fmt.Sprint(b.TopicID)) {
		if strings.Contains(txt, "deploy-target") {
			found = true
		}
	}
	if !found {
		t.Error("/memory did not show the stored memory")
	}

	in.db.Model(&Bot{}).Where("id = ?", b.ID).Update("session_id", "eng-789")
	in.handleMessage(ownerMessage(b.TopicID, "/engine"))
	in.handleMessage(ownerMessage(b.TopicID, "/engine grok"))
	after, _ = botByID(in.db, b.ID)
	if after.Engine != engineGrok {
		t.Errorf("engine = %q after /engine grok", after.Engine)
	}
	if after.SessionID != "" {
		t.Error("/engine must rotate the session: the conversation id is per CLI")
	}
	in.handleMessage(ownerMessage(b.TopicID, "/engine not-a-cli"))
	after, _ = botByID(in.db, b.ID)
	if after.Engine != engineGrok {
		t.Error("a bad /engine argument must not change the stored engine")
	}

	in.handleMessage(ownerMessage(b.TopicID, "/forget user deploy-target"))
	var n int64
	in.db.Model(&Memory{}).Where("key = ?", "deploy-target").Count(&n)
	if n != 0 {
		t.Error("/forget did not delete the memory")
	}
}

func TestBotsCommandWorksAnywhere(t *testing.T) {
	in, _, api := testInstance(t)
	if _, err := in.createBot("alpha", "does alpha things"); err != nil {
		t.Fatal(err)
	}
	in.handleMessage(ownerMessage(0, "/bots"))
	joined := strings.Join(api.texts(""), "\n")
	if !strings.Contains(joined, "alpha") || !strings.Contains(joined, "does alpha things") {
		t.Errorf("/bots output missing the bot: %q", joined)
	}
}

func TestDocumentIsSavedIntoTheBotInbox(t *testing.T) {
	in, runner, _ := testInstance(t)
	b, _ := in.createBot("filer", "")

	msg := ownerMessage(b.TopicID, "")
	msg.Document = &TelegramDocument{FileID: "file-1", FileName: "../../escape.txt"}
	msg.Caption = "read this"
	in.handleMessage(msg)

	last, ok := runner.last()
	if !ok {
		t.Fatal("nothing enqueued for the attachment")
	}
	wantDir := filepath.Join(b.Cwd, "inbox")
	if !strings.Contains(last.Text, wantDir) {
		t.Errorf("message %q does not point at the bot inbox %q", last.Text, wantDir)
	}
	if strings.Contains(last.Text, "..") {
		t.Errorf("path traversal survived sanitisation: %q", last.Text)
	}
}

func TestNameCommandRenamesTheBotAndTheTopic(t *testing.T) {
	in, _, api := testInstance(t)
	b, err := in.createBot("worker", "does things")
	if err != nil {
		t.Fatal(err)
	}
	in.db.Model(&Bot{}).Where("id = ?", b.ID).Update("session_id", "abc-123")

	// No argument shows the current name and changes nothing.
	in.handleMessage(ownerMessage(b.TopicID, "/name"))
	if len(api.since("editForumTopic")) != 0 {
		t.Error("/name with no argument must not rename anything")
	}
	if texts := api.texts(fmt.Sprint(b.TopicID)); len(texts) == 0 || !strings.Contains(texts[len(texts)-1], "worker") {
		t.Errorf("/name did not show the current name: %v", texts)
	}

	in.handleMessage(ownerMessage(b.TopicID, "/name shipper 🚀"))

	after, err := botByID(in.db, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != "shipper" {
		t.Errorf("name = %q, want shipper", after.Name)
	}
	if after.SessionID != "" {
		t.Error("/name must rotate the session: the name is part of the system prompt")
	}
	if after.Role != "does things" {
		t.Errorf("role = %q, want it untouched by a rename", after.Role)
	}
	edits := api.since("editForumTopic")
	if len(edits) != 1 {
		t.Fatalf("expected one editForumTopic call, got %d", len(edits))
	}
	if got := edits[0].Params.Get("name"); got != "shipper" {
		t.Errorf("topic renamed to %q, want shipper", got)
	}
	if got := edits[0].Params.Get("message_thread_id"); got != fmt.Sprint(b.TopicID) {
		t.Errorf("renamed topic %s, want %d", got, b.TopicID)
	}
	if got := edits[0].Params.Get("icon_custom_emoji_id"); got != "icon-rocket" {
		t.Errorf("icon id = %q, want the id 🚀 maps to", got)
	}
}

func TestNameCommandRejectsACollision(t *testing.T) {
	in, _, api := testInstance(t)
	if _, err := in.createBot("alpha", ""); err != nil {
		t.Fatal(err)
	}
	b, err := in.createBot("beta", "")
	if err != nil {
		t.Fatal(err)
	}
	in.db.Model(&Bot{}).Where("id = ?", b.ID).Update("session_id", "keep-me")

	// Case-insensitively taken: alpha already exists.
	in.handleMessage(ownerMessage(b.TopicID, "/name ALPHA"))

	after, err := botByID(in.db, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != "beta" {
		t.Errorf("name = %q, want beta: the rename should have been rejected", after.Name)
	}
	if after.SessionID != "keep-me" {
		t.Error("a rejected rename must not rotate the session")
	}
	if len(api.since("editForumTopic")) != 0 {
		t.Error("a rejected rename must not touch the topic")
	}
	joined := strings.Join(api.texts(fmt.Sprint(b.TopicID)), "\n")
	if !strings.Contains(joined, "alpha") {
		t.Errorf("the rejection should say which bot holds the name: %q", joined)
	}
}

func TestNameCommandReportsAnUnavailableIcon(t *testing.T) {
	in, _, api := testInstance(t)
	b, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}

	in.handleMessage(ownerMessage(b.TopicID, "/name shipper 🦄"))

	after, _ := botByID(in.db, b.ID)
	if after.Name != "shipper" {
		t.Errorf("name = %q: an unavailable icon must not block the rename", after.Name)
	}
	edits := api.since("editForumTopic")
	if len(edits) != 1 || edits[0].Params.Get("icon_custom_emoji_id") != "" {
		t.Errorf("an emoji Telegram does not offer must leave the icon alone: %v", edits)
	}
	joined := strings.Join(api.texts(fmt.Sprint(b.TopicID)), "\n")
	if !strings.Contains(joined, "🦄") || !strings.Contains(joined, "🚀") {
		t.Errorf("the reply should name the rejected emoji and the available ones: %q", joined)
	}
}

func TestTopicRenameSyncsTheBotName(t *testing.T) {
	in, _, api := testInstance(t)
	b, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}
	in.db.Model(&Bot{}).Where("id = ?", b.ID).Update("session_id", "abc-123")

	renamed := ownerMessage(b.TopicID, "")
	renamed.ForumTopicEdited = &ForumTopicEdited{Name: "shipper"}
	in.handleMessage(renamed)

	after, err := botByID(in.db, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != "shipper" {
		t.Errorf("name = %q, want the topic title", after.Name)
	}
	if after.SessionID != "" {
		t.Error("a synced rename rotates the session too")
	}
	// The title is already right, so ccc must not rename it back.
	if len(api.since("editForumTopic")) != 0 {
		t.Error("following a topic rename must not call editForumTopic")
	}
}

func TestTopicRenameKeepsTheOldNameOnACollision(t *testing.T) {
	in, _, api := testInstance(t)
	if _, err := in.createBot("alpha", ""); err != nil {
		t.Fatal(err)
	}
	b, err := in.createBot("beta", "")
	if err != nil {
		t.Fatal(err)
	}

	renamed := ownerMessage(b.TopicID, "")
	renamed.ForumTopicEdited = &ForumTopicEdited{Name: "alpha"}
	in.handleMessage(renamed)

	after, _ := botByID(in.db, b.ID)
	if after.Name != "beta" {
		t.Errorf("name = %q, want beta: a colliding title must not be adopted", after.Name)
	}
	joined := strings.Join(api.texts(fmt.Sprint(b.TopicID)), "\n")
	if !strings.Contains(joined, "beta") {
		t.Errorf("the topic should have been told the name was kept: %q", joined)
	}
}

func TestSendToBotFollowsTheRename(t *testing.T) {
	in, _, _ := testInstance(t)
	sender, err := in.createBot("alpha", "")
	if err != nil {
		t.Fatal(err)
	}
	target, err := in.createBot("beta", "")
	if err != nil {
		t.Fatal(err)
	}

	in.handleMessage(ownerMessage(target.TopicID, "/name gamma"))

	s := &mcpServer{db: in.db, config: in.cfg, botID: sender.ID}
	res, _, err := s.sendToBot(t.Context(), nil, sendToBotIn{Bot: "gamma", Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Errorf("the new name should be addressable: %+v", res.Content)
	}
	res, _, err = s.sendToBot(t.Context(), nil, sendToBotIn{Bot: "beta", Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("the old name should no longer resolve")
	}
}

func TestSetNameToolRenamesAndSetsTheIcon(t *testing.T) {
	in, _, api := testInstance(t)
	b, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}
	s := &mcpServer{db: in.db, config: in.cfg, botID: b.ID}

	res, _, err := s.setName(t.Context(), nil, setNameIn{Name: "shipper", Emoji: "📝"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("set_name failed: %+v", res.Content)
	}
	after, _ := botByID(in.db, b.ID)
	if after.Name != "shipper" {
		t.Errorf("name = %q, want shipper", after.Name)
	}
	edits := api.since("editForumTopic")
	if len(edits) != 1 || edits[0].Params.Get("name") != "shipper" ||
		edits[0].Params.Get("icon_custom_emoji_id") != "icon-memo" {
		t.Errorf("set_name did not update the topic correctly: %v", edits)
	}

	// A taken name is a tool error, not a silent no-op.
	if _, err := in.createBot("taken", ""); err != nil {
		t.Fatal(err)
	}
	res, _, err = s.setName(t.Context(), nil, setNameIn{Name: "taken"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("set_name should refuse a name another bot already has")
	}
}

// A bot created from General has no role, so its very first turn must carry the
// onboarding instruction. The fake runner records the raw input; the envelope
// the real runner would build from it is rendered here.
func TestNewBotIsOnboardedOnItsFirstTurn(t *testing.T) {
	in, runner, _ := testInstance(t)
	in.handleMessage(ownerMessage(0, "help me with the deploy"))

	last, ok := runner.last()
	if !ok {
		t.Fatal("nothing enqueued for the new bot")
	}
	b, err := botByID(in.db, last.BotID)
	if err != nil {
		t.Fatal(err)
	}
	envelope := buildEnvelope(in.db, b, last.Source, last.Text, time.Now())
	for _, want := range []string{"no role yet", "update_instructions", "set_name", "help me with the deploy"} {
		if !strings.Contains(envelope, want) {
			t.Errorf("the first turn is missing %q:\n%s", want, envelope)
		}
	}
}

func TestUsageCommandWorksAnywhere(t *testing.T) {
	in, _, api := testInstance(t)
	b, err := in.createBot("spender", "")
	if err != nil {
		t.Fatal(err)
	}
	usageTurn(t, in, b.ID, time.Now(), 12*time.Second,
		`{"input_tokens":1000,"cache_read_input_tokens":4000,"cost_usd":0.4}`)

	in.handleMessage(ownerMessage(0, "/usage"))
	texts := api.texts("")
	if len(texts) == 0 || !strings.Contains(texts[len(texts)-1], "spender") {
		t.Errorf("/usage did not report the bot: %v", texts)
	}
	if !strings.Contains(texts[len(texts)-1], "cache 80%") {
		t.Errorf("/usage does not show the cache hit ratio: %v", texts)
	}
}

func TestMemoryStatsAndRestoreCommands(t *testing.T) {
	in, _, api := testInstance(t)
	b, err := in.createBot("rememberer", "")
	if err != nil {
		t.Fatal(err)
	}
	seedMemories(t, in.db, memCompactMaxCount+10)
	rep := runMaintenance(in.db, in.cfg, maintenanceDeps{Turner: answering(compactionAnswer(60))}, time.Now())
	if len(rep.Compactions) != 1 {
		t.Fatalf("no compaction to work with: %v", rep.Problems)
	}
	id := rep.Compactions[0].CompactionID

	in.handleMessage(ownerMessage(b.TopicID, "/memory stats"))
	last := func() string {
		texts := api.texts("")
		return texts[len(texts)-1]
	}
	if !strings.Contains(last(), "user memories") {
		t.Errorf("/memory stats = %q", last())
	}

	// A non-owner may look, but not undo.
	stranger := ownerMessage(b.TopicID, fmt.Sprintf("/memory restore %d", id))
	stranger.From.ID = 4242
	if err := in.db.Create(&Access{TelegramUserID: 4242, State: accessApproved}).Error; err != nil {
		t.Fatal(err)
	}
	in.handleMessage(stranger)
	if !strings.Contains(last(), "owner-only") {
		t.Errorf("restore is owner-only: %q", last())
	}

	in.handleMessage(ownerMessage(b.TopicID, fmt.Sprintf("/memory restore %d", id)))
	if !strings.Contains(last(), "Restored") {
		t.Errorf("/memory restore = %q", last())
	}
	var live int64
	in.db.Model(&Memory{}).Where("scope = ?", scopeUser).Count(&live)
	if live != int64(memCompactMaxCount+10) {
		t.Errorf("%d memories after the restore, want the originals back", live)
	}
}

// TestSetCommandIsGone locks in that instance tuning has no Telegram surface:
// debounce_ms, compaction_model and maintenance_hour are config.json keys
// (`ccc config set …`), and defaults are what everyone else gets.
func TestSetCommandIsGone(t *testing.T) {
	in, _, api := testInstance(t)

	in.handleMessage(ownerMessage(0, "/set debounce_ms 800"))

	texts := api.texts("")
	if len(texts) == 0 {
		t.Fatal("/set produced no reply at all")
	}
	if last := texts[len(texts)-1]; strings.Contains(last, "debounce_ms") || strings.Contains(last, "Instance settings") {
		t.Errorf("/set still has a Telegram surface: %q", last)
	}
	if debounceMS(in.config()) != defaultDebounceMS {
		t.Errorf("debounce is %d, want the default %d", debounceMS(in.config()), defaultDebounceMS)
	}
}
