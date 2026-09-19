package main

import (
	"strings"
	"testing"
)

// dmMessage builds an inbound private message from an arbitrary user.
func dmMessage(userID int64, text string) *TelegramMessage {
	m := &TelegramMessage{Text: text, MessageID: 11}
	m.Chat.ID = userID
	m.Chat.Type = "private"
	m.From.ID = userID
	m.From.Username = "stranger"
	return m
}

// groupMessage builds an inbound group message from an arbitrary user.
func groupMessage(userID, threadID int64, text string) *TelegramMessage {
	m := &TelegramMessage{MessageThreadID: threadID, Text: text, MessageID: 7}
	m.Chat.ID = -100777
	m.Chat.Type = "supergroup"
	m.From.ID = userID
	m.From.Username = "stranger"
	return m
}

func allowUsers(in *instance, ids ...int64) {
	in.cfg.AllowedUserIDs = ids
}

// A stranger in the group is dropped without a sound: answering there would let
// anyone who finds the group make the bot talk.
func TestStrangerInGroupIsIgnoredSilently(t *testing.T) {
	in, runner, api := testInstance(t)
	if _, err := in.createBot("worker", ""); err != nil {
		t.Fatal(err)
	}
	before := len(api.since("sendMessage"))

	in.handleMessage(groupMessage(999, 1, "run rm -rf /"))
	in.handleMessage(groupMessage(999, 0, "make me a bot"))

	if _, ok := runner.last(); ok {
		t.Error("a stranger's group message must not enqueue anything")
	}
	if got := len(api.since("sendMessage")) - before; got != 0 {
		t.Errorf("ccc sent %d message(s) to a stranger in the group, want 0", got)
	}
	if len(api.since("createForumTopic")) != 0 {
		t.Error("a stranger must not be able to create a bot")
	}
}

// A stranger's DM is dropped with no reply, no owner ping, and no pairing code.
func TestStrangerDMIsIgnoredSilently(t *testing.T) {
	in, runner, api := testInstance(t)
	before := len(api.since("sendMessage"))

	in.handleMessage(dmMessage(999, "hello?"))
	in.handleMessage(dmMessage(999, "let me in"))
	in.handleMessage(dmMessage(999, "please"))

	if got := len(api.since("sendMessage")) - before; got != 0 {
		t.Errorf("a stranger's DM got %d reply(ies), want 0: %q", got, api.texts(""))
	}
	if _, ok := runner.last(); ok {
		t.Error("a stranger's DM must not reach a bot")
	}
	joined := strings.Join(api.texts(""), "\n")
	if strings.Contains(joined, "This bot is private") || strings.Contains(joined, "wants access") {
		t.Errorf("pairing leaked through: %s", joined)
	}
}

// An allowed user may talk in the DM (General), but the instance itself stays
// the owner's: /account, /access, /model and /secret are refused. Group messages drop.
func TestAllowedUserCanTalkButNotAdminister(t *testing.T) {
	in, runner, api := testInstance(t)
	allowUsers(in, 999)

	in.handleMessage(dmMessage(999, "what is the status?"))
	last, ok := runner.last()
	if !ok || last.Text != "what is the status?" {
		t.Fatalf("an allowed user could not talk in the DM: %+v", last)
	}
	g, err := generalBot(in.db)
	if err != nil || last.BotID != g.ID {
		t.Fatalf("allowed-user DM must go to General: %+v", last)
	}

	in.handleMessage(groupMessage(999, 1, "talk in a leftover topic"))
	if got, _ := runner.last(); got.Text != "what is the status?" {
		t.Errorf("an allowed user's group message must be ignored, last=%+v", got)
	}

	for _, cmd := range []string{"/account", "/access list", "/model haiku", "/secret list"} {
		in.handleMessage(dmMessage(999, cmd))
	}
	joined := strings.Join(api.texts(""), "\n")
	if strings.Count(joined, "owner-only") != 4 {
		t.Errorf("owner commands were not all refused for an allowed user:\n%s", joined)
	}
	if in.config().Model != "" {
		t.Errorf("an allowed user changed the model to %q", in.config().Model)
	}
}

func TestUnknownUserGetsNothing(t *testing.T) {
	in, _, api := testInstance(t)
	before := len(api.since("sendMessage"))
	in.handleMessage(dmMessage(999, "hi again"))
	if got := len(api.since("sendMessage")) - before; got != 0 {
		t.Errorf("an unknown user got %d message(s), want 0", got)
	}
}

func TestClassifyAccessOwnerAndUnbootstrapped(t *testing.T) {
	in, _, _ := testInstance(t)
	if got := classifyAccess(in.config(), 42); got != roleOwner {
		t.Errorf("owner role = %v, want roleOwner", got)
	}
	if got := classifyAccess(in.config(), 0); got != roleDenied {
		t.Error("an update with no sender must be denied")
	}
	// Before bootstrap there is no owner, so nobody is allowed — not even the
	// first person to message the bot.
	if got := classifyAccess(&Config{}, 42); got != roleDenied {
		t.Error("with no chat_id configured, everybody must be denied")
	}
	if got := classifyAccess(in.config(), 999); got != roleDenied {
		t.Error("an unknown id must be denied")
	}
	allowUsers(in, 999)
	if got := classifyAccess(in.config(), 999); got != roleUser {
		t.Error("a whitelisted id must be allowed")
	}
	if got := classifyAccess(in.config(), 42); got != roleOwner {
		t.Error("the owner stays owner even when also listed")
	}
}

func TestAccessListShowsConfigWhitelist(t *testing.T) {
	in, _, api := testInstance(t)
	allowUsers(in, 4242, 42)

	in.handleMessage(dmMessage(42, "/access"))
	joined := strings.Join(api.texts(""), "\n")
	if !strings.Contains(joined, "4242") || !strings.Contains(joined, "owner") {
		t.Errorf("/access list did not show the config whitelist:\n%s", joined)
	}
	if strings.Contains(joined, "pairing") || strings.Contains(joined, "pending") {
		t.Errorf("/access still talks about pairing:\n%s", joined)
	}

	in.handleMessage(dmMessage(42, "/access add 7777"))
	if classifyAccess(in.config(), 7777) != roleDenied {
		t.Error("/access add must not mutate the whitelist")
	}
	if !strings.Contains(strings.Join(api.texts(""), "\n"), "allowed_user_ids") {
		t.Error("/access add should point at ccc config set allowed_user_ids")
	}
}

// Leftover Allow/Block buttons from the old pairing flow must not grant access.
func TestAccessCallbackIsIgnored(t *testing.T) {
	in, _, _ := testInstance(t)

	stranger := &CallbackQuery{ID: "cb", Data: "access:pair:dead00"}
	stranger.From.ID = 999
	in.handleCallback(stranger)
	if classifyAccess(in.config(), 999) != roleDenied {
		t.Fatal("a stranger's tap approved a pairing request")
	}

	owner := &CallbackQuery{ID: "cb2", Data: "access:pair:dead00"}
	owner.From.ID = 42
	owner.Message = dmMessage(42, "")
	in.handleCallback(owner)
	if classifyAccess(in.config(), 777) != roleDenied {
		t.Error("the owner's leftover Allow tap must not approve anyone")
	}
}

func TestParseAllowedUserIDs(t *testing.T) {
	got, err := parseAllowedUserIDs("4242, 99 99, 7")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != 4242 || got[1] != 99 || got[2] != 7 {
		t.Errorf("parseAllowedUserIDs = %v", got)
	}
	if ids, err := parseAllowedUserIDs(""); err != nil || ids != nil {
		t.Errorf("empty should clear, got %v (%v)", ids, err)
	}
	if _, err := parseAllowedUserIDs("12a"); err == nil {
		t.Error("non-numeric must be rejected")
	}
	if _, err := parseAllowedUserIDs("0"); err == nil {
		t.Error("zero must be rejected")
	}
	if _, err := parseAllowedUserIDs("-1"); err == nil {
		t.Error("negative must be rejected")
	}
}

func TestLegacyAccessTableIsDropped(t *testing.T) {
	in, _, _ := testInstance(t)
	if err := in.db.Exec(`CREATE TABLE access (telegram_user_id INTEGER PRIMARY KEY, state TEXT)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := dropLegacyAccessTable(in.db); err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := in.db.Raw(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='access'`).Scan(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("the access table is still there")
	}
}
