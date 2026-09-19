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
}

type apiCall struct {
	Method string
	Params url.Values
}

func newFakeBotAPI(t *testing.T) *fakeBotAPI {
	t.Helper()
	f := &fakeBotAPI{nextMsg: 1000}
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
// isolateConfigEnv drops CCC_CONFIG/CCC_DB inherited from a ccc turn so tests
// that pin HOME actually read the temp config, not the instance's.
func isolateConfigEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CCC_CONFIG", "")
	t.Setenv("CCC_DB", "")
}

func testInstance(t *testing.T) (*instance, *fakeRunner, *fakeBotAPI) {
	t.Helper()
	// Commands that write the configuration (/model, /account) go through
	// loadConfig/saveConfig, so the tests get their own HOME.
	isolateConfigEnv(t)
	t.Setenv("HOME", t.TempDir())
	api := newFakeBotAPI(t)
	dir := t.TempDir()
	cfg := &Config{BotToken: "TESTTOKEN", ChatID: 42, DataDir: dir}
	db, err := openStore(dbPath(cfg))
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	runner := &fakeRunner{db: db}
	return &instance{db: db, cfg: cfg, runner: runner, dataDir: dir}, runner, api
}

// ownerMessage builds an inbound DM from the owner (General).
func ownerMessage(text string) *TelegramMessage {
	return dmMessage(42, text)
}

// An edited message is how a mistyped command gets fixed on a phone, so a
// command edit runs — exactly once per edit, however often Telegram redelivers
// it — while an edited plain message still does nothing.
func TestEditedCommandIsDispatchedOnce(t *testing.T) {
	in, runner, api := testInstance(t)
	if err := saveConfig(in.config()); err != nil {
		t.Fatal(err)
	}

	edit := ownerMessage("/model sonnet")
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
	again := ownerMessage("/model opus")
	again.EditDate = 1700000060
	in.handleEditedMessage(again)
	if got := in.config().Model; got != "opus" {
		t.Errorf("a second edit of the same message did not run: model = %q", got)
	}

	// Plain text is still ignored: fixing a typo must not re-run a turn.
	plain := ownerMessage("watch the deploy")
	plain.EditDate = 1700000120
	in.handleEditedMessage(plain)
	if _, ok := runner.last(); ok {
		t.Error("an edited plain message enqueued a turn")
	}
	if got := len(api.since("createForumTopic")); got != 0 {
		t.Errorf("an edited plain message created %d bots", got)
	}
}

func TestTextInGeneralGoesToDispatcher(t *testing.T) {
	in, runner, api := testInstance(t)

	in.handleMessage(ownerMessage("watch the deploy\nand tell me when it is green"))

	if got := len(api.since("createForumTopic")); got != 0 {
		t.Fatalf("General must not open a new topic, created %d", got)
	}
	bots, err := liveBots(in.db)
	if err != nil || len(bots) != 1 {
		t.Fatalf("expected the General bot, got %d (%v)", len(bots), err)
	}
	b := bots[0]
	if b.TopicID != 0 || b.Name != generalBotName {
		t.Errorf("dispatcher = %+v, want name %s topic 0", b, generalBotName)
	}
	if want := filepath.Join(in.dataDir, "bots", generalBotName, "workspace"); b.Cwd != want {
		t.Errorf("cwd = %q, want %q", b.Cwd, want)
	}
	last, ok := runner.last()
	if !ok || last.BotID != b.ID || !strings.HasPrefix(last.Text, "watch the deploy") {
		t.Errorf("General did not get the message: %+v", last)
	}
}

func TestOwnerDMGoesToGeneral(t *testing.T) {
	in, runner, api := testInstance(t)

	in.handleMessage(dmMessage(42, "watch the deploy"))

	if got := len(api.since("createForumTopic")); got != 0 {
		t.Fatalf("a DM must not open a topic, created %d", got)
	}
	b, err := generalBot(in.db)
	if err != nil {
		t.Fatal(err)
	}
	last, ok := runner.last()
	if !ok || last.BotID != b.ID || last.Text != "watch the deploy" {
		t.Errorf("DM did not go to General: %+v", last)
	}
}

func TestSessionFromDMCreatesBackendWorker(t *testing.T) {
	in, runner, api := testInstance(t)
	in.handleMessage(dmMessage(42, "/session fix the login"))

	if got := len(api.since("createForumTopic")); got != 0 {
		t.Fatalf("/session must not create a topic, got %d", got)
	}
	last, ok := runner.last()
	if !ok {
		t.Fatal("nothing enqueued")
	}
	b, err := botByID(in.db, last.BotID)
	if err != nil {
		t.Fatal(err)
	}
	if isGeneralBot(b) {
		t.Fatal("/session must not enqueue on General")
	}
	if b.TopicID >= 0 {
		t.Fatalf("worker TopicID = %d, want < 0 (backend-only)", b.TopicID)
	}
	if last.Text != "fix the login" {
		t.Errorf("first turn = %q", last.Text)
	}
}

func TestGroupMessageIsIgnored(t *testing.T) {
	in, runner, api := testInstance(t)
	if _, err := in.createBot("deployer", ""); err != nil {
		t.Fatal(err)
	}
	before := len(api.since("sendMessage"))
	in.handleMessage(groupMessage(42, 1, "status?"))
	in.handleMessage(groupMessage(42, 31337, "hello?"))
	if _, ok := runner.last(); ok {
		t.Error("a group message must not enqueue a turn")
	}
	if got := len(api.since("sendMessage")) - before; got != 0 {
		t.Errorf("ccc replied %d times in a group, want 0", got)
	}
}

func TestNonOwnerIsIgnored(t *testing.T) {
	in, runner, api := testInstance(t)
	msg := ownerMessage("let me in")
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
	cb.Message = ownerMessage("")
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

	msg := ownerMessage("main")
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

func TestFreeTextDoesNotAnswerWaitingQuestion(t *testing.T) {
	in, runner, api := testInstance(t)
	b, err := in.createBot("asker", "")
	if err != nil {
		t.Fatal(err)
	}
	setBotStatus(in.db, b.ID, botWaiting)
	q := Question{BotID: b.ID, Question: "Deploy to prod?", OptionsJSON: `["ship it","hold"]`, AskedMessageID: 555}
	if err := in.db.Create(&q).Error; err != nil {
		t.Fatal(err)
	}

	in.handleMessage(ownerMessage("do the other thing"))

	var stored Question
	if err := in.db.First(&stored, q.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.AnsweredAt != nil {
		t.Fatalf("free text answered the ask: %+v", stored)
	}
	g, err := generalBot(in.db)
	if err != nil {
		t.Fatal(err)
	}
	last, ok := runner.last()
	if !ok || last.BotID != g.ID || last.Text != "do the other thing" {
		t.Fatalf("free text must go to General, got %+v ok=%v", last, ok)
	}
	joined := strings.Join(api.texts(""), "\n")
	if !strings.Contains(joined, "Pending decision") || !strings.Contains(joined, "Deploy to prod?") {
		t.Errorf("DM must list pending asks, got %q", joined)
	}
}

func TestFreeTextListsPendingAskButtons(t *testing.T) {
	in, _, api := testInstance(t)
	b, err := in.createBot("ads", "")
	if err != nil {
		t.Fatal(err)
	}
	q := Question{BotID: b.ID, Question: "Raise the bid?", OptionsJSON: `["yes","no"]`, AskedMessageID: 9}
	if err := in.db.Create(&q).Error; err != nil {
		t.Fatal(err)
	}

	in.handleMessage(ownerMessage("hola"))

	kb := lastKeyboard(t, api)
	if len(kb) != 1 || len(kb[0]) != 2 {
		t.Fatalf("pending list keyboard = %+v", kb)
	}
	if kb[0][0].CallbackData != fmt.Sprintf("q:%d:0", q.ID) {
		t.Errorf("button callback = %q", kb[0][0].CallbackData)
	}
	if kb[0][0].Text != "yes" || kb[0][1].Text != "no" {
		t.Errorf("single pending list should use option labels, got %+v", kb[0])
	}
}

func TestQuestionCallbackFansOutToSimilar(t *testing.T) {
	in, runner, _ := testInstance(t)
	a, err := in.createBot("one", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := in.createBot("two", "")
	if err != nil {
		t.Fatal(err)
	}
	opts, err := json.Marshal([]string{"ship it", "hold"})
	if err != nil {
		t.Fatal(err)
	}
	q1 := Question{BotID: a.ID, Question: "Deploy to prod?", OptionsJSON: string(opts), AskedMessageID: 1}
	q2 := Question{BotID: b.ID, Question: "deploy to prod", OptionsJSON: string(opts), AskedMessageID: 2}
	if err := in.db.Create(&q1).Error; err != nil {
		t.Fatal(err)
	}
	if err := in.db.Create(&q2).Error; err != nil {
		t.Fatal(err)
	}

	cb := &CallbackQuery{ID: "cb1", Data: fmt.Sprintf("q:%d:0", q1.ID)}
	cb.From.ID = 42
	cb.Message = ownerMessage("")
	cb.Message.MessageID = 1
	in.handleCallback(cb)

	var first, second Question
	if err := in.db.First(&first, q1.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := in.db.First(&second, q2.ID).Error; err != nil {
		t.Fatal(err)
	}
	if first.AnsweredAt == nil || first.Answer != "ship it" {
		t.Fatalf("tapped question not answered: %+v", first)
	}
	if second.AnsweredAt == nil || second.Answer != "ship it" {
		t.Fatalf("similar question not answered: %+v", second)
	}
	runner.mu.Lock()
	n := len(runner.enqueued)
	runner.mu.Unlock()
	if n != 2 {
		t.Errorf("want 2 answer turns, got %d: %+v", n, runner.enqueued)
	}
}

func TestQuestionCallbackSkipsDissimilar(t *testing.T) {
	in, _, _ := testInstance(t)
	a, _ := in.createBot("one", "")
	b, _ := in.createBot("two", "")
	q1 := Question{BotID: a.ID, Question: "Deploy?", OptionsJSON: `["ship","hold"]`}
	q2 := Question{BotID: b.ID, Question: "Archive the session?", OptionsJSON: `["ship","hold"]`}
	if err := in.db.Create(&q1).Error; err != nil {
		t.Fatal(err)
	}
	if err := in.db.Create(&q2).Error; err != nil {
		t.Fatal(err)
	}

	cb := &CallbackQuery{ID: "cb", Data: fmt.Sprintf("q:%d:0", q1.ID)}
	cb.From.ID = 42
	cb.Message = ownerMessage("")
	in.handleCallback(cb)

	var other Question
	if err := in.db.First(&other, q2.ID).Error; err != nil {
		t.Fatal(err)
	}
	if other.AnsweredAt != nil {
		t.Fatalf("dissimilar question was answered: %+v", other)
	}
}

func TestQuestionsSimilarNormalized(t *testing.T) {
	a := &Question{ID: 1, Question: "Deploy to prod?"}
	b := &Question{ID: 2, Question: "deploy to prod"}
	if !questionsSimilar(a, b) {
		t.Fatal("same question with different punctuation must match")
	}
	c := &Question{ID: 3, Question: "Archive it?"}
	if questionsSimilar(a, c) {
		t.Fatal("different questions must not match")
	}
	if _, ok := mapQuestionAnswer(&Question{OptionsJSON: `["Yes","No"]`}, "yes"); !ok {
		t.Fatal("option match is case-insensitive")
	}
	if _, ok := mapQuestionAnswer(&Question{OptionsJSON: `["Yes","No"]`}, "later"); ok {
		t.Fatal("unknown option must not map")
	}
}

func TestSendToBotDoesNotDumpIntoTelegram(t *testing.T) {
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
	if queued[0].Text != "please review PR 12" {
		t.Fatalf("full body must stay in the inbox: %+v", queued)
	}

	for _, c := range api.since("sendMessage") {
		if strings.Contains(c.Params.Get("text"), "please review PR 12") || strings.Contains(c.Params.Get("text"), "🤝") {
			t.Errorf("session traffic must not be posted to Telegram: %q", c.Params.Get("text"))
		}
	}
}

func TestQueueBotMessageDoesNotDumpIntoTelegram(t *testing.T) {
	in, _, api := testInstance(t)
	a, err := in.createBot("alpha", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.createBot("beta", ""); err != nil {
		t.Fatal(err)
	}

	if _, _, err := queueBotMessage(in.db, a, "beta", "please review PR 12", true); err != nil {
		t.Fatalf("queueBotMessage: %v", err)
	}

	for _, c := range api.since("sendMessage") {
		if strings.Contains(c.Params.Get("text"), "please review PR 12") || strings.Contains(c.Params.Get("text"), "🤝") {
			t.Errorf("session traffic must not be posted to Telegram: %q", c.Params.Get("text"))
		}
	}
}

func TestTellCommandUsesCCCBotID(t *testing.T) {
	in, _, api := testInstance(t)
	if err := saveConfig(in.config()); err != nil {
		t.Fatal(err)
	}
	a, err := in.createBot("alpha", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.createBot("beta", ""); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CCC_BOT_ID", fmt.Sprint(a.ID))
	t.Setenv("CCC_DB", dbPath(in.cfg))
	t.Setenv("CCC_CONFIG", getConfigPath())
	if err := runTellCommand([]string{"beta", "handoff the chrome brief"}); err != nil {
		t.Fatalf("ccc tell: %v", err)
	}

	var queued []InboxMessage
	if err := in.db.Where("to_bot_id <> ? AND from_bot_id = ?", a.ID, a.ID).Find(&queued).Error; err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0].Text != "handoff the chrome brief" || !queued[0].Wake {
		t.Fatalf("inbox row: %+v", queued)
	}
	for _, c := range api.since("sendMessage") {
		if strings.Contains(c.Params.Get("text"), "handoff the chrome brief") || strings.Contains(c.Params.Get("text"), "🤝") {
			t.Errorf("ccc tell must not dump the body into Telegram: %q", c.Params.Get("text"))
		}
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

func TestCommandsInDM(t *testing.T) {
	in, runner, api := testInstance(t)
	g, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	in.db.Model(&Bot{}).Where("id = ?", g.ID).Update("session_id", "abc-123")

	in.handleMessage(ownerMessage("/role ships the deploy"))
	after, _ := botByID(in.db, g.ID)
	if after.Role != "" {
		t.Errorf("/role must not set a role anymore: %q", after.Role)
	}
	if after.SessionID != "abc-123" {
		t.Error("/role must not rotate the session")
	}

	in.db.Model(&Bot{}).Where("id = ?", g.ID).Update("session_id", "def-456")
	in.handleMessage(ownerMessage("/new"))
	after, _ = botByID(in.db, g.ID)
	if after.SessionID != "" {
		t.Error("/new must clear the session id")
	}

	in.handleMessage(ownerMessage("/stop"))
	if len(runner.stops) != 1 || runner.stops[0] != g.ID {
		t.Errorf("/stop did not reach the runner: %v", runner.stops)
	}

	in.handleMessage(ownerMessage("/cwd /definitely/not/here"))
	after, _ = botByID(in.db, g.ID)
	if after.Cwd == "/definitely/not/here" {
		t.Error("/cwd accepted a directory that does not exist")
	}
	dir := t.TempDir()
	in.handleMessage(ownerMessage("/cwd " + dir))
	after, _ = botByID(in.db, g.ID)
	if after.Cwd != dir {
		t.Errorf("cwd = %q, want %q", after.Cwd, dir)
	}

	if err := upsertMemory(in.db, scopeUser, "", "deploy-target", "vps3", g.ID); err != nil {
		t.Fatal(err)
	}
	in.handleMessage(ownerMessage("/memory deploy"))
	found := false
	for _, txt := range api.texts("") {
		if strings.Contains(txt, "deploy-target") {
			found = true
		}
	}
	if !found {
		t.Error("/memory did not show the stored memory")
	}

	in.db.Model(&Bot{}).Where("id = ?", g.ID).Update("session_id", "eng-789")
	in.handleMessage(ownerMessage("/engine"))
	in.handleMessage(ownerMessage("/engine grok"))
	after, _ = botByID(in.db, g.ID)
	if after.Engine != engineGrok {
		t.Errorf("engine = %q after /engine grok", after.Engine)
	}
	if after.SessionID != "" {
		t.Error("/engine must rotate the session: the conversation id is per CLI")
	}
	in.handleMessage(ownerMessage("/engine not-a-cli"))
	after, _ = botByID(in.db, g.ID)
	if after.Engine != engineGrok {
		t.Error("a bad /engine argument must not change the stored engine")
	}

	in.handleMessage(ownerMessage("/forget user deploy-target"))
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
	in.handleMessage(ownerMessage("/sessions"))
	joined := strings.Join(api.texts(""), "\n")
	if !strings.Contains(joined, "• <b>alpha</b>") {
		t.Errorf("/sessions must stay a list, got %q", joined)
	}
	if strings.Contains(joined, "does alpha things") {
		t.Errorf("/sessions must not list leftover roles: %q", joined)
	}
	if len(api.since("pinChatMessage")) != 0 {
		t.Error("/sessions is a list reply, not the pinned live card")
	}
}

func TestDocumentIsSavedIntoTheBotInbox(t *testing.T) {
	in, runner, _ := testInstance(t)
	g, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}

	msg := ownerMessage("")
	msg.Document = &TelegramDocument{FileID: "file-1", FileName: "../../escape.txt"}
	msg.Caption = "read this"
	in.handleMessage(msg)

	last, ok := runner.last()
	if !ok {
		t.Fatal("nothing enqueued for the attachment")
	}
	wantDir := filepath.Join(g.Cwd, "inbox")
	if !strings.Contains(last.Text, wantDir) {
		t.Errorf("message %q does not point at the bot inbox %q", last.Text, wantDir)
	}
	if strings.Contains(last.Text, "..") {
		t.Errorf("path traversal survived sanitisation: %q", last.Text)
	}
}

func TestNameCommandOnGeneralStaysGeneral(t *testing.T) {
	in, _, api := testInstance(t)
	if _, err := in.ensureGeneralBot(); err != nil {
		t.Fatal(err)
	}
	in.handleMessage(ownerMessage("/name"))
	joined := strings.Join(api.texts(""), "\n")
	if !strings.Contains(joined, "General stays General") && !strings.Contains(joined, generalBotName) {
		t.Errorf("/name in the DM should mention General: %q", joined)
	}
	in.handleMessage(ownerMessage("/name shipper"))
	g, err := generalBot(in.db)
	if err != nil {
		t.Fatal(err)
	}
	if g.Name != generalBotName {
		t.Errorf("General was renamed to %q", g.Name)
	}
}

func TestTellSessionFollowsTheRename(t *testing.T) {
	in, _, _ := testInstance(t)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	target, err := in.createBot("beta", "")
	if err != nil {
		t.Fatal(err)
	}

	s := &mcpServer{db: in.db, config: in.cfg, botID: target.ID}
	res, _, err := s.setName(t.Context(), nil, setNameIn{Name: "gamma"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("set_name failed: %+v", res.Content)
	}

	s = &mcpServer{db: in.db, config: in.cfg, botID: chief.ID}
	res, _, err = s.tellSession(t.Context(), nil, tellSessionIn{Session: "gamma", Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Errorf("the new name should be addressable: %+v", res.Content)
	}
	res, _, err = s.tellSession(t.Context(), nil, tellSessionIn{Session: "beta", Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("the old name should no longer resolve")
	}
}

func TestSetNameToolRenamesTheSession(t *testing.T) {
	in, _, api := testInstance(t)
	b, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}
	s := &mcpServer{db: in.db, config: in.cfg, botID: b.ID}

	res, _, err := s.setName(t.Context(), nil, setNameIn{Name: "shipper"})
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
	if len(api.since("editForumTopic")) != 0 {
		t.Error("set_name must not call Telegram forum APIs")
	}

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

// A session created from General goes straight to the engine with the owner's
// text. No role interview, no /role, no update_instructions.
func TestNewSessionDispatchesThePromptStraightToTheEngine(t *testing.T) {
	in, runner, api := testInstance(t)
	in.handleMessage(ownerMessage("/session help me with the deploy"))

	if len(api.since("createForumTopic")) != 0 {
		t.Fatalf("new sessions must not create a Telegram topic, got %d", len(api.since("createForumTopic")))
	}
	last, ok := runner.last()
	if !ok {
		t.Fatal("nothing enqueued for the new session")
	}
	if last.Text != "help me with the deploy" || last.Source != sourceUser {
		t.Errorf("first turn = %+v, want the owner's exact prompt", last)
	}
	b, err := botByID(in.db, last.BotID)
	if err != nil {
		t.Fatal(err)
	}
	if isGeneralBot(b) {
		t.Fatal("/session must not enqueue on General")
	}
	envelope := buildEnvelope(in.db, b, last.Source, last.Text, time.Now())
	if !strings.Contains(envelope, "help me with the deploy") {
		t.Errorf("the first turn is missing the owner's prompt:\n%s", envelope)
	}
	for _, banned := range []string{"no role yet", "update_instructions", "what you should be responsible", "/role"} {
		if strings.Contains(envelope, banned) {
			t.Errorf("the first turn still has role ceremony (%q):\n%s", banned, envelope)
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

	in.handleMessage(ownerMessage("/usage"))
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
	seedMemories(t, in.db, memCompactMaxCount+10)
	rep := runMaintenance(in.db, in.cfg, maintenanceDeps{Turner: answering(compactionAnswer(60))}, time.Now())
	if len(rep.Compactions) != 1 {
		t.Fatalf("no compaction to work with: %v", rep.Problems)
	}
	id := rep.Compactions[0].CompactionID

	in.handleMessage(ownerMessage("/memory stats"))
	last := func() string {
		texts := api.texts("")
		return texts[len(texts)-1]
	}
	if !strings.Contains(last(), "user memories") {
		t.Errorf("/memory stats = %q", last())
	}

	// A non-owner may look, but not undo.
	if err := in.db.Create(&Access{TelegramUserID: 4242, State: accessApproved}).Error; err != nil {
		t.Fatal(err)
	}
	in.handleMessage(dmMessage(4242, fmt.Sprintf("/memory restore %d", id)))
	if !strings.Contains(last(), "owner-only") {
		t.Errorf("restore is owner-only: %q", last())
	}

	in.handleMessage(ownerMessage(fmt.Sprintf("/memory restore %d", id)))
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

	in.handleMessage(ownerMessage("/set debounce_ms 800"))

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
