package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
)

// account.go is `/account` (DESIGN §8): the only UI for accounts, so the owner
// never needs a terminal on the machine ccc runs on. Engine is set when the
// account is added (`/account add <identity> <engine>`). It renders one card
// per account (Claude, Grok, Antigravity), drives that CLI's login through the
// pseudo-terminal in ptyflow.go, and — for Claude only — records the
// bypass-permissions disclaimer straight into settings.json afterwards.

// accountState is the health of one profile as the card shows it.
type accountState int

const (
	accountOK         accountState = iota // logged in and usable
	accountNeedsLogin                     // credentials exist but a turn was refused; re-login
	accountLoggedOut                      // auth status / credential file says logged out
	accountUnknown                        // could not ask
)

func (s accountState) icon() string {
	switch s {
	case accountOK:
		return "✅ logged in"
	case accountNeedsLogin:
		return "⚠️ needs login"
	case accountLoggedOut:
		return "❌ not logged in"
	case accountUnknown:
		return "❔ unknown"
	default:
		return "❔ unknown" // exhaustive-ok: every state above is covered; this is the zero-value guard
	}
}

// accountCard is everything /account status shows about one profile.
type accountCard struct {
	Profile    Profile
	State      accountState
	Account    string
	Usage      profileUsage
	Bots       []string
	IsDefault  bool
	Disclaimer bool
}

// collectAccountCards probes every profile. It shells out once per profile
// (`claude auth status --json`), so it is called on demand, never per turn.
// Learning what a profile's account actually is re-keys it (accounts are
// addressed by email, DESIGN §8), so the cards are built from the config as it
// is AFTER the probes.
func (in *instance) collectAccountCards() []accountCard {
	type probe struct {
		p       Profile
		state   accountState
		account string
	}
	var probes []probe
	for _, p := range listProfiles(in.config()) {
		pr := probe{p: p}
		loggedIn, account, err := profileLoggedIn(p)
		switch {
		case err != nil:
			pr.state = accountUnknown
		case !loggedIn:
			pr.state = accountLoggedOut
		default:
			pr.state = accountOK
		}
		pr.account = account
		if profileEngine(p) == engineClaude {
			pr.p.Name = in.rememberProfileEmail(p.Name, account)
		}
		probes = append(probes, pr)
	}

	cfg := in.config()
	def := defaultProfile(cfg).Name
	needsLogin := in.needsLoginSet()
	busy := in.busyBotsByProfile()

	var cards []accountCard
	for _, pr := range probes {
		p, ok := profileByKey(cfg, pr.p.Name)
		if !ok {
			p = pr.p
		}
		card := accountCard{Profile: p, State: pr.state, Usage: refreshProfileUsage(p),
			IsDefault: p.Name == def, Bots: busy[p.Name]}
		card.Disclaimer, _ = bypassAccepted(p)
		if card.State == accountOK && needsLogin[p.Name] {
			card.State = accountNeedsLogin
		}
		card.Account = firstNonEmpty(pr.account, p.Label)
		cards = append(cards, card)
	}
	return cards
}

// rememberProfileEmail records the address `claude auth status` reports for a
// profile and, once it is known, re-keys the profile by it. That is what makes
// the machine's pre-existing ~/.claude account show up as its email instead of
// "default", and what migrates a profile that predates emails as keys. The
// config dir never moves — only the key and the label change — and the turns
// already recorded against the old key follow it so /usage stays whole.
// It returns the key the profile is addressable by afterwards.
func (in *instance) rememberProfileEmail(name, account string) string {
	if p, ok := profileByKey(in.config(), name); ok && profileEngine(p) != engineClaude {
		return name
	}
	email := normalizeEmail(account)
	if !isAccountEmail(email) {
		return name
	}
	renamed := false
	updated := updateConfig(func(c *Config) bool {
		if c.Profiles == nil {
			c.Profiles = map[string]*Profile{}
		}
		current := c.Profiles[name]
		if current == nil {
			// The implicit profile has no entry yet. Register it with an EMPTY
			// config_dir: pinning CLAUDE_CONFIG_DIR at claude's own default
			// location would make it start a fresh .claude.json (profiles.go).
			if name != defaultProfileName || c.Profiles[email] != nil {
				return false
			}
			c.Profiles[email] = &Profile{Label: email}
			if c.DefaultProfile == "" || c.DefaultProfile == name {
				c.DefaultProfile = email
			}
			renamed = true
			return true
		}
		if name == email {
			if current.Label == email {
				return false
			}
			current.Label = email
			return true
		}
		if c.Profiles[email] != nil {
			// Two profiles claim the same account; leave the key alone and just
			// record what this one is, so the owner can see the clash.
			if current.Label == email {
				return false
			}
			current.Label = email
			return true
		}
		current.Label = email
		c.Profiles[email] = current
		delete(c.Profiles, name)
		if c.DefaultProfile == name {
			c.DefaultProfile = email
		}
		renamed = true
		return true
	})
	if updated == nil {
		return name
	}
	in.setConfig(updated)
	if renamed && name != email && in.db != nil {
		in.db.Model(&Turn{}).Where("profile = ?", name).Update("profile", email)
	}
	return email
}

func (in *instance) needsLoginSet() map[string]bool {
	if r, ok := in.runner.(*Runner); ok {
		return r.needsLoginSnapshot()
	}
	return map[string]bool{}
}

// busyBotsByProfile names the bots with a turn running on each profile.
func (in *instance) busyBotsByProfile() map[string][]string {
	var rows []struct {
		Profile string
		Name    string
	}
	in.db.Model(&Turn{}).Select("turns.profile as profile, bots.name as name").
		Joins("JOIN bots ON bots.id = turns.bot_id").
		Where("turns.status = ?", turnRunning).Scan(&rows)
	out := map[string][]string{}
	for _, r := range rows {
		out[r.Profile] = append(out[r.Profile], r.Name)
	}
	return out
}

// renderAccounts is the /account status body plus its buttons.
func renderAccounts(cards []accountCard) (string, [][]InlineKeyboardButton) {
	var sb strings.Builder
	sb.WriteString("<b>Accounts</b>\n")
	if len(cards) == 0 {
		sb.WriteString("None configured. <code>/account add &lt;identity&gt; &lt;engine&gt;</code>")
		return sb.String(), nil
	}
	var buttons [][]InlineKeyboardButton
	for _, c := range cards {
		// An account is its email here: the config dir behind it is an
		// implementation detail the owner never has to know (DESIGN §8).
		name := accountDisplay(c.Profile)
		marker := ""
		if c.IsDefault {
			marker = " ⭐"
		}
		eng := profileEngine(c.Profile)
		fmt.Fprintf(&sb, "\n<b>%s</b>%s — %s · %s\n", htmlEscape(name), marker, engineLabel(eng), c.State.icon())
		if acct := strings.TrimSpace(c.Account); acct != "" && normalizeEmail(acct) != normalizeEmail(name) && acct != name {
			fmt.Fprintf(&sb, "  %s\n", htmlEscape(acct))
		}
		if eng == engineClaude {
			fmt.Fprintf(&sb, "  usage: 5h %s · 7d %s\n",
				pct(c.Usage.FiveHour, c.Usage.FiveHourKnown), pct(c.Usage.SevenDay, c.Usage.SevenDayKnown))
		}
		if len(c.Bots) > 0 {
			fmt.Fprintf(&sb, "  running: %s\n", htmlEscape(strings.Join(c.Bots, ", ")))
		} else {
			sb.WriteString("  running: nothing\n")
		}
		if eng == engineClaude && !c.Disclaimer {
			sb.WriteString("  ⚠️ bypass disclaimer not accepted\n")
		}
		target := accountTarget(c.Profile.Name)
		row := []InlineKeyboardButton{{Text: "🔑 Relogin " + name, CallbackData: "account:login:" + target}}
		if !c.IsDefault {
			row = append(row, InlineKeyboardButton{Text: "⭐ Default", CallbackData: "account:default:" + target})
		}
		buttons = append(buttons, row)
	}
	buttons = append(buttons, []InlineKeyboardButton{{Text: "🔄 Refresh", CallbackData: "account:refresh:-"}})
	return sb.String(), buttons
}

// ---------------------------------------------------------------------------
// Commands
// ---------------------------------------------------------------------------

func (in *instance) handleAccountCommand(msg *TelegramMessage, rest string) {
	sub, arg := splitFirstWord(rest)
	arg = strings.TrimSpace(arg)
	switch strings.ToLower(sub) {
	case "", "status", "list":
		in.postAccounts(msg.Chat.ID, msg.MessageThreadID)

	case "add":
		if arg == "" {
			in.reply(msg, accountAddUsage)
			return
		}
		in.accountAdd(msg, arg)

	case "login":
		if arg == "" {
			in.reply(msg, "Usage: /account login &lt;identity&gt; [engine]")
			return
		}
		in.startLogin(msg.Chat.ID, msg.MessageThreadID, arg)

	case "remove":
		if arg == "" {
			in.reply(msg, "Usage: /account remove &lt;identity&gt; [engine]")
			return
		}
		in.accountAskRemove(msg, arg)

	case "default":
		if arg == "" {
			in.reply(msg, "Usage: /account default &lt;identity&gt; [engine]")
			return
		}
		in.accountSetDefault(msg.Chat.ID, msg.MessageThreadID, arg)

	default:
		in.reply(msg, "Usage: /account [status] · add &lt;identity&gt; [engine] · login · remove · default")
	}
}

const accountAddUsage = "Usage: /account add &lt;identity&gt; &lt;engine&gt;\n" +
	"<code>/account add you@example.com claude</code>\n" +
	"<code>/account add work grok</code>\n" +
	"<code>/account add lab agy</code>\n" +
	"<code>/account add openai codex</code>"

// parseAccountAdd reads `/account add` arguments. Engine is part of add, not a
// later `/engine` flip. Accepted shapes:
//
//	you@example.com              → claude (legacy)
//	you@example.com claude
//	claude you@example.com
//	work grok
//	grok work
//	lab agy / antigravity lab
//	openai codex / codex openai
func parseAccountAdd(arg string) (identity, engine string, err error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "", "", errAccountAddUsage
	}
	first, rest := splitFirstWord(arg)
	second, extra := splitFirstWord(rest)
	if strings.TrimSpace(extra) != "" {
		return "", "", errAccountAddUsage
	}
	if e, ok := knownEngineName(first); ok {
		if second == "" {
			return "", "", errAccountAddUsage
		}
		return second, e, nil
	}
	if second == "" {
		return first, engineClaude, nil
	}
	if e, ok := knownEngineName(second); ok {
		return first, e, nil
	}
	return "", "", fmt.Errorf("unknown engine %q (use claude, grok, antigravity or codex)", second)
}

var errAccountAddUsage = fmt.Errorf("usage")

func (in *instance) postAccounts(chatID, topicID int64) {
	cfg := in.config()
	body, buttons := renderAccounts(in.collectAccountCards())
	if cfg.BotToken == "" {
		return
	}
	if len(buttons) == 0 {
		_, _ = sendMessageHTMLGetID(cfg, chatID, topicID, body) // safe-ignore: a failed status card is not worth failing the command
		return
	}
	_, _ = sendMessageKeyboardGetID(cfg, chatID, topicID, body, buttons) // safe-ignore: same
}

// accountAdd registers a new account for one engine and starts that engine's
// login flow. Engine is stored on the profile; turns pick from the matching
// pool. Claude identities are emails; Grok/Antigravity accept a short name.
func (in *instance) accountAdd(msg *TelegramMessage, arg string) {
	identity, engine, err := parseAccountAdd(arg)
	if err != nil {
		if errors.Is(err, errAccountAddUsage) {
			in.reply(msg, accountAddUsage)
			return
		}
		in.reply(msg, htmlEscape(err.Error()))
		return
	}
	canon := normalizeEmail(identity)
	if engine == engineClaude && !isAccountEmail(canon) {
		in.reply(msg, "Claude accounts are identified by email: "+
			"<code>/account add you@example.com claude</code>")
		return
	}
	if engine != engineClaude && !isAccountIdentity(canon) {
		in.reply(msg, "That identity is not a usable name. Use a short label or email, no spaces or slashes.")
		return
	}
	cfg := in.config()
	if p, exists := profileByIdentityEngine(cfg, canon, engine); exists {
		in.reply(msg, "That account already exists. Use <code>/account login "+
			htmlEscape(accountAddress(p))+"</code>.")
		return
	}
	key := accountMapKey(cfg, canon, engine)
	dir := accountDirFor(cfg, engine, canon)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		in.reply(msg, "Could not create the config dir: "+htmlEscape(err.Error()))
		return
	}
	updated := updateConfig(func(c *Config) bool {
		if c.Profiles == nil {
			c.Profiles = map[string]*Profile{}
		}
		// An explicit config dir per profile is what keeps two accounts'
		// credentials apart; see profiles.go.
		c.Profiles[key] = &Profile{Engine: engine, ConfigDir: dir, Label: canon}
		if c.DefaultProfile == "" {
			c.DefaultProfile = key
		}
		return true
	})
	if updated == nil {
		in.reply(msg, "Could not write the configuration.")
		return
	}
	in.setConfig(updated)
	// Claude profiles share transcripts so same-engine failover can resume a
	// conversation this profile never started (DESIGN §4). Other engines keep
	// their own isolated homes.
	if engine == engineClaude {
		linkSharedProjects(updated)
	}
	in.reply(msg, "➕ Added <b>"+htmlEscape(canon)+"</b> ("+htmlEscape(engineLabel(engine))+"). Logging it in now...")
	in.startLogin(msg.Chat.ID, msg.MessageThreadID, key)
}

func (in *instance) accountSetDefault(chatID, topicID int64, name string) {
	cfg := in.config()
	p, hint, ok := addressedAccount(cfg, name)
	if !ok {
		if hint != "" {
			in.post(chatID, topicID, hint)
			return
		}
		in.post(chatID, topicID, "No account <b>"+htmlEscape(name)+"</b>.")
		return
	}
	key := p.Name
	updated := updateConfig(func(c *Config) bool { c.DefaultProfile = key; return true })
	if updated == nil {
		in.post(chatID, topicID, "Could not write the configuration.")
		return
	}
	in.setConfig(updated)
	in.post(chatID, topicID, "⭐ Default account: <b>"+htmlEscape(accountDisplay(p))+"</b>")
}

// accountAskRemove refuses outright while the profile is carrying a turn, and
// otherwise asks for a button confirmation before unregistering it.
func (in *instance) accountAskRemove(msg *TelegramMessage, name string) {
	cfg := in.config()
	p, hint, ok := addressedAccount(cfg, name)
	if !ok {
		if hint != "" {
			in.reply(msg, hint)
			return
		}
		in.reply(msg, "No account <b>"+htmlEscape(name)+"</b>.")
		return
	}
	if cfg.Profiles[p.Name] == nil {
		in.reply(msg, "No account <b>"+htmlEscape(name)+"</b>.")
		return
	}
	shown := accountDisplay(p)
	if busy := in.busyBotsByProfile()[p.Name]; len(busy) > 0 {
		in.reply(msg, "🚫 <b>"+htmlEscape(shown)+"</b> is running a turn for "+
			htmlEscape(strings.Join(busy, ", "))+". Wait, or /stop them first.")
		return
	}
	body := "Remove account <b>" + htmlEscape(shown) + "</b>? Its config dir stays on disk."
	buttons := [][]InlineKeyboardButton{{
		{Text: "🗑 Remove", CallbackData: "account:removeok:" + accountTarget(p.Name)},
		{Text: "Cancel", CallbackData: "account:cancel:-"},
	}}
	_, _ = sendMessageKeyboardGetID(cfg, msg.Chat.ID, msg.MessageThreadID, body, buttons) // safe-ignore: the command is a no-op if this fails
}

func (in *instance) accountRemove(name string) string {
	shown := name
	if p, ok := profileByKey(in.config(), name); ok {
		name, shown = p.Name, accountDisplay(p)
	} else if p, ok := profileByName(in.config(), name); ok {
		name, shown = p.Name, accountDisplay(p)
	}
	// Re-check under the confirmation: a turn may have started meanwhile.
	if busy := in.busyBotsByProfile()[name]; len(busy) > 0 {
		return "🚫 <b>" + htmlEscape(shown) + "</b> started a turn for " + htmlEscape(strings.Join(busy, ", ")) + "; not removed."
	}
	updated := updateConfig(func(c *Config) bool {
		if c.Profiles == nil || c.Profiles[name] == nil {
			return false
		}
		delete(c.Profiles, name)
		if c.DefaultProfile == name {
			c.DefaultProfile = ""
			for n := range c.Profiles {
				if c.DefaultProfile == "" || n < c.DefaultProfile {
					c.DefaultProfile = n
				}
			}
		}
		return true
	})
	if updated == nil {
		return "Could not write the configuration."
	}
	in.setConfig(updated)
	return "🗑 Removed <b>" + htmlEscape(shown) + "</b> (its config dir was left on disk)."
}

// handleAccountCallback answers the buttons on the account cards.
func (in *instance) handleAccountCallback(cb *CallbackQuery, parts []string) {
	if len(parts) < 3 || cb.Message == nil {
		return
	}
	chatID, topicID := cb.Message.Chat.ID, cb.Message.MessageThreadID
	if parts[1] == "refresh" {
		in.postAccounts(chatID, topicID)
		return
	}
	if parts[1] == "cancel" {
		in.editCallbackMessage(cb, "Cancelled.")
		return
	}
	// Every other button carries a profile reference, which is an email or a
	// digest standing in for one too long for callback_data (profiles.go).
	p, ok := resolveAccountTarget(in.config(), parts[2])
	if !ok {
		in.editCallbackMessage(cb, "That account is gone.")
		return
	}
	switch parts[1] {
	case "login":
		in.startLogin(chatID, topicID, p.Name)
	case "default":
		in.accountSetDefault(chatID, topicID, p.Name)
	case "removeok":
		in.editCallbackMessage(cb, in.accountRemove(p.Name))
	}
}

// ---------------------------------------------------------------------------
// The login flow
// ---------------------------------------------------------------------------

// loginWaiter is an in-flight `/account login`: while one exists, the owner's
// next plain message in the same chat is the code, not a message for a bot.
type loginWaiter struct {
	chatID  int64
	topicID int64
	profile string
	codes   chan string
	cancel  context.CancelFunc
}

// loginState is the instance's at-most-one-at-a-time login slot.
type loginState struct {
	mu      sync.Mutex
	waiting *loginWaiter
}

// takeLoginCode hands a message to the pending login if there is one in this
// chat, and reports whether it was consumed.
func (in *instance) takeLoginCode(chatID, topicID int64, text string) bool {
	in.login.mu.Lock()
	w := in.login.waiting
	in.login.mu.Unlock()
	if w == nil || w.chatID != chatID || w.topicID != topicID {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(text), "/cancel") {
		w.cancel()
		return true
	}
	select {
	case w.codes <- text:
		return true
	default:
		return false // a code is already being verified; let the message through
	}
}

// telegramPrompter posts the login URL and waits for the code in the chat.
type telegramPrompter struct {
	in      *instance
	chatID  int64
	topicID int64
	waiter  *loginWaiter
}

func (t telegramPrompter) Progress(text string) {
	t.in.post(t.chatID, t.topicID, "⏳ "+htmlEscape(text))
}

func (t telegramPrompter) AskForCode(ctx context.Context, url string) (string, error) {
	t.in.post(t.chatID, t.topicID, "🔗 Open this on the device with the right account, "+
		"then send me the code it gives you if asked (or /cancel):\n\n"+htmlEscape(url))
	select {
	case code := <-t.waiter.codes:
		return code, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// startLogin runs the login + disclaimer flow for a profile in the background,
// reporting into the chat it was started from.
func (in *instance) startLogin(chatID, topicID int64, name string) {
	cfg := in.config()
	p, hint, ok := addressedAccount(cfg, name)
	if !ok {
		if hint != "" {
			in.post(chatID, topicID, hint)
			return
		}
		addHint := ""
		identity, _ := parseAccountRef(name)
		if identity == "" {
			identity = strings.ToLower(strings.TrimSpace(name))
		}
		if isAccountEmail(identity) || isAccountIdentity(identity) {
			addHint = " Add it with <code>/account add " + htmlEscape(identity) + " &lt;engine&gt;</code>."
		}
		in.post(chatID, topicID, "No account <b>"+htmlEscape(name)+"</b>."+addHint)
		return
	}
	name = p.Name

	ctx, cancel := context.WithTimeout(context.Background(), ptyLoginTimeout)
	waiter := &loginWaiter{chatID: chatID, topicID: topicID, profile: name, codes: make(chan string, 1), cancel: cancel}

	in.login.mu.Lock()
	if in.login.waiting != nil {
		busy := in.login.waiting.profile
		in.login.mu.Unlock()
		cancel()
		in.post(chatID, topicID, "A login for <b>"+htmlEscape(busy)+"</b> is already in progress. Finish it or send /cancel.")
		return
	}
	in.login.waiting = waiter
	in.login.mu.Unlock()

	go func() {
		defer cancel()
		defer func() {
			in.login.mu.Lock()
			if in.login.waiting == waiter {
				in.login.waiting = nil
			}
			in.login.mu.Unlock()
		}()

		in.post(chatID, topicID, "🔑 Starting "+htmlEscape(loginCommandLabel(p))+" for <b>"+htmlEscape(accountDisplay(p))+"</b>...")
		account, err := runAccountLogin(ctx, in.ptyStart(), p, telegramPrompter{in: in, chatID: chatID, topicID: topicID, waiter: waiter})
		if err != nil {
			in.post(chatID, topicID, "❌ Login failed: "+htmlEscape(truncate(err.Error(), 500)))
			return
		}
		// The browser may have been signed in as somebody else: the account is
		// whatever auth status reports, not what the owner typed. Re-key is
		// Claude-only (emails).
		if profileEngine(p) == engineClaude {
			name = in.reconcileLoginEmail(chatID, topicID, name, account)
		} else if hint := strings.TrimSpace(account); hint != "" && hint != name {
			in.post(chatID, topicID, "ℹ️ Logged in as <b>"+htmlEscape(hint)+"</b>.")
		}
		in.post(chatID, topicID, "✅ <b>"+htmlEscape(name)+"</b> is logged in ("+htmlEscape(engineLabel(profileEngine(p)))+").")

		// Claude is only usable once the disclaimer is accepted too. That is
		// one settings.json key, written straight into the profile's config
		// dir — no second claude process, no TUI to answer (DESIGN §14.23).
		// Other engines have no equivalent gate.
		if profileEngine(p) == engineClaude {
			target := p
			if fresh, ok := profileByKey(in.config(), name); ok {
				target = fresh
			}
			if err := acceptBypassDisclaimer(target); err != nil {
				in.post(chatID, topicID, "⚠️ Could not accept the bypass disclaimer: "+
					htmlEscape(truncate(err.Error(), 300))+"\n<code>"+htmlEscape(bypassDisclaimerHint(target))+"</code>")
			} else {
				in.post(chatID, topicID, "🛡 Bypass disclaimer accepted for <b>"+htmlEscape(name)+"</b>.")
			}
		}
		in.clearNeedsLogin(name)
		in.postAccounts(chatID, topicID)
	}()
}

// reconcileLoginEmail stores the account the login actually produced. The owner
// types the address they MEANT to log in as; if the device was signed in as
// somebody else the profile is stored under the real address and the owner is
// told, because otherwise two accounts would silently be one.
func (in *instance) reconcileLoginEmail(chatID, topicID int64, typed, reported string) string {
	email := normalizeEmail(reported)
	if !isAccountEmail(email) {
		return typed // nothing usable came back; keep addressing it as it is
	}
	key := in.rememberProfileEmail(typed, email)
	if isAccountEmail(typed) && normalizeEmail(typed) != email {
		in.post(chatID, topicID, "⚠️ You typed <b>"+htmlEscape(normalizeEmail(typed))+
			"</b> but logged in as <b>"+htmlEscape(email)+"</b>; the account is stored as <b>"+htmlEscape(email)+"</b>.")
	}
	return key
}

// ptyStart is the PTY starter the flows use. It is a method so tests can point
// the instance at a fake script instead of the claude binary.
func (in *instance) ptyStart() ptyStarter {
	if in.pty != nil {
		return in.pty
	}
	return startPTY
}

func (in *instance) clearNeedsLogin(name string) {
	if r, ok := in.runner.(*Runner); ok {
		r.clearNeedsLogin(name)
	}
}

// post writes into an arbitrary chat/topic (the account flow reports wherever
// it was started, which may be the owner's DM or a topic).
func (in *instance) post(chatID, topicID int64, html string) {
	cfg := in.config()
	if cfg.BotToken == "" || chatID == 0 {
		return
	}
	if _, err := sendMessageHTMLGetID(cfg, chatID, topicID, html); err != nil {
		hookLog("account post failed: %v", err)
	}
}
