package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRenderSessionPanelCards(t *testing.T) {
	got := renderSessionPanel([]sessionCard{
		{Name: "deployer", Status: botRunning, Line: "editing runner.go", Elapsed: "12s"},
		{Name: "ads", Status: botWaiting, Line: "Raise the bid?"},
	})
	for _, want := range []string{
		"⏳ <b>deployer</b> · running · 12s",
		"<i>editing runner.go</i>",
		"❓ <b>ads</b> · waiting",
		"<i>Raise the bid?</i>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "<b>Work</b>") {
		t.Error("the panel is the cards; it does not need a heading")
	}
}

func TestRenderSessionPanelEscapes(t *testing.T) {
	got := renderSessionPanel([]sessionCard{
		{Name: "a<b>", Status: botRunning, Line: "use <token>"},
	})
	if strings.Contains(got, "a<b>") || strings.Contains(got, "<token>") {
		t.Fatalf("raw HTML reached the panel: %q", got)
	}
	if !strings.Contains(got, "a&lt;b&gt;") {
		t.Errorf("name was not escaped: %q", got)
	}
}

func TestSessionPanelPinsWhileWorkingAndUnpinsWhenIdle(t *testing.T) {
	in, _, _ := testInstance(t)
	ui := &fakeUI{}
	p := newSessionPanel(in.db, ui)
	w, err := in.createBot("deployer", "")
	if err != nil {
		t.Fatal(err)
	}
	setBotStatus(in.db, w.ID, botRunning)
	now := time.Now()
	in.db.Create(&Turn{BotID: w.ID, Source: sourceUser, Input: "go", Status: turnRunning, StartedAt: &now})

	p.setActivity(w.ID, "editing runner.go")
	if len(ui.posts) != 1 || !ui.silentPosts[0] {
		t.Fatalf("working card must be a silent post, posts=%v silent=%v", ui.posts, ui.silentPosts)
	}
	if !strings.Contains(ui.posts[0], "<b>deployer</b>") || !strings.Contains(ui.posts[0], "editing runner.go") {
		t.Errorf("card = %q", ui.posts[0])
	}
	if len(ui.pins) != 1 {
		t.Errorf("working card must be pinned, pins=%v", ui.pins)
	}

	p.lastEdit = time.Time{}
	p.setActivity(w.ID, "running go test")
	if len(ui.posts) != 1 {
		t.Errorf("activity should edit in place, posts=%d", len(ui.posts))
	}
	if len(ui.edits) == 0 || !strings.Contains(ui.edits[len(ui.edits)-1], "running go test") {
		t.Errorf("edits = %v", ui.edits)
	}

	setBotStatus(in.db, w.ID, botIdle)
	p.showTerminal(w, "done")
	if len(ui.unpins) != 1 {
		t.Errorf("done card must unpin, unpins=%v", ui.unpins)
	}
	last := ui.edits[len(ui.edits)-1]
	if !strings.Contains(last, "✅") || !strings.Contains(last, "done") {
		t.Errorf("settled card = %q", last)
	}
}

func TestSessionPanelOmitsGeneral(t *testing.T) {
	in, _, _ := testInstance(t)
	ui := &fakeUI{}
	p := newSessionPanel(in.db, ui)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	setBotStatus(in.db, chief.ID, botRunning)
	p.setActivity(chief.ID, "thinking")
	if len(ui.posts) != 0 {
		t.Errorf("General has its own ⏳ progress, not a session card: %v", ui.posts)
	}
}

func TestSessionPanelShowsWaitingQuestion(t *testing.T) {
	in, _, _ := testInstance(t)
	ui := &fakeUI{}
	p := newSessionPanel(in.db, ui)
	w, err := in.createBot("ads", "")
	if err != nil {
		t.Fatal(err)
	}
	setBotStatus(in.db, w.ID, botWaiting)
	in.db.Create(&Question{BotID: w.ID, Question: "Raise the bid to €0.80?"})
	p.sync(true)
	if len(ui.posts) != 0 || len(ui.pins) != 0 {
		t.Fatalf("a parked ask_owner must not get a card, posts=%v pins=%v", ui.posts, ui.pins)
	}
	for _, edit := range ui.edits {
		if strings.Contains(edit, "Raise the bid") || strings.Contains(edit, "waiting") {
			t.Errorf("question leaked onto the card: %q", edit)
		}
	}
}

func TestSessionPanelIncludesBackgroundJob(t *testing.T) {
	in, _, _ := testInstance(t)
	ui := &fakeUI{}
	p := newSessionPanel(in.db, ui)
	w, err := in.createBot("builder", "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	in.db.Create(&BackgroundJob{BotID: w.ID, Name: "go test", Status: jobRunning, Kind: jobKindShell, Command: "go test ./...", StartedAt: &now})
	p.sync(true)
	if len(ui.posts) != 1 || !strings.Contains(ui.posts[0], "go test") {
		t.Errorf("job card = %v", ui.posts)
	}
	if len(ui.pins) != 1 {
		t.Errorf("a running job is work and must pin, pins=%v", ui.pins)
	}
}

func TestPinChatMessageIsSilent(t *testing.T) {
	in, _, api := testInstance(t)
	tg := telegramUI{in}
	if err := tg.Pin(99); err != nil {
		t.Fatal(err)
	}
	pins := api.since("pinChatMessage")
	if len(pins) != 1 {
		t.Fatalf("pin calls = %d, want 1", len(pins))
	}
	if pins[0].Params.Get("message_id") != "99" {
		t.Errorf("message_id = %s", pins[0].Params.Get("message_id"))
	}
	if pins[0].Params.Get("disable_notification") != "true" {
		t.Error("the pin must not notify")
	}
	if err := tg.Unpin(99); err != nil {
		t.Fatal(err)
	}
	if n := len(api.since("unpinChatMessage")); n != 1 {
		t.Errorf("unpin calls = %d, want 1", n)
	}
}

func TestSessionPanelRepostsWhenEditFails(t *testing.T) {
	in, _, _ := testInstance(t)
	ui := &fakeUI{editErr: errPanelGone}
	p := newSessionPanel(in.db, ui)
	w, err := in.createBot("deployer", "")
	if err != nil {
		t.Fatal(err)
	}
	setBotStatus(in.db, w.ID, botRunning)
	p.setActivity(w.ID, "thinking")
	if len(ui.posts) != 1 {
		t.Fatalf("first post = %v", ui.posts)
	}
	p.lastEdit = time.Time{} // bypass rate limit
	p.setActivity(w.ID, "editing")
	if len(ui.posts) < 2 {
		t.Fatalf("failed edit must post a replacement, posts=%v edits=%v", ui.posts, ui.edits)
	}
}

var errPanelGone = errors.New("message to edit not found")
