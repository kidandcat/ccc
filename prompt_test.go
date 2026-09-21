package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRenderSystemPromptCarriesIdentity(t *testing.T) {
	got := renderSystemPrompt(
		promptBot{Name: "deployer", Cwd: "/srv/fecha"},
		"jairo.local",
		[]otherBot{{Name: "watcher", Role: "watches CI"}},
	)
	for _, want := range []string{"deployer", "jairo.local", "/srv/fecha",
		"set_name", "session"} {
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
	if !strings.Contains(got, "4 hours") {
		t.Error("system prompt must say watches expire so they can be re-set")
	}
	if !strings.Contains(got, "MUST be a watch") {
		t.Error("system prompt must forbid polling via schedule_wakeup")
	}
	if !strings.Contains(got, "fresh worker") {
		t.Error("system prompt must say routines run as a fresh worker")
	}
	if !strings.Contains(got, "run_background") {
		t.Error("system prompt does not describe background jobs")
	}
	if !strings.Contains(got, "secrets_list") || !strings.Contains(got, "there is no secrets_get") {
		t.Error("system prompt must describe the vault and that there is no secrets_get")
	}
	if strings.Contains(got, "secrets_get") && !strings.Contains(got, "no secrets_get") {
		t.Error("system prompt must not offer secrets_get")
	}
	if strings.Contains(got, "spawn_bot") {
		t.Error("system prompt must not offer spawn_bot")
	}
	if strings.Contains(got, "Role:") || strings.Contains(got, "/role") {
		t.Errorf("system prompt must not talk about roles:\n%s", got)
	}
	if strings.Contains(got, "send_to_bot") || strings.Contains(got, "list_bots") || strings.Contains(got, "update_instructions") {
		t.Errorf("system prompt still offers crew-of-bots tools:\n%s", got)
	}
	if strings.Contains(got, "Other bots") || strings.Contains(got, "watches CI") {
		t.Errorf("system prompt must not list other sessions as teammates:\n%s", got)
	}
	if strings.Contains(got, "Topic icons") || strings.Contains(got, "topic icon") {
		t.Errorf("system prompt must not talk about topic icons:\n%s", got)
	}
}

func TestSystemPromptRequiresAskOwnerForDecisions(t *testing.T) {
	worker := renderSystemPrompt(promptBot{Name: "a", Cwd: "/tmp"}, "host", nil)
	chief := renderSystemPrompt(promptBot{Name: "General", Cwd: "/tmp", Chief: true}, "host", nil)
	for name, got := range map[string]string{"worker": worker, "chief": chief} {
		if !strings.Contains(got, askOwnerRule) {
			t.Errorf("%s prompt missing shared ask_owner rule:\n%s", name, got)
		}
		for _, want := range []string{
			"ask_owner",
			"A or B?",
			"Yes/no",
			"recommended first",
			"architectural",
			"end the turn",
			"never an answer",
			"answers by writing a reply",
			"to the question in the DM",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("%s prompt missing %q:\n%s", name, want, got)
			}
		}
		for _, leak := range []string{"Telegram buttons", "Omitir", "native Telegram buttons"} {
			if strings.Contains(got, leak) {
				t.Errorf("%s prompt still mentions %q:\n%s", name, leak, got)
			}
		}
	}
	if !strings.Contains(worker, "optional listed options") {
		t.Errorf("worker tool list should name listed options:\n%s", worker)
	}
	if strings.Contains(worker, "Prefer ask_owner over guessing on anything architectural") {
		t.Error("old ask_owner wording leaked into the worker prompt")
	}
	if strings.Contains(worker, "When you need the owner to decide anything") {
		t.Error("soft ask_owner wording leaked into the worker prompt")
	}
}

func TestRenderSystemPromptHasNoRoleCeremony(t *testing.T) {
	got := renderSystemPrompt(promptBot{Name: "fresh", Cwd: "/tmp"}, "host", nil)
	if strings.Contains(got, "/role") || strings.Contains(got, "no specific role") {
		t.Errorf("a new session must not be told to pick a role:\n%s", got)
	}
	if strings.Contains(got, "Other bots:") {
		t.Error("no roster should be rendered")
	}
	if !strings.Contains(got, "backend session") {
		t.Errorf("prompt should describe a session:\n%s", got)
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
		"session memories:", "last-run: green",
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

func TestBuildEnvelopeKeepsOtherSessionsMemoriesOut(t *testing.T) {
	in, _, _ := testInstance(t)
	mine, _ := in.createBot("mine", "")
	theirs, _ := in.createBot("theirs", "")
	if err := upsertMemory(in.db, scopeBot, fmt.Sprint(theirs.ID), "secret", "not for you", theirs.ID); err != nil {
		t.Fatal(err)
	}
	got := buildEnvelope(in.db, mine, sourceUser, "hi", time.Now())
	if strings.Contains(got, "not for you") {
		t.Error("a session's private memory leaked into another session's envelope")
	}
}

func TestEnvelopeDoesNotOnboardANewSession(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("nameless", "")
	if err != nil {
		t.Fatal(err)
	}

	got := buildEnvelope(in.db, b, sourceUser, "help me with the deploy", time.Now())
	if !strings.Contains(got, "help me with the deploy") {
		t.Errorf("the owner's prompt was dropped:\n%s", got)
	}
	for _, banned := range []string{"no role yet", "update_instructions", "what you should be responsible"} {
		if strings.Contains(got, banned) {
			t.Errorf("a new session was given role onboarding (%q):\n%s", banned, got)
		}
	}
}

// The system prompt is the first thing in every request of a session, so the
// API's prompt cache only helps while it is byte-identical from turn to turn
// (DESIGN §14.20). Nothing that moves on its own may appear in it.
func TestSystemPromptIsByteStableAcrossTurns(t *testing.T) {
	b := promptBot{Name: "deployer", Cwd: "/srv/fecha"}
	first := renderSystemPrompt(b, "jairo.local",
		[]otherBot{{Name: "watcher", Role: "watches CI"}, {Name: "archivist", Role: "keeps notes"}})
	// Same facts, different leftover roster order — the roster is not rendered.
	second := renderSystemPrompt(b, "jairo.local",
		[]otherBot{{Name: "archivist", Role: "keeps notes"}, {Name: "watcher", Role: "watches CI"}})
	if first != second {
		t.Errorf("the system prompt changed between turns:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	if strings.Contains(first, botIdle) || strings.Contains(first, botRunning) {
		t.Error("a session's live status must not be in the system prompt: it changes every turn")
	}
	if strings.Contains(first, time.Now().Format("2006-01-02")) {
		t.Error("the date belongs in the envelope, not in the system prompt")
	}
}

// The prompt tells the session how to do long work without spawning teammates.
func TestSystemPromptTeachesBackgroundInsteadOfSpawn(t *testing.T) {
	got := renderSystemPrompt(promptBot{Name: "a", Cwd: "/tmp"}, "host", nil)
	for _, want := range []string{"run_background", "source=background", "60 seconds", "You cannot create other sessions"} {
		if !strings.Contains(got, want) {
			t.Errorf("the system prompt does not teach background jobs (%q):\n%s", want, got)
		}
	}
	if strings.Contains(got, "Spawn a bot") || strings.Contains(got, "spawn_bot") || strings.Contains(got, "send_to_bot") {
		t.Errorf("the system prompt still offers spawning or messaging teammates:\n%s", got)
	}
	if strings.Contains(got, "30 second") || strings.Contains(got, "60 second cap") {
		t.Error("the General turn cap is General-only; workers must not see it")
	}
	if strings.Contains(got, "the owner sees what you report_to_general") {
		t.Error("workers must not be told that reports land in the owner's chat")
	}
	if !strings.Contains(got, "readable digest") {
		t.Error("workers must be told to write a readable digest, not a crushed paragraph")
	}
	if !strings.Contains(got, "archive_bot last") && !strings.Contains(got, "do not keep using tools") {
		t.Error("workers must be told to archive last, not keep tooling after archive_bot")
	}
}

func TestSystemPromptHasNoWakeDiscipline(t *testing.T) {
	got := renderSystemPrompt(promptBot{Name: "a", Cwd: "/tmp"}, "host", nil)
	if strings.Contains(got, "wake=false") || strings.Contains(got, "wake=true") {
		t.Errorf("wake discipline is a crew-of-bots leftover:\n%s", got)
	}
}

// The roster helper is still sorted wherever it is built.
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
