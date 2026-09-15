package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRenderSystemPromptCarriesIdentityAndRoster(t *testing.T) {
	got := renderSystemPrompt(
		promptBot{Name: "deployer", Role: "ships fecha to prod", Cwd: "/srv/fecha"},
		"jairo.local",
		[]otherBot{{Name: "watcher", Role: "watches CI"}},
		[]string{"🚀", "📝"},
	)
	for _, want := range []string{"deployer", "ships fecha to prod", "jairo.local", "/srv/fecha", "watcher", "watches CI",
		"set_name", "🚀"} {
		if !strings.Contains(got, want) {
			t.Errorf("system prompt is missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "ask_owner") || !strings.Contains(got, "remember") {
		t.Error("system prompt does not describe the ccc tools")
	}
	if !strings.Contains(got, "set_routine") {
		t.Error("system prompt does not describe routines")
	}
	if !strings.Contains(got, "run_background") {
		t.Error("system prompt does not describe background jobs")
	}
	if strings.Contains(got, "spawn_bot") {
		t.Error("system prompt must not offer spawn_bot")
	}
}

func TestRenderSystemPromptWithoutRole(t *testing.T) {
	got := renderSystemPrompt(promptBot{Name: "fresh", Cwd: "/tmp"}, "host", nil, nil)
	if !strings.Contains(got, "/role") {
		t.Errorf("a role-less bot should be told how a role gets set:\n%s", got)
	}
	if strings.Contains(got, "Other bots:") {
		t.Error("no roster should be rendered when there are no other bots")
	}
}

func TestRenderEnvelopeShape(t *testing.T) {
	now := time.Date(2026, 9, 14, 10, 30, 0, 0, time.UTC)
	got := renderEnvelope(envelopeInput{
		Source:      "bot:watcher",
		Message:     "the deploy failed",
		Now:         now,
		UserMems:    []Memory{{Key: "tz", Text: "Europe/Madrid"}},
		ProjectMems: []Memory{{Key: "deploy", Text: "systemd on vps3"}},
		BotMems:     []Memory{{Key: "last-run", Text: "green"}},
		InboxFrom:   map[string]int{"watcher": 2},
	})
	for _, want := range []string{
		"<context>", "</context>", "2026-09-14",
		"user memories:", "tz: Europe/Madrid",
		"project memories:", "deploy: systemd on vps3",
		"your memories:", "last-run: green",
		"pending inbox: 2 message(s) (2 from watcher)",
		`<message source="bot:watcher">`, "the deploy failed", "</message>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("envelope is missing %q:\n%s", want, got)
		}
	}
}

func TestRenderEnvelopeDefaultsSourceToUser(t *testing.T) {
	got := renderEnvelope(envelopeInput{Message: "hi", Now: time.Now()})
	if !strings.Contains(got, `<message source="user">`) {
		t.Errorf("missing default source:\n%s", got)
	}
}

func TestRenderEnvelopeRespectsTheContextBudget(t *testing.T) {
	mems := make([]Memory, 200)
	for i := range mems {
		mems[i] = Memory{Key: fmt.Sprintf("key-%03d", i), Text: strings.Repeat("x", 200)}
	}
	message := "what is the status?"
	got := renderEnvelope(envelopeInput{
		Message: message, Now: time.Now(),
		UserMems: mems, ProjectMems: mems, BotMems: mems,
	})

	start := strings.Index(got, "<context>\n")
	end := strings.Index(got, "</context>")
	if start < 0 || end < 0 {
		t.Fatalf("no context block:\n%s", got)
	}
	ctx := got[start+len("<context>\n") : end]
	if len(ctx) > envelopeBudget {
		t.Errorf("context block is %d bytes, over the %d budget", len(ctx), envelopeBudget)
	}
	// The message itself is never sacrificed to the budget.
	if !strings.Contains(got, message) {
		t.Error("the message was dropped by the budget")
	}
	if !strings.Contains(ctx, "key-000") {
		t.Error("the budget dropped everything instead of filling up to the cap")
	}
}

func TestBuildEnvelopePullsLiveContext(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("ctx", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := upsertMemory(in.db, scopeUser, "", "tz", "Europe/Madrid", b.ID); err != nil {
		t.Fatal(err)
	}
	if err := upsertMemory(in.db, scopeBot, fmt.Sprint(b.ID), "mood", "calm", b.ID); err != nil {
		t.Fatal(err)
	}
	other, err := in.createBot("other", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := in.db.Create(&InboxMessage{ToBotID: b.ID, FromBotID: &other.ID, Text: "ping"}).Error; err != nil {
		t.Fatal(err)
	}

	got := buildEnvelope(in.db, b, sourceUser, "hello", time.Now())
	for _, want := range []string{"tz: Europe/Madrid", "mood: calm", "1 from other", "hello"} {
		if !strings.Contains(got, want) {
			t.Errorf("envelope missing %q:\n%s", want, got)
		}
	}
}

func TestBuildEnvelopeKeepsOtherBotsMemoriesOut(t *testing.T) {
	in, _, _ := testInstance(t)
	mine, _ := in.createBot("mine", "")
	theirs, _ := in.createBot("theirs", "")
	if err := upsertMemory(in.db, scopeBot, fmt.Sprint(theirs.ID), "secret", "not for you", theirs.ID); err != nil {
		t.Fatal(err)
	}
	got := buildEnvelope(in.db, mine, sourceUser, "hi", time.Now())
	if strings.Contains(got, "not for you") {
		t.Error("a bot's private memory leaked into another bot's envelope")
	}
}

// A bot with no role is onboarded through the envelope, so the instruction can
// disappear the moment update_instructions runs (the system prompt could not).
func TestEnvelopeOnboardsARoleLessBot(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("nameless", "")
	if err != nil {
		t.Fatal(err)
	}

	got := buildEnvelope(in.db, b, sourceUser, "hello", time.Now())
	for _, want := range []string{"no role yet", "update_instructions", "set_name"} {
		if !strings.Contains(got, want) {
			t.Errorf("a role-less bot was not onboarded (missing %q):\n%s", want, got)
		}
	}

	if err := in.db.Model(&Bot{}).Where("id = ?", b.ID).Update("role", "ships things").Error; err != nil {
		t.Fatal(err)
	}
	withRole, err := botByID(in.db, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := buildEnvelope(in.db, withRole, sourceUser, "hello", time.Now()); strings.Contains(got, "no role yet") {
		t.Errorf("the onboarding instruction survived the role being set:\n%s", got)
	}
}

// The system prompt is the first thing in every request of a session, so the
// API's prompt cache only helps while it is byte-identical from turn to turn
// (DESIGN §14.20). Nothing that moves on its own may appear in it.
func TestSystemPromptIsByteStableAcrossTurns(t *testing.T) {
	b := promptBot{Name: "deployer", Role: "ships fecha", Cwd: "/srv/fecha"}
	first := renderSystemPrompt(b, "jairo.local",
		[]otherBot{{Name: "watcher", Role: "watches CI"}, {Name: "archivist", Role: "keeps notes"}},
		[]string{"🚀", "📝"})
	// Same facts, different order, and the bots have been busy meanwhile.
	second := renderSystemPrompt(b, "jairo.local",
		[]otherBot{{Name: "archivist", Role: "keeps notes"}, {Name: "watcher", Role: "watches CI"}},
		[]string{"📝", "🚀"})
	if first != second {
		t.Errorf("the system prompt changed between turns:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	if strings.Contains(first, botIdle) || strings.Contains(first, botRunning) {
		t.Error("a bot's live status must not be in the system prompt: it changes every turn")
	}
	if strings.Contains(first, time.Now().Format("2006-01-02")) {
		t.Error("the date belongs in the envelope, not in the system prompt")
	}
}

// The prompt tells bots how to spend a teammate's tokens.
func TestSystemPromptTeachesBackgroundInsteadOfSpawn(t *testing.T) {
	got := renderSystemPrompt(promptBot{Name: "a", Cwd: "/tmp"}, "host", nil, nil)
	for _, want := range []string{"run_background", "source=background", "60 seconds", "You cannot create other bots"} {
		if !strings.Contains(got, want) {
			t.Errorf("the system prompt does not teach background jobs (%q):\n%s", want, got)
		}
	}
	if strings.Contains(got, "Spawn a bot") || strings.Contains(got, "spawn_bot") {
		t.Errorf("the system prompt still offers spawning bots:\n%s", got)
	}
}

func TestSystemPromptTeachesWakeDiscipline(t *testing.T) {
	got := renderSystemPrompt(promptBot{Name: "a", Cwd: "/tmp"}, "host", nil, nil)
	for _, want := range []string{"wake=false", "wake=true", "ONE message"} {
		if !strings.Contains(got, want) {
			t.Errorf("the system prompt does not teach bot-to-bot wake discipline (%q):\n%s", want, got)
		}
	}
}

// The roster is sorted wherever it is built, not only where it is rendered.
func TestBotRosterIsSortedByName(t *testing.T) {
	in, _, _ := testInstance(t)
	for _, name := range []string{"zeta", "alpha", "mu"} {
		if _, err := in.createBot(name, ""); err != nil {
			t.Fatal(err)
		}
	}
	roster := botRoster(in.db, 0)
	if len(roster) != 3 || roster[0].Name != "alpha" || roster[2].Name != "zeta" {
		t.Errorf("roster = %+v, want it sorted by name", roster)
	}
}
