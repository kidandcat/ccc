package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeUI records what the runner would have sent to Telegram.
type fakeUI struct {
	posts       []string
	silentPosts []bool
	edits       []string
	deleted     []int64
	editErr     error
	postErr     error
	deleteErr   error
	nextID      int64
}

func (f *fakeUI) Post(_ int64, html string) (int64, error) {
	if f.postErr != nil {
		return 0, f.postErr
	}
	f.posts = append(f.posts, html)
	f.silentPosts = append(f.silentPosts, false)
	f.nextID++
	return f.nextID, nil
}

func (f *fakeUI) PostSilent(_ int64, html string) (int64, error) {
	if f.postErr != nil {
		return 0, f.postErr
	}
	f.posts = append(f.posts, html)
	f.silentPosts = append(f.silentPosts, true)
	f.nextID++
	return f.nextID, nil
}

func (f *fakeUI) Edit(_, _ int64, html string) error {
	if f.editErr != nil {
		return f.editErr
	}
	f.edits = append(f.edits, html)
	return nil
}

func (f *fakeUI) React(int64, string) {}

func (f *fakeUI) Delete(_, msgID int64) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, msgID)
	return nil
}

// TestProgressFinishRendersModelMarkdown is the regression for the reply that
// Telegram rejected: the raw "<" placeholders must reach Telegram escaped.
func TestProgressFinishRendersModelMarkdown(t *testing.T) {
	ui := &fakeUI{}
	p := newProgress(ui, 7, time.Now())
	p.set("writing a reply")

	p.finish("Use `Authorization: Bearer <20>:<20>` with **<token_del_agente>**")

	if len(ui.posts) != 2 {
		t.Fatalf("expected progress + final reply, got posts=%v edits=%v", ui.posts, ui.edits)
	}
	if !ui.silentPosts[0] {
		t.Fatal("the progress message must be posted silently")
	}
	if ui.silentPosts[1] {
		t.Fatal("the final reply must notify")
	}
	final := ui.posts[1]
	if strings.Contains(final, "<20>") || strings.Contains(final, "<token_del_agente>") {
		t.Fatalf("raw angle brackets reached Telegram: %q", final)
	}
	if err := checkTelegramHTML(final); err != nil {
		t.Fatalf("final reply is not valid Telegram HTML: %v (%q)", err, final)
	}
	if len(ui.deleted) != 1 {
		t.Fatalf("the silent progress message should have been deleted, deleted=%v", ui.deleted)
	}
}

func TestProgressFinishDeletesTheStaleProgressMessage(t *testing.T) {
	ui := &fakeUI{}
	p := newProgress(ui, 7, time.Now())
	p.set("writing a reply")
	progressID := ui.nextID

	p.finish("done")

	if len(ui.posts) != 2 {
		t.Fatalf("the reply should have been posted as a new message, got %v", ui.posts)
	}
	if got := ui.posts[1]; got != "done" {
		t.Fatalf("posted %q, want %q", got, "done")
	}
	if len(ui.deleted) != 1 || ui.deleted[0] != progressID {
		t.Fatalf("stale progress message %d was not deleted, deleted=%v", progressID, ui.deleted)
	}
}

func TestProgressFinishFallsBackToEditingWhenDeleteFails(t *testing.T) {
	ui := &fakeUI{deleteErr: errors.New("telegram error: message can't be deleted")}
	p := newProgress(ui, 7, time.Now())
	p.set("writing a reply")

	p.finish("done")

	if len(ui.edits) != 1 || ui.edits[0] != "✅ replied below" {
		t.Fatalf("expected the progress message to be retired by an edit, got %v", ui.edits)
	}
}

func TestProgressFinishSplitsLongRepliesIntoValidChunks(t *testing.T) {
	ui := &fakeUI{}
	p := newProgress(ui, 7, time.Now())

	p.finish(longReplySample())

	if len(ui.posts) < 2 {
		t.Fatalf("a 4787-char reply should have been split, got %d message(s)", len(ui.posts))
	}
	for i, c := range ui.posts {
		if len(c) > telegramTextLimit {
			t.Errorf("chunk %d is %d bytes, over Telegram's %d cap", i+1, len(c), telegramTextLimit)
		}
		if err := checkTelegramHTML(c); err != nil {
			t.Errorf("chunk %d is not valid Telegram HTML: %v", i+1, err)
		}
	}
}

func TestProgressMessagesAreSilentUntilFinish(t *testing.T) {
	in, _, api := testInstance(t)
	ui := telegramUI{in}
	p := newProgress(ui, 7, time.Now())
	p.set("running tests")

	sends := api.since("sendMessage")
	if len(sends) != 1 {
		t.Fatalf("expected one progress send, got %d", len(sends))
	}
	if sends[0].Params.Get("disable_notification") != "true" {
		t.Fatalf("progress create must set disable_notification, params=%v", sends[0].Params)
	}

	p.finish("all green")
	sends = api.since("sendMessage")
	if len(sends) < 2 {
		t.Fatalf("expected progress + final, got %d sends", len(sends))
	}
	final := sends[len(sends)-1]
	if got := final.Params.Get("text"); got != "all green" {
		t.Fatalf("final text = %q", got)
	}
	if final.Params.Get("disable_notification") == "true" {
		t.Fatal("the final answer must notify")
	}
	if len(api.since("deleteMessage")) != 1 {
		t.Fatal("the silent progress message should have been deleted")
	}
}

func TestProgressFinishEmptyReply(t *testing.T) {
	ui := &fakeUI{}
	newProgress(ui, 7, time.Now()).finish("   ")
	if len(ui.posts) != 1 || ui.posts[0] != "(no reply)" {
		t.Fatalf("got %v, want a single \"(no reply)\"", ui.posts)
	}
}
