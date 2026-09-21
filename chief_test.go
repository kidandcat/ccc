package main

import (
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestSpawnSessionIsChiefOnly(t *testing.T) {
	in, _, api := testInstance(t)
	worker, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}
	s := &mcpServer{db: in.db, config: in.cfg, botID: worker.ID}
	res, _, err := s.spawnSession(t.Context(), nil, spawnSessionIn{Prompt: "fix the deploy"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("a worker must not spawn sessions")
	}
	if n := len(api.since("createForumTopic")); n != 0 {
		t.Errorf("worker spawn created %d topics; sessions are backend-only", n)
	}

	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	s = &mcpServer{db: in.db, config: in.cfg, botID: chief.ID}
	res, _, err = s.spawnSession(t.Context(), nil, spawnSessionIn{Prompt: "fix the deploy"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("chief spawn failed: %+v", res.Content)
	}
	if n := len(api.since("createForumTopic")); n != 0 {
		t.Fatalf("spawn_session must not create a Telegram topic, got %d", n)
	}
	spawned, err := botByName(in.db, botNameFromText("fix the deploy"))
	if err != nil {
		t.Fatalf("spawned worker: %v", err)
	}
	if spawned.TopicID >= 0 {
		t.Fatalf("spawned worker should be backend-only (TopicID < 0), got %+v", spawned)
	}
	var queued []InboxMessage
	in.db.Where("from_bot_id = ?", chief.ID).Find(&queued)
	if len(queued) != 1 || queued[0].Text != "fix the deploy" || !queued[0].Wake {
		t.Fatalf("first prompt not queued: %+v", queued)
	}
	for _, c := range api.since("sendMessage") {
		if strings.Contains(c.Params.Get("text"), "fix the deploy") || strings.Contains(c.Params.Get("text"), "🤝") {
			t.Errorf("spawn_session must not dump the prompt into Telegram: %q", c.Params.Get("text"))
		}
	}
}

func TestTellSessionIsChiefOnly(t *testing.T) {
	in, _, api := testInstance(t)
	alpha, _ := in.createBot("alpha", "")
	beta, _ := in.createBot("beta", "")
	s := &mcpServer{db: in.db, config: in.cfg, botID: alpha.ID}
	res, _, err := s.tellSession(t.Context(), nil, tellSessionIn{Session: "beta", Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("workers must not tell each other")
	}
	var n int64
	in.db.Model(&InboxMessage{}).Where("to_bot_id = ?", beta.ID).Count(&n)
	if n != 0 {
		t.Fatalf("worker tell leaked an inbox row")
	}

	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	s = &mcpServer{db: in.db, config: in.cfg, botID: chief.ID}
	res, _, err = s.tellSession(t.Context(), nil, tellSessionIn{Session: "beta", Text: "ship it"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("chief tell failed: %+v", res.Content)
	}
	for _, c := range api.since("sendMessage") {
		if strings.Contains(c.Params.Get("text"), "ship it") || strings.Contains(c.Params.Get("text"), "🤝") {
			t.Errorf("tell_session must not dump the message into Telegram: %q", c.Params.Get("text"))
		}
	}
}

func TestReportToGeneralIsWorkerOnly(t *testing.T) {
	in, _, api := testInstance(t)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	s := &mcpServer{db: in.db, config: in.cfg, botID: chief.ID}
	res, _, err := s.reportToGeneral(t.Context(), nil, reportToGeneralIn{Text: "done"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("General must not report to itself")
	}

	worker, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}
	s = &mcpServer{db: in.db, config: in.cfg, botID: worker.ID}
	res, _, err = s.reportToGeneral(t.Context(), nil, reportToGeneralIn{Text: "build green"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("worker report failed: %+v", res.Content)
	}
	var queued []InboxMessage
	in.db.Where("to_bot_id = ?", chief.ID).Find(&queued)
	if len(queued) != 1 || queued[0].Text != "build green" || !queued[0].Wake || !queued[0].Relay {
		t.Fatalf("report inbox: %+v", queued)
	}
	for _, c := range api.since("sendMessage") {
		if strings.Contains(c.Params.Get("text"), "build green") || strings.Contains(c.Params.Get("text"), "🤝") {
			t.Errorf("report_to_general must not dump the report into Telegram: %q", c.Params.Get("text"))
		}
	}
}

func TestNotifyOwnerStillPostsToTelegram(t *testing.T) {
	in, _, api := testInstance(t)
	w, err := in.createBot("deployer", "")
	if err != nil {
		t.Fatal(err)
	}
	s := &mcpServer{db: in.db, config: in.cfg, botID: w.ID}
	res, _, err := s.notifyOwner(t.Context(), nil, notifyOwnerIn{Text: "deploy is down"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("notify_owner failed: %+v", res.Content)
	}
	found := false
	for _, c := range api.since("sendMessage") {
		if strings.Contains(c.Params.Get("text"), "deploy is down") {
			found = true
		}
	}
	if !found {
		t.Error("notify_owner must still reach the owner")
	}
}

func TestIdleRemindTextIsForGeneral(t *testing.T) {
	got := idleRemindText("chrome-profile-sync")
	if strings.Contains(got, "esperando") || strings.Contains(got, "⏳") {
		t.Errorf("idle nag is for General, not a Telegram ping: %q", got)
	}
	for _, want := range []string{"chrome-profile-sync", "ask_owner", "tell_session", "ignore"} {
		if !strings.Contains(got, want) {
			t.Errorf("idle nag missing %q: %q", want, got)
		}
	}
}

func TestOwnerSessionStatusOneLiner(t *testing.T) {
	if got, ok := ownerSessionStatus("", false); !ok || got != "done" {
		t.Errorf("idle success = %q ok=%v, want done", got, ok)
	}
	if got, ok := ownerSessionStatus("", true); !ok || got != "waiting" {
		t.Errorf("pending question = %q ok=%v, want waiting", got, ok)
	}
	if got, ok := ownerSessionStatus(errFatal, false); !ok || got != "error" {
		t.Errorf("failure = %q ok=%v, want error", got, ok)
	}
	if _, ok := ownerSessionStatus("stopped", false); ok {
		t.Error("/stop should not ping the owner")
	}
	if _, ok := ownerSessionStatus(chiefTimeoutClass, false); ok {
		t.Error("General timeout should not ping as a session status")
	}
	if got := ownerSessionStatusLine("deployer", "done"); got != "session deployer done" {
		t.Errorf("line = %q", got)
	}
}

func TestPostOwnerSessionStatusSkipsGeneral(t *testing.T) {
	in, _, _ := testInstance(t)
	ui := &fakeUI{}
	r := newRunner(in.db, in.cfg, ui)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	r.postOwnerSessionStatus(chief, "", false)
	if len(ui.posts) != 0 {
		t.Errorf("General must not post a session status card, got %v", ui.posts)
	}

	w, err := in.createBot("deployer", "")
	if err != nil {
		t.Fatal(err)
	}
	r.postOwnerSessionStatus(w, "", false)
	if len(ui.posts) != 1 || !ui.silentPosts[0] || !strings.Contains(ui.posts[0], "<b>deployer</b>") || !strings.Contains(ui.posts[0], "done") {
		t.Errorf("worker done = %v, want a silent done card", ui.posts)
	}
	setBotStatus(in.db, w.ID, botWaiting)
	r.postOwnerSessionStatus(w, "", true)
	got := ""
	if len(ui.edits) > 0 {
		got = ui.edits[len(ui.edits)-1]
	} else if len(ui.posts) > 0 {
		got = ui.posts[len(ui.posts)-1]
	}
	if !strings.Contains(got, "waiting") {
		t.Errorf("worker waiting = posts %v edits %v", ui.posts, ui.edits)
	}
	if len(ui.pins) == 0 {
		t.Errorf("waiting is work and must pin, pins=%v", ui.pins)
	}
	setBotStatus(in.db, w.ID, botIdle)
	r.postOwnerSessionStatus(w, errFatal, false)
	last := ui.edits[len(ui.edits)-1]
	if !strings.Contains(last, "error") {
		t.Errorf("worker error = %q", last)
	}
}

func TestChiefEnvelopeListsWorkersNotItself(t *testing.T) {
	in, _, _ := testInstance(t)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	w, err := in.createBot("deployer", "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	in.db.Create(&Turn{BotID: w.ID, Source: sourceUser, Input: "go", Output: "deployed fecha to vps3", Status: turnDone, EndedAt: &now})

	got := buildEnvelope(in.db, chief, sourceUser, "what's running?", time.Now())
	if !strings.Contains(got, "active sessions:") || !strings.Contains(got, "deployer") {
		t.Errorf("chief envelope missing roster:\n%s", got)
	}
	if !strings.Contains(got, "deployed fecha") {
		t.Errorf("chief envelope missing last output:\n%s", got)
	}
	if strings.Contains(got, generalBotName+" [") {
		t.Errorf("roster listed General:\n%s", got)
	}

	workerEnv := buildEnvelope(in.db, w, sourceUser, "hi", time.Now())
	if strings.Contains(workerEnv, "active sessions:") {
		t.Errorf("worker envelope must not list the roster:\n%s", workerEnv)
	}
}

func TestArchiveAndRenameRefuseGeneral(t *testing.T) {
	in, _, _ := testInstance(t)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	s := &mcpServer{db: in.db, config: in.cfg, botID: chief.ID}
	res, _, err := s.archiveBot(t.Context(), nil, archiveBotIn{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("General must not archive itself")
	}
	res, _, err = s.setName(t.Context(), nil, setNameIn{Name: "boss"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("General must not rename")
	}
	if err := archiveBotRow(in.db, chief.ID); err == nil {
		t.Fatal("archiveBotRow should refuse General")
	}
}

func TestChiefPromptIsByteStable(t *testing.T) {
	b := promptBot{Name: "General", Cwd: "/tmp", Chief: true}
	first := renderSystemPrompt(b, "host", []otherBot{{Name: "a"}})
	second := renderSystemPrompt(b, "host", []otherBot{{Name: "b"}})
	if first != second {
		t.Errorf("chief system prompt is not byte-stable")
	}
	if strings.Contains(first, botIdle) || strings.Contains(first, "deployer") {
		t.Error("live roster leaked into the chief system prompt")
	}
	if !strings.Contains(first, "spawn_session") || !strings.Contains(first, "dispatcher") {
		t.Errorf("chief prompt missing dispatcher tools:\n%s", first)
	}
	if !strings.Contains(first, "60 second") {
		t.Errorf("chief prompt must mention the 60s cap:\n%s", first)
	}
	if !strings.Contains(first, "ccc starts one") {
		t.Errorf("chief prompt must say ccc auto-spawns if General cannot:\n%s", first)
	}
	if !strings.Contains(first, "does not see those reports") {
		t.Errorf("chief prompt must not dump session reports to the owner:\n%s", first)
	}
	if !strings.Contains(first, "MUST reply in this DM") {
		t.Errorf("chief prompt must require a user-visible summary after a worker reports:\n%s", first)
	}
	if !strings.Contains(first, "do not crush") {
		t.Errorf("chief prompt must not crush structured worker digests:\n%s", first)
	}
	if !strings.Contains(first, "not under the") || !strings.Contains(first, "60s cap") {
		t.Errorf("chief prompt must say report turns are not 60s-capped:\n%s", first)
	}
	if !strings.Contains(first, "every 10 minutes") || !strings.Contains(first, "Do not notify_owner just to repeat the nag") {
		t.Errorf("chief prompt must teach idle nags are dispatcher-only:\n%s", first)
	}
	if !strings.Contains(first, "Do not re-ask a pending ask_owner") {
		t.Errorf("chief prompt must not re-ask pending decisions:\n%s", first)
	}
	if !strings.Contains(first, "MUST be a watch") || !strings.Contains(first, "fresh worker") {
		t.Errorf("chief prompt must teach watch-not-wakeup and isolated routines:\n%s", first)
	}
}

func TestCreateBotIsBackendOnly(t *testing.T) {
	in, _, api := testInstance(t)
	b, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}
	if b.TopicID >= 0 {
		t.Fatalf("new session TopicID = %d, want < 0", b.TopicID)
	}
	if isGeneralBot(b) {
		t.Fatalf("new session must be a backend worker: %+v", b)
	}
	if n := len(api.since("createForumTopic")); n != 0 {
		t.Errorf("createBot created %d forum topics", n)
	}
}

func TestDestForTopic(t *testing.T) {
	cfg := &Config{BotToken: "T", ChatID: 42}
	chat, thread, ok := destForTopic(cfg, 0)
	if !ok || chat != 42 || thread != 0 {
		t.Errorf("General dest = %d/%d ok=%v, want DM 42/0", chat, thread, ok)
	}
	_, _, ok = destForTopic(cfg, 7)
	if ok {
		t.Error("a leftover positive TopicID must have no Telegram dest")
	}
	_, _, ok = destForTopic(cfg, -3)
	if ok {
		t.Error("backend worker must have no Telegram dest")
	}
	cfg.ChatID = 0
	_, _, ok = destForTopic(cfg, 0)
	if ok {
		t.Error("General without ChatID has no Telegram dest")
	}
}

func TestChiefTimeoutIsSixtySecondsForGeneralOnly(t *testing.T) {
	owner := &Turn{Source: sourceUser, Input: "deploy fecha"}
	if d := chiefTimeoutFor(&Bot{TopicID: 0}, owner); d != 60*time.Second {
		t.Errorf("General timeout = %s, want 60s", d)
	}
	if d := chiefTimeoutFor(&Bot{TopicID: 1}, owner); d != 0 {
		t.Errorf("worker timeout = %s, want none", d)
	}
	if d := chiefTimeoutFor(nil, owner); d != 0 {
		t.Errorf("nil bot timeout = %s, want none", d)
	}
}

func TestChiefTimeoutSkipsSessionReports(t *testing.T) {
	chief := &Bot{TopicID: 0}
	report := &Turn{Source: sourceBot, Input: "Message from nota de voz:\ndone."}
	if d := chiefTimeoutFor(chief, report); d != 0 {
		t.Errorf("session report timeout = %s, want none so General can summarize", d)
	}
	idle := &Turn{Source: sourceBot, Input: idleRemindText("parked")}
	if d := chiefTimeoutFor(chief, idle); d != 0 {
		t.Errorf("idle nag timeout = %s, want none", d)
	}
	follow := &Turn{Source: sourceSystem, Input: chiefTimeoutInput()}
	if d := chiefTimeoutFor(chief, follow); d != 60*time.Second {
		t.Errorf("timeout follow-up = %s, want 60s", d)
	}
	if d := chiefTimeoutFor(chief, nil); d != 60*time.Second {
		t.Errorf("unknown General turn = %s, want 60s (fail closed)", d)
	}
}

func TestChiefTimeoutInputTellsGeneralToSpawn(t *testing.T) {
	got := chiefTimeoutInput()
	for _, want := range []string{"Error:", "too long for General", "spawn_session", "tell_session"} {
		if !strings.Contains(got, want) {
			t.Errorf("timeout input missing %q:\n%s", want, got)
		}
	}
	if !isChiefTimeoutFollowUp(got) {
		t.Error("the injected error must be recognised as a follow-up so it cannot loop")
	}
	if isChiefTimeoutFollowUp("please spawn a session for the deploy") {
		t.Error("an ordinary spawn request must not look like the timeout follow-up")
	}
}

func TestPersistChiefTimeoutEnqueuesTheInstruction(t *testing.T) {
	in, _, _ := testInstance(t)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	ui := &fakeUI{}
	r := newRunner(in.db, in.cfg, ui)
	row := &Turn{BotID: chief.ID, Source: sourceUser, Input: "fix fecha", Status: turnRunning}
	if err := in.db.Create(row).Error; err != nil {
		t.Fatal(err)
	}
	r.persistChiefTimeout(chief, row, "fix fecha", time.Now(), nil)

	var queued []Turn
	in.db.Where("bot_id = ? AND status = ?", chief.ID, turnQueued).Find(&queued)
	if len(queued) != 1 {
		t.Fatalf("queued %d turns, want the injected error", len(queued))
	}
	if queued[0].Source != sourceSystem {
		t.Errorf("source = %q, want system", queued[0].Source)
	}
	if !strings.Contains(queued[0].Input, "too long for General") {
		t.Errorf("injected input = %q", queued[0].Input)
	}

	var done Turn
	in.db.First(&done, row.ID)
	if done.Status != turnFailed || done.ErrorClass != chiefTimeoutClass {
		t.Errorf("timed-out turn = %+v, want failed/%s", done, chiefTimeoutClass)
	}
	var stillQueued int64
	in.db.Model(&Turn{}).Where("bot_id = ? AND status = ? AND id = ?", chief.ID, turnQueued, row.ID).Count(&stillQueued)
	if stillQueued != 0 {
		t.Error("the killed turn must not stay queued")
	}

	spawned := mustAutoSpawnedWorker(t, in.db, chief, "fix fecha")
	if !strings.Contains(queued[0].Input, spawned.Name) {
		t.Errorf("inject should name the auto-spawned session %q:\n%s", spawned.Name, queued[0].Input)
	}
	assertNoOwnerSessionNudge(t, ui)
	if !ownerPostsInclude(ui, spawned.Name) {
		t.Errorf("owner live card missing spawned session, posts=%v", ui.posts)
	}
}

func TestPersistChiefTimeoutDoesNotLoop(t *testing.T) {
	in, _, _ := testInstance(t)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	ui := &fakeUI{}
	r := newRunner(in.db, in.cfg, ui)
	orig := &Turn{BotID: chief.ID, Source: sourceUser, Input: "fix fecha", Status: turnFailed, ErrorClass: chiefTimeoutClass}
	if err := in.db.Create(orig).Error; err != nil {
		t.Fatal(err)
	}
	row := &Turn{BotID: chief.ID, Source: sourceSystem, Input: chiefTimeoutInput(), Status: turnRunning}
	if err := in.db.Create(row).Error; err != nil {
		t.Fatal(err)
	}
	r.persistChiefTimeout(chief, row, chiefTimeoutInput(), time.Now(), nil)

	var queued int64
	in.db.Model(&Turn{}).Where("bot_id = ? AND status = ?", chief.ID, turnQueued).Count(&queued)
	if queued != 0 {
		t.Errorf("follow-up timeout enqueued %d more turns; that would loop", queued)
	}
	mustAutoSpawnedWorker(t, in.db, chief, "fix fecha")
	assertNoOwnerSessionNudge(t, ui)
}

func TestPersistChiefTimeoutSkipsWhenAlreadyHandedOff(t *testing.T) {
	in, _, _ := testInstance(t)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	r := newRunner(in.db, in.cfg, &fakeUI{})
	started := time.Now().Add(-time.Second)
	row := &Turn{BotID: chief.ID, Source: sourceUser, Input: "fix fecha", Status: turnRunning, StartedAt: &started}
	if err := in.db.Create(row).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := startBackendSession(in.db, in.cfg, chief, "deployer", "fix fecha"); err != nil {
		t.Fatal(err)
	}
	r.persistChiefTimeout(chief, row, "fix fecha", time.Now(), nil)

	var workers int64
	in.db.Model(&Bot{}).Where("id != ? AND archived_at IS NULL", chief.ID).Count(&workers)
	if workers != 1 {
		t.Fatalf("already-handed-off timeout spawned extra workers: %d", workers)
	}
	var queued int64
	in.db.Model(&Turn{}).Where("bot_id = ? AND status = ?", chief.ID, turnQueued).Count(&queued)
	if queued != 0 {
		t.Errorf("General already spawned; should not inject, got %d queued turns", queued)
	}
}

func TestPersistChiefTimeoutFollowUpDoesNotDuplicate(t *testing.T) {
	in, _, _ := testInstance(t)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	r := newRunner(in.db, in.cfg, &fakeUI{})
	first := &Turn{BotID: chief.ID, Source: sourceUser, Input: "fix fecha", Status: turnRunning}
	if err := in.db.Create(first).Error; err != nil {
		t.Fatal(err)
	}
	r.persistChiefTimeout(chief, first, "fix fecha", time.Now(), nil)
	mustAutoSpawnedWorker(t, in.db, chief, "fix fecha")

	follow := &Turn{BotID: chief.ID, Source: sourceSystem, Input: chiefTimeoutInputAfterSpawn("fix fecha"), Status: turnRunning}
	if err := in.db.Create(follow).Error; err != nil {
		t.Fatal(err)
	}
	r.persistChiefTimeout(chief, follow, chiefTimeoutInputAfterSpawn("fix fecha"), time.Now(), nil)

	var workers int64
	in.db.Model(&Bot{}).Where("id != ? AND archived_at IS NULL", chief.ID).Count(&workers)
	if workers != 1 {
		t.Fatalf("follow-up timeout spawned a duplicate worker: %d", workers)
	}
}

func TestEnsureChiefTimeoutHandoffOnSuccessfulFollowUp(t *testing.T) {
	in, _, _ := testInstance(t)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	r := newRunner(in.db, in.cfg, &fakeUI{})
	orig := &Turn{BotID: chief.ID, Source: sourceUser, Input: "ship the fix", Status: turnFailed, ErrorClass: chiefTimeoutClass}
	if err := in.db.Create(orig).Error; err != nil {
		t.Fatal(err)
	}
	follow := &Turn{BotID: chief.ID, Source: sourceSystem, Input: chiefTimeoutInput(), Status: turnDone}
	if err := in.db.Create(follow).Error; err != nil {
		t.Fatal(err)
	}
	if spawned := r.ensureChiefTimeoutHandoff(chief, follow, chiefTimeoutInput()); spawned == nil {
		t.Fatal("follow-up without spawn_session must auto-spawn")
	}
	mustAutoSpawnedWorker(t, in.db, chief, "ship the fix")
}

func TestPersistChiefTimeoutIgnoresSessionReports(t *testing.T) {
	in, _, _ := testInstance(t)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.createBot("worker", ""); err != nil {
		t.Fatal(err)
	}
	// A recent owner request exists, but this timeout is a session report.
	orig := &Turn{BotID: chief.ID, Source: sourceUser, Input: "fix fecha", Status: turnDone}
	if err := in.db.Create(orig).Error; err != nil {
		t.Fatal(err)
	}
	ui := &fakeUI{}
	r := newRunner(in.db, in.cfg, ui)
	row := &Turn{BotID: chief.ID, Source: sourceBot, Input: "Message from worker:\ndone.", Status: turnRunning}
	if err := in.db.Create(row).Error; err != nil {
		t.Fatal(err)
	}
	r.persistChiefTimeout(chief, row, row.Input, time.Now(), nil)

	var workers int64
	in.db.Model(&Bot{}).Where("id != ? AND archived_at IS NULL AND name != ?", chief.ID, "worker").Count(&workers)
	if workers != 0 {
		t.Fatalf("session-report timeout spawned %d extra workers", workers)
	}
	var queued int64
	in.db.Model(&Turn{}).Where("bot_id = ? AND status = ?", chief.ID, turnQueued).Count(&queued)
	if queued != 0 {
		t.Errorf("session-report timeout must not inject a follow-up, got %d queued", queued)
	}
}

func TestLastOwnerRequestSkipsBotReports(t *testing.T) {
	in, _, _ := testInstance(t)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	user := &Turn{BotID: chief.ID, Source: sourceUser, Input: "ship the fix", Status: turnDone}
	bot := &Turn{BotID: chief.ID, Source: sourceBot, Input: "Message from worker:\ndone.", Status: turnRunning}
	if err := in.db.Create(user).Error; err != nil {
		t.Fatal(err)
	}
	if err := in.db.Create(bot).Error; err != nil {
		t.Fatal(err)
	}
	if got := lastOwnerRequest(in.db, chief.ID, bot); got != "ship the fix" {
		t.Errorf("lastOwnerRequest = %q, want the user text", got)
	}
	if chiefTimeoutShouldSpawn(bot, bot.Input) {
		t.Error("a bot report must not auto-spawn")
	}
	if !chiefTimeoutShouldSpawn(user, user.Input) {
		t.Error("an owner turn must auto-spawn")
	}
}

func TestChiefTimeoutInputAfterSpawnIsStillAFollowUp(t *testing.T) {
	got := chiefTimeoutInputAfterSpawn("deployer")
	if !isChiefTimeoutFollowUp(got) {
		t.Error("after-spawn inject must still be recognised as a follow-up")
	}
	if strings.Contains(got, "MUST pass it to a session") {
		t.Error("after-spawn inject must not tell General to spawn again")
	}
	if !strings.Contains(got, "deployer") || !strings.Contains(got, "do not spawn a duplicate") {
		t.Errorf("after-spawn inject missing the session name:\n%s", got)
	}
}

func mustAutoSpawnedWorker(t *testing.T, db *gorm.DB, chief *Bot, owner string) *Bot {
	t.Helper()
	wantName := botNameFromText(owner)
	spawned, err := botByName(db, wantName)
	if err != nil {
		t.Fatalf("auto-spawned worker %q: %v", wantName, err)
	}
	if spawned.TopicID >= 0 {
		t.Fatalf("auto-spawned worker should be backend-only, got %+v", spawned)
	}
	var queued []InboxMessage
	db.Where("from_bot_id = ? AND to_bot_id = ?", chief.ID, spawned.ID).Find(&queued)
	if len(queued) != 1 {
		t.Fatalf("first prompt not queued: %+v", queued)
	}
	if !strings.Contains(queued[0].Text, owner) {
		t.Errorf("worker prompt missing owner request %q: %q", owner, queued[0].Text)
	}
	if !strings.Contains(queued[0].Text, "60s cap") {
		t.Errorf("worker prompt missing timeout context: %q", queued[0].Text)
	}
	if !queued[0].Wake {
		t.Error("auto-spawned worker must wake")
	}
	return spawned
}

func assertNoOwnerSessionNudge(t *testing.T, ui *fakeUI) {
	t.Helper()
	for _, p := range ui.posts {
		if strings.Contains(p, "/session") || strings.Contains(strings.ToLower(p), "could not hand this off") {
			t.Errorf("owner-facing text must not ask for /session: %q", p)
		}
	}
}

func TestWorkerDoneRelaysSummaryNotOnlyOneLiner(t *testing.T) {
	in, _, _ := testInstance(t)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	w, err := in.createBot("Fragua", "")
	if err != nil {
		t.Fatal(err)
	}
	ui := &fakeUI{}
	r := newRunner(in.db, in.cfg, ui)
	s := &mcpServer{db: in.db, config: in.cfg, botID: w.ID}
	if res, _, err := s.reportToGeneral(t.Context(), nil, reportToGeneralIn{Text: "Valheim is up on vps2, 3x resources."}); err != nil || res.IsError {
		t.Fatalf("report: %+v %v", res, err)
	}

	r.postOwnerSessionStatus(w, "", false)
	r.ensureWorkerRelayToGeneral(w, "", false, "a long transcript the owner must not see")
	r.deliverInbox(w.ID)

	var genTurns []Turn
	in.db.Where("bot_id = ? AND source = ?", chief.ID, sourceBot).Find(&genTurns)
	if len(genTurns) != 1 {
		t.Fatalf("General turns = %d, want the relay wake", len(genTurns))
	}
	var inbox InboxMessage
	if err := in.db.Where("turn_id = ?", genTurns[0].ID).First(&inbox).Error; err != nil {
		t.Fatal(err)
	}
	if !inbox.Relay || inbox.Text != "Valheim is up on vps2, 3x resources." {
		t.Fatalf("relay inbox = %+v", inbox)
	}

	summary := "Valheim is up on vps2."
	if _, err := ui.Post(0, summary); err != nil {
		t.Fatal(err)
	}
	r.fulfillOwnerRelay(&genTurns[0], "", summary)

	if !ownerPostsInclude(ui, summary) {
		t.Errorf("DM missing General's summary, posts=%v", ui.posts)
	}
	if ownerPostsOnlySessionOneLiner(ui, w.Name) {
		t.Errorf("owner was left with only the one-liner: %v", ui.posts)
	}
	for _, p := range ui.posts {
		if strings.Contains(p, "a long transcript") {
			t.Errorf("synthetic last-message must not dump when report_to_general already queued: %q", p)
		}
	}
}

func TestGeneralTimeoutOnWorkerReportPostsFallback(t *testing.T) {
	in, _, _ := testInstance(t)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	w, err := in.createBot("Fragua", "")
	if err != nil {
		t.Fatal(err)
	}
	ui := &fakeUI{}
	r := newRunner(in.db, in.cfg, ui)
	s := &mcpServer{db: in.db, config: in.cfg, botID: w.ID}
	if res, _, err := s.reportToGeneral(t.Context(), nil, reportToGeneralIn{Text: "Valheim is up on vps2, 3x resources."}); err != nil || res.IsError {
		t.Fatalf("report: %+v %v", res, err)
	}

	r.postOwnerSessionStatus(w, "", false)
	r.ensureWorkerRelayToGeneral(w, "", false, "")
	r.deliverInbox(w.ID)

	var genTurn Turn
	if err := in.db.Where("bot_id = ? AND source = ?", chief.ID, sourceBot).First(&genTurn).Error; err != nil {
		t.Fatal(err)
	}
	r.persistChiefTimeout(chief, &genTurn, genTurn.Input, time.Now(), nil)
	r.fulfillOwnerRelay(&genTurn, chiefTimeoutClass, "")

	if ownerPostsOnlySessionOneLiner(ui, w.Name) {
		t.Fatalf("timeout left only the one-liner: %v", ui.posts)
	}
	if !ownerPostsInclude(ui, "Valheim is up on vps2") {
		t.Errorf("fallback missing worker last message, posts=%v", ui.posts)
	}
	var extra int64
	in.db.Model(&Bot{}).Where("id != ? AND id != ? AND archived_at IS NULL", chief.ID, w.ID).Count(&extra)
	if extra != 0 {
		t.Fatalf("report timeout spawned %d extra workers", extra)
	}
	var queued int64
	in.db.Model(&Turn{}).Where("bot_id = ? AND status = ?", chief.ID, turnQueued).Count(&queued)
	if queued != 0 {
		t.Errorf("report timeout must not inject a follow-up, got %d queued", queued)
	}
}

func TestWorkerLastMessageWakesGeneralWhenNoReport(t *testing.T) {
	in, _, _ := testInstance(t)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	w, err := in.createBot("deployer", "")
	if err != nil {
		t.Fatal(err)
	}
	ui := &fakeUI{}
	r := newRunner(in.db, in.cfg, ui)
	r.postOwnerSessionStatus(w, "", false)
	r.ensureWorkerRelayToGeneral(w, "", false, "shipped fecha to vps3")
	r.deliverInbox(w.ID)

	var genTurn Turn
	if err := in.db.Where("bot_id = ? AND source = ?", chief.ID, sourceBot).First(&genTurn).Error; err != nil {
		t.Fatal(err)
	}
	r.fulfillOwnerRelay(&genTurn, chiefTimeoutClass, "")
	if ownerPostsOnlySessionOneLiner(ui, w.Name) {
		t.Fatalf("timeout left only the one-liner: %v", ui.posts)
	}
	if !ownerPostsInclude(ui, "shipped fecha to vps3") {
		t.Errorf("fallback missing last message, posts=%v", ui.posts)
	}
}

func TestIdleRemindDoesNotFallbackToOwner(t *testing.T) {
	in, _, _ := testInstance(t)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	w, err := in.createBot("parked", "")
	if err != nil {
		t.Fatal(err)
	}
	ui := &fakeUI{}
	r := newRunner(in.db, in.cfg, ui)
	if _, _, err := queueBotMessage(in.db, w, chief.Name, idleRemindText(w.Name), true); err != nil {
		t.Fatal(err)
	}
	r.deliverInbox(w.ID)
	var genTurn Turn
	if err := in.db.Where("bot_id = ? AND source = ?", chief.ID, sourceBot).First(&genTurn).Error; err != nil {
		t.Fatal(err)
	}
	r.persistChiefTimeout(chief, &genTurn, genTurn.Input, time.Now(), nil)
	r.fulfillOwnerRelay(&genTurn, chiefTimeoutClass, "")
	if ownerPostsInclude(ui, "still waiting") || ownerPostsInclude(ui, "Idle session") {
		t.Errorf("idle nag must not land in the DM: %v", ui.posts)
	}
}

func TestOwnerRelayFallbackIsShortNotTranscript(t *testing.T) {
	got := ownerRelayFallback("Fragua", "Valheim is up.")
	if got != "Fragua: Valheim is up." {
		t.Errorf("fallback = %q", got)
	}
	long := strings.Repeat("x", ownerRelayFallbackMax+50)
	got = ownerRelayFallback("Fragua", long)
	if !strings.HasPrefix(got, "Fragua: ") {
		t.Errorf("missing name prefix: %q", got[:20])
	}
	if strings.Contains(got, strings.Repeat("x", ownerRelayFallbackMax+1)) {
		t.Error("fallback dumped the full transcript")
	}
	if len(got) > ownerRelayFallbackMax+len("Fragua: ")+10 {
		t.Errorf("fallback too long: %d", len(got))
	}
}

func ownerPostsInclude(ui *fakeUI, needle string) bool {
	for _, p := range ui.posts {
		if strings.Contains(p, needle) {
			return true
		}
	}
	return false
}

func ownerPostsOnlySessionOneLiner(ui *fakeUI, name string) bool {
	n := 0
	for _, p := range ui.posts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if isOwnerSessionStatusPost(p, name) {
			n++
			continue
		}
		return false
	}
	return n > 0
}

func isOwnerSessionStatusPost(html, name string) bool {
	switch html {
	case ownerSessionStatusLine(name, "done"),
		ownerSessionStatusLine(name, "waiting"),
		ownerSessionStatusLine(name, "error"),
		ownerSessionStatusLine(name, "started"):
		return true
	}
	// Live session card: one HTML block per working session, silent, no summary.
	if !strings.Contains(html, "<b>"+name+"</b>") {
		return false
	}
	for _, label := range []string{"running", "waiting", "done", "error", "started", "job"} {
		if strings.Contains(html, " · "+label) {
			return true
		}
	}
	return false
}
