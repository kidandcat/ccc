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
	old := telegramBase()
	setTelegramBaseURL(f.URL)
	t.Cleanup(func() {
		setTelegramBaseURL(old)
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
	// running, when non-nil, makes Stop return true only for those ids
	// (and clears the flag). Nil keeps the old "always true" behaviour.
	running map[int64]bool
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
	defer f.mu.Unlock()
	f.stops = append(f.stops, botID)
	if f.db != nil {
		f.db.Model(&Turn{}).Where("bot_id = ? AND status = ?", botID, turnQueued).
			Updates(map[string]any{"status": turnFailed, "stop_reason": "dropped by /stop"})
	}
	if f.running == nil {
		return true
	}
	if f.running[botID] {
		delete(f.running, botID)
		return true
	}
	return false
}

func (f *fakeRunner) setRunning(id int64, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running == nil {
		f.running = map[int64]bool{}
	}
	if on {
		f.running[id] = true
	} else {
		delete(f.running, id)
	}
}

func (f *fakeRunner) last() (fakeTurn, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.enqueued) == 0 {
		return fakeTurn{}, false
	}
	return f.enqueued[len(f.enqueued)-1], true
}

func (f *fakeRunner) queued() []fakeTurn {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeTurn, len(f.enqueued))
	copy(out, f.enqueued)
	return out
}

func waitEnqueued(t *testing.T, r *fakeRunner, n int) []fakeTurn {
	t.Helper()
	var got []fakeTurn
	waitUntil(t, 2*time.Second, func() bool {
		got = r.queued()
		return len(got) >= n
	})
	return got
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
	in, runner, api := testInstance(t)
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
	assertCallbackAck(t, api, "ship it")
	assertMessageRetired(t, api, 555, "ship it")
}

func TestPendingListCallbackAnswersAndRetiresCard(t *testing.T) {
	in, runner, api := testInstance(t)
	b, err := in.createBot("notion-calendar-menubar", "")
	if err != nil {
		t.Fatal(err)
	}
	setBotStatus(in.db, b.ID, botWaiting)
	opts, err := json.Marshal([]string{"Sí, ya se ve", "No, sigue sin verse", "Se ve pero falta sitio para otros", skipOptionLabel})
	if err != nil {
		t.Fatal(err)
	}
	q := Question{BotID: b.ID, Question: "¿aparece ya el icono de Notion Calendar?", OptionsJSON: string(opts), AskedMessageID: 555}
	if err := in.db.Create(&q).Error; err != nil {
		t.Fatal(err)
	}

	in.handleMessage(ownerMessage("hola"))
	kb := lastKeyboard(t, api)
	if len(kb) != 1 || len(kb[0]) != 4 || kb[0][0].Text != "1" {
		t.Fatalf("pending list keyboard = %+v", kb)
	}

	cb := &CallbackQuery{ID: "tap", Data: kb[0][0].CallbackData}
	cb.From.ID = 42
	cb.Message = ownerMessage("")
	cb.Message.MessageID = int(api.nextMsg)
	in.handleCallback(cb)

	var stored Question
	if err := in.db.First(&stored, q.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.AnsweredAt == nil || stored.Answer != "Sí, ya se ve" {
		t.Fatalf("pending-list tap did not answer: %+v", stored)
	}
	last, ok := runner.last()
	if !ok || last.BotID != b.ID || !strings.Contains(last.Text, "Sí, ya se ve") {
		t.Errorf("answer was not fed back: %+v", last)
	}
	assertCallbackAck(t, api, "Sí, ya se ve")
	assertMessageRetired(t, api, 555, "Sí, ya se ve")
	assertMessageRetired(t, api, int64(cb.Message.MessageID), "Sí, ya se ve")
}

func TestQuestionCallbackAlreadyAnsweredRetiresButtons(t *testing.T) {
	in, _, api := testInstance(t)
	b, err := in.createBot("asker", "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	q := Question{BotID: b.ID, Question: "Deploy?", OptionsJSON: `["ship it","hold"]`, AskedMessageID: 7, Answer: "ship it", AnsweredAt: &now}
	if err := in.db.Create(&q).Error; err != nil {
		t.Fatal(err)
	}

	cb := &CallbackQuery{ID: "again", Data: fmt.Sprintf("q:%d:0", q.ID)}
	cb.From.ID = 42
	cb.Message = ownerMessage("")
	cb.Message.MessageID = 9
	in.handleCallback(cb)

	assertCallbackAck(t, api, "Already answered")
	assertMessageRetired(t, api, 7, "ship it")
	assertMessageRetired(t, api, 9, "ship it")
}

func TestLivePendingAsksSkipsDisabled(t *testing.T) {
	in, _, _ := testInstance(t)
	live, err := in.createBot("live", "")
	if err != nil {
		t.Fatal(err)
	}
	dead, err := in.createBot("dead", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := in.db.Model(dead).Update("status", botDisabled).Error; err != nil {
		t.Fatal(err)
	}
	for _, row := range []Question{
		{BotID: dead.ID, Question: "stale?", OptionsJSON: `["yes","Omitir"]`},
		{BotID: live.ID, Question: "live?", OptionsJSON: `["yes","Omitir"]`},
	} {
		if err := in.db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	asks := livePendingAsks(in.db)
	if len(asks) != 1 || asks[0].Name != "live" {
		t.Fatalf("live pending = %+v, want only the live session", asks)
	}
}

func assertCallbackAck(t *testing.T, api *fakeBotAPI, want string) {
	t.Helper()
	acks := api.since("answerCallbackQuery")
	if len(acks) == 0 {
		t.Fatal("no answerCallbackQuery")
	}
	got := acks[len(acks)-1].Params.Get("text")
	if got != want {
		t.Errorf("callback ack = %q, want %q", got, want)
	}
}

func assertMessageRetired(t *testing.T, api *fakeBotAPI, messageID int64, wantText string) {
	t.Helper()
	id := fmt.Sprintf("%d", messageID)
	for _, e := range api.since("editMessageText") {
		if e.Params.Get("message_id") != id {
			continue
		}
		if !strings.Contains(e.Params.Get("text"), wantText) {
			t.Errorf("edit %s text = %q, want substring %q", id, e.Params.Get("text"), wantText)
		}
		raw := e.Params.Get("reply_markup")
		if !strings.Contains(raw, `"inline_keyboard":[]`) {
			t.Errorf("edit %s did not remove the keyboard: %q", id, raw)
		}
		return
	}
	t.Errorf("no editMessageText for message %s", id)
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
	if kb[0][0].Text != "1" || kb[0][1].Text != "2" {
		t.Errorf("pending list buttons should be numbers, got %+v", kb[0])
	}
	joined := strings.Join(api.texts(""), "\n")
	for _, want := range []string{"1. yes", "2. no"} {
		if !strings.Contains(joined, want) {
			t.Errorf("pending list body missing %q, got %q", want, joined)
		}
	}
}

func TestRenderPendingAsksNumbersMultipleSessions(t *testing.T) {
	// The owner's screenshot: two sessions, long names, every option prefixed
	// onto one row so Telegram truncated them all to "notion-cale...".
	a := pendingAsk{
		Name: "notion-calendar-menubar",
		Q: Question{
			ID:          11,
			Question:    "¿me dejas quitar iconos de Centro de Control?",
			OptionsJSON: `["Quitar Bluetooth, Pantalla, Sonido","Dejarlo","Otra idea","Omitir"]`,
		},
	}
	b := pendingAsk{
		Name: "General",
		Q: Question{
			ID:          22,
			Question:    "¿aviso en el hilo de Slack o lo dejo?",
			OptionsJSON: `["Avísale en el hilo","Déjalo, y no avises","Omitir"]`,
		},
	}
	body, rows := renderPendingAsks([]pendingAsk{a, b}, 2, 0)

	if !strings.Contains(body, "❓ 2 pending decisions") {
		t.Errorf("header = %q", body)
	}
	for _, want := range []string{
		"<b>notion-calendar-menubar</b>",
		"<b>General</b>",
		"1. Quitar Bluetooth, Pantalla, Sonido",
		"2. Dejarlo",
		"3. Otra idea",
		"4. Omitir",
		"5. Avísale en el hilo",
		"6. Déjalo, y no avises",
		"7. Omitir",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "notion-calendar-menubar:") {
		t.Errorf("options must not be prefixed with the session name:\n%s", body)
	}
	if len(rows) != 2 || len(rows[0]) != 4 || len(rows[1]) != 3 {
		t.Fatalf("keyboard = %+v", rows)
	}
	wantLabels := [][]string{{"1", "2", "3", "4"}, {"5", "6", "7"}}
	wantData := [][]string{
		{"q:11:0", "q:11:1", "q:11:2", "q:11:3"},
		{"q:22:0", "q:22:1", "q:22:2"},
	}
	for i, wantRow := range wantLabels {
		for j, want := range wantRow {
			if rows[i][j].Text != want {
				t.Errorf("button [%d][%d] = %q, want %q", i, j, rows[i][j].Text, want)
			}
			if rows[i][j].CallbackData != wantData[i][j] {
				t.Errorf("callback [%d][%d] = %q, want %q", i, j, rows[i][j].CallbackData, wantData[i][j])
			}
			if strings.Contains(rows[i][j].Text, "notion") || strings.Contains(rows[i][j].Text, "General") {
				t.Errorf("button still carries a session name: %q", rows[i][j].Text)
			}
		}
	}
}

func TestRenderPendingAsksExtraFooter(t *testing.T) {
	item := pendingAsk{Name: "ads", Q: Question{ID: 1, Question: "Raise?", OptionsJSON: `["yes","Omitir"]`}}
	body, rows := renderPendingAsks([]pendingAsk{item}, 9, 8)
	if !strings.Contains(body, "❓ 9 pending decisions") || !strings.Contains(body, "and 8 more") {
		t.Errorf("extra footer = %q", body)
	}
	if len(rows) != 1 || rows[0][0].Text != "1" || rows[0][1].Text != "2" {
		t.Errorf("keyboard = %+v", rows)
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
	mapped, ok := mapQuestionAnswer(&Question{OptionsJSON: `["Yes","No"]`}, "skip")
	if !ok || mapped != skipOptionLabel {
		t.Fatalf("skip on a sibling without Omitir must still map, got %q ok=%v", mapped, ok)
	}
	mapped, ok = mapQuestionAnswer(&Question{OptionsJSON: `["Yes","No","Omitir"]`}, "SKIP")
	if !ok || mapped != skipOptionLabel {
		t.Fatalf("skip should match the Omitir button, got %q ok=%v", mapped, ok)
	}
}

func TestEnsureSkipOptionAlwaysLast(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{nil, []string{skipOptionLabel}},
		{[]string{}, []string{skipOptionLabel}},
		{[]string{"yes", "no"}, []string{"yes", "no", skipOptionLabel}},
		{[]string{"a", "b", "c"}, []string{"a", "b", "c", skipOptionLabel}},
		{[]string{"a", "b", "c", "d"}, []string{"a", "b", "c", skipOptionLabel}},
		{[]string{"yes", "Omitir", "no"}, []string{"yes", "no", skipOptionLabel}},
		{[]string{"Skip", "yes"}, []string{"yes", skipOptionLabel}},
		{[]string{skipOptionLabel}, []string{skipOptionLabel}},
	}
	for _, tc := range cases {
		got := ensureSkipOption(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("ensureSkipOption(%v) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("ensureSkipOption(%v) = %v, want %v", tc.in, got, tc.want)
				break
			}
		}
		if got[len(got)-1] != skipOptionLabel {
			t.Errorf("last option must be Omitir, got %v", got)
		}
		if len(got) > maxQuestionOptions {
			t.Errorf("too many options: %v", got)
		}
	}
}

func TestAskOwnerAlwaysAddsOmitir(t *testing.T) {
	in, _, api := testInstance(t)
	b, err := in.createBot("deployer", "")
	if err != nil {
		t.Fatal(err)
	}
	s := &mcpServer{db: in.db, config: in.cfg, botID: b.ID}
	res, _, err := s.askOwner(t.Context(), nil, askOwnerIn{
		Question: "Deploy to prod?",
		Options:  []string{"ship it", "hold"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("ask_owner failed: %+v", res.Content)
	}
	var q Question
	if err := in.db.Where("bot_id = ?", b.ID).First(&q).Error; err != nil {
		t.Fatal(err)
	}
	opts := questionOptions(&q)
	if len(opts) != 3 || opts[0] != "ship it" || opts[1] != "hold" || opts[2] != skipOptionLabel {
		t.Fatalf("stored options = %v, want ship it / hold / Omitir", opts)
	}
	kb := lastKeyboard(t, api)
	if len(kb) != 3 || kb[2][0].Text != skipOptionLabel {
		t.Fatalf("keyboard = %+v, want Omitir last", kb)
	}
}

func TestAskOwnerReplacesFourthWithOmitir(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("deployer", "")
	if err != nil {
		t.Fatal(err)
	}
	s := &mcpServer{db: in.db, config: in.cfg, botID: b.ID}
	if _, _, err := s.askOwner(t.Context(), nil, askOwnerIn{
		Question: "Which host?",
		Options:  []string{"vps2", "vps3", "fecha", "mac"},
	}); err != nil {
		t.Fatal(err)
	}
	var q Question
	if err := in.db.Where("bot_id = ?", b.ID).First(&q).Error; err != nil {
		t.Fatal(err)
	}
	opts := questionOptions(&q)
	if len(opts) != 4 || opts[3] != skipOptionLabel || opts[2] != "fecha" {
		t.Fatalf("options = %v, want first 3 kept and Omitir last", opts)
	}
	for _, o := range opts {
		if o == "mac" {
			t.Fatal("the 4th content option must be replaced by Omitir")
		}
	}
}

func TestAskOwnerSkipUnblocksWithoutChoosing(t *testing.T) {
	in, runner, api := testInstance(t)
	b, err := in.createBot("deployer", "")
	if err != nil {
		t.Fatal(err)
	}
	setBotStatus(in.db, b.ID, botWaiting)
	s := &mcpServer{db: in.db, config: in.cfg, botID: b.ID}
	if _, _, err := s.askOwner(t.Context(), nil, askOwnerIn{
		Question: "Deploy to prod?",
		Options:  []string{"ship it", "hold"},
	}); err != nil {
		t.Fatal(err)
	}
	var q Question
	if err := in.db.Where("bot_id = ?", b.ID).First(&q).Error; err != nil {
		t.Fatal(err)
	}
	kb := lastKeyboard(t, api)
	if len(kb) < 1 {
		t.Fatal("no keyboard")
	}
	skip := kb[len(kb)-1][0]
	if skip.Text != skipOptionLabel {
		t.Fatalf("last button = %q", skip.Text)
	}
	cb := &CallbackQuery{ID: "skip", Data: skip.CallbackData}
	cb.From.ID = 42
	cb.Message = ownerMessage("")
	in.handleCallback(cb)

	if err := in.db.First(&q, q.ID).Error; err != nil {
		t.Fatal(err)
	}
	if q.AnsweredAt == nil || !isSkipOption(q.Answer) {
		t.Fatalf("skip did not close the question: %+v", q)
	}
	last, ok := runner.last()
	if !ok || last.BotID != b.ID {
		t.Fatal("skip must unblock the worker")
	}
	if strings.Contains(last.Text, "ship it") || strings.Contains(last.Text, "hold") {
		t.Errorf("skip must not pick A or B: %q", last.Text)
	}
	if !strings.Contains(last.Text, "skipped") || !strings.Contains(last.Text, "without choosing") {
		t.Errorf("skip envelope = %q", last.Text)
	}
	after, err := botByID(in.db, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != botIdle {
		t.Errorf("bot status = %q, want idle after skip", after.Status)
	}
}

func TestSkipFansOutToSimilarWithoutOmitirButton(t *testing.T) {
	in, runner, _ := testInstance(t)
	a, err := in.createBot("one", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := in.createBot("two", "")
	if err != nil {
		t.Fatal(err)
	}
	opts, err := json.Marshal([]string{"ship it", "hold", skipOptionLabel})
	if err != nil {
		t.Fatal(err)
	}
	q1 := Question{BotID: a.ID, Question: "Deploy to prod?", OptionsJSON: string(opts), AskedMessageID: 1}
	q2 := Question{BotID: b.ID, Question: "deploy to prod", OptionsJSON: `["ship it","hold"]`, AskedMessageID: 2}
	if err := in.db.Create(&q1).Error; err != nil {
		t.Fatal(err)
	}
	if err := in.db.Create(&q2).Error; err != nil {
		t.Fatal(err)
	}

	cb := &CallbackQuery{ID: "cb", Data: fmt.Sprintf("q:%d:2", q1.ID)}
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
	if first.AnsweredAt == nil || !isSkipOption(first.Answer) {
		t.Fatalf("tapped skip not recorded: %+v", first)
	}
	if second.AnsweredAt == nil || !isSkipOption(second.Answer) {
		t.Fatalf("similar question must be skipped too: %+v", second)
	}
	runner.mu.Lock()
	n := len(runner.enqueued)
	runner.mu.Unlock()
	if n != 2 {
		t.Errorf("want 2 skip turns, got %d: %+v", n, runner.enqueued)
	}
}

func TestAskOwnerFreeTextStillGetsOmitir(t *testing.T) {
	in, _, api := testInstance(t)
	b, err := in.createBot("writer", "")
	if err != nil {
		t.Fatal(err)
	}
	s := &mcpServer{db: in.db, config: in.cfg, botID: b.ID}
	if _, _, err := s.askOwner(t.Context(), nil, askOwnerIn{Question: "What should the title be?"}); err != nil {
		t.Fatal(err)
	}
	var q Question
	if err := in.db.Where("bot_id = ?", b.ID).First(&q).Error; err != nil {
		t.Fatal(err)
	}
	opts := questionOptions(&q)
	if len(opts) != 1 || opts[0] != skipOptionLabel {
		t.Fatalf("free-text options = %v, want only Omitir", opts)
	}
	joined := strings.Join(api.texts(""), "\n")
	if !strings.Contains(joined, "Reply to this message") {
		t.Errorf("free-text question must keep the reply hint, got %q", joined)
	}
	kb := lastKeyboard(t, api)
	if len(kb) != 1 || kb[0][0].Text != skipOptionLabel {
		t.Fatalf("free-text keyboard = %+v", kb)
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

func TestStopCommandReachesWorkers(t *testing.T) {
	in, runner, api := testInstance(t)
	g, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	deployer, err := in.createBot("deployer", "")
	if err != nil {
		t.Fatal(err)
	}
	analyst, err := in.createBot("analyst", "")
	if err != nil {
		t.Fatal(err)
	}

	in.db.Model(&Bot{}).Where("id = ?", deployer.ID).Update("status", botRunning)
	runner.setRunning(deployer.ID, true)
	in.handleMessage(ownerMessage("/stop"))
	if len(runner.stops) < 2 || runner.stops[0] != g.ID || runner.stops[1] != deployer.ID {
		t.Errorf("bare /stop with idle General = %v, want [General, deployer, …]", runner.stops)
	}
	joined := strings.Join(api.texts(""), "\n")
	if !strings.Contains(joined, "deployer") || !strings.Contains(joined, "🛑") {
		t.Errorf("reply missing deployer stop:\n%s", joined)
	}
	var after Bot
	in.db.First(&after, deployer.ID)
	if after.Status != botIdle {
		t.Errorf("deployer status = %s, want idle", after.Status)
	}

	runner.stops = nil
	runner.setRunning(g.ID, true)
	runner.setRunning(deployer.ID, true)
	in.db.Model(&Bot{}).Where("id IN ?", []int64{g.ID, deployer.ID}).Update("status", botRunning)
	in.handleMessage(ownerMessage("/stop"))
	if len(runner.stops) != 1 || runner.stops[0] != g.ID {
		t.Errorf("both running: stops = %v, want only General", runner.stops)
	}
	joined = strings.Join(api.texts(""), "\n")
	if !strings.Contains(joined, "Still running") || !strings.Contains(strings.ToLower(joined), "deployer") ||
		!strings.Contains(joined, "/stop all") {
		t.Errorf("want Still running + /stop all:\n%s", joined)
	}

	runner.stops = nil
	runner.setRunning(deployer.ID, true)
	runner.setRunning(analyst.ID, true)
	in.db.Model(&Bot{}).Where("id IN ?", []int64{deployer.ID, analyst.ID}).Update("status", botRunning)
	in.handleMessage(ownerMessage("/stop DEPLOYER"))
	if len(runner.stops) != 1 || runner.stops[0] != deployer.ID {
		t.Errorf("/stop DEPLOYER stops = %v", runner.stops)
	}
	joined = strings.Join(api.texts(""), "\n")
	if !strings.Contains(joined, "analyst") || !strings.Contains(joined, "Still running") {
		t.Errorf("should list analyst still running:\n%s", joined)
	}

	runner.stops = nil
	runner.setRunning(g.ID, true)
	runner.setRunning(deployer.ID, true)
	runner.setRunning(analyst.ID, true)
	in.db.Model(&Bot{}).Where("id IN ?", []int64{g.ID, deployer.ID, analyst.ID}).Update("status", botRunning)
	beforeAll := len(api.texts(""))
	in.handleMessage(ownerMessage("/stop all"))
	if len(runner.stops) != 3 {
		t.Errorf("/stop all stops = %v, want 3", runner.stops)
	}
	allReply := strings.Join(api.texts("")[beforeAll:], "\n")
	if strings.Contains(allReply, "Still running") {
		t.Errorf("/stop all should not say Still running:\n%s", allReply)
	}

	runner.stops = nil
	in.handleMessage(ownerMessage("/stop nope"))
	if len(runner.stops) != 0 {
		t.Errorf("unknown name stopped %v", runner.stops)
	}
	if !strings.Contains(strings.Join(api.texts(""), "\n"), "No live session") {
		t.Error("unknown name should say No live session")
	}

	runner.stops = nil
	in.db.Model(&Bot{}).Where("id IN ?", []int64{g.ID, deployer.ID, analyst.ID}).Update("status", botIdle)
	in.handleMessage(ownerMessage("/stop"))
	if !strings.Contains(strings.Join(api.texts(""), "\n"), "Nothing was running") {
		t.Errorf("quiet /stop:\n%s", strings.Join(api.texts(""), "\n"))
	}

	runner.stops = nil
	in.db.Model(&Bot{}).Where("id = ?", analyst.ID).Update("status", botWaiting)
	if err := in.db.Create(&Turn{BotID: deployer.ID, Source: sourceUser, Input: "x", Status: turnQueued}).Error; err != nil {
		t.Fatal(err)
	}
	in.handleMessage(ownerMessage("/stop all"))
	var a Bot
	in.db.First(&a, analyst.ID)
	if a.Status != botWaiting {
		t.Errorf("waiting analyst became %s", a.Status)
	}
	joined = strings.ToLower(strings.Join(api.texts(""), "\n"))
	if !strings.Contains(joined, "dropped") || !strings.Contains(joined, "1 queued") {
		t.Errorf("want dropped 1 queued:\n%s", joined)
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
	allowUsers(in, 4242)
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
