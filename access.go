package main

import (
	"fmt"
	"strconv"
	"strings"
)

// Access control (DESIGN §8 "Access control", §12).
//
// Bots run with bypass permissions on the owner's machine, so the chat IS the
// trust boundary: this file is what stops anyone who finds the bot from
// getting a shell. The policy is default-deny by Telegram user id — the owner
// from config.chat_id is always allowed, everybody else must be listed in
// config.allowed_user_ids (loaded at process start) before a single update
// from them is looked at. Unknown DMs are dropped in silence. Group messages
// are dropped. There is no pairing, no code, no owner ping.
//
// Changing the list is `ccc config set allowed_user_ids …` and a listen restart.

// accessRole is what the gate decided about an inbound update.
type accessRole int

const (
	roleDenied accessRole = iota // drop the update
	roleUser                     // allowed: may talk to bots
	roleOwner                    // the owner: may also run the owner commands
)

// classifyAccess is the whole policy: a pure function over the config loaded
// at start. It never writes and never talks to SQLite.
func classifyAccess(config *Config, userID int64) accessRole {
	if userID == 0 {
		return roleDenied // a Telegram update with no sender is not something to act on
	}
	if config == nil || config.ChatID == 0 {
		// Not bootstrapped yet: nobody is the owner, so nobody is allowed.
		// `ccc config set chat_id <id>` (or `ccc setup`) is what opens the door.
		return roleDenied
	}
	if userID == config.ChatID {
		return roleOwner
	}
	for _, id := range config.AllowedUserIDs {
		if id == userID {
			return roleUser
		}
	}
	return roleDenied
}

// gate is the single entry point every inbound update goes through. Denied
// updates are dropped with no reply, no DB row, and no owner notify.
func (in *instance) gate(userID int64) accessRole {
	return classifyAccess(in.config(), userID)
}

// handleAccessCommand implements `/access …` (owner only; the caller checks).
// The list is read-only: mutating it is `ccc config set allowed_user_ids`.
func (in *instance) handleAccessCommand(msg *TelegramMessage, rest string) {
	sub, _ := splitFirstWord(rest)
	switch strings.ToLower(sub) {
	case "", "list":
		in.reply(msg, in.renderAccessList())
	case "add", "approve", "allow", "remove", "rm", "block":
		in.reply(msg, "/access "+htmlEscape(sub)+" does not grant anyone. Remote grant is disabled. The allowlist is whitelist-only: <code>ccc config set allowed_user_ids &lt;id,id,…&gt;</code> (restart listen).")
	default:
		in.reply(msg, "Usage: /access [list]\nRemote grant is disabled. The allowlist is <code>ccc config set allowed_user_ids &lt;id,id,…&gt;</code> (restart listen).")
	}
}

func (in *instance) renderAccessList() string {
	cfg := in.config()
	var sb strings.Builder
	sb.WriteString("<b>Access</b>\n")
	fmt.Fprintf(&sb, "• owner <code>%d</code>\n", cfg.ChatID)
	n := 0
	for _, id := range cfg.AllowedUserIDs {
		if id == 0 || (cfg.ChatID != 0 && id == cfg.ChatID) {
			continue
		}
		fmt.Fprintf(&sb, "• allowed <code>%d</code>\n", id)
		n++
	}
	if n == 0 {
		sb.WriteString("<i>Nobody else. Unknown DMs are dropped in silence.</i>")
	}
	return sb.String()
}

// parseAllowedUserIDs reads a comma- or space-separated list of Telegram user
// ids for `ccc config set allowed_user_ids`. Empty clears the list. Zero and
// non-numeric values are refused rather than silently dropping a typo.
func parseAllowedUserIDs(value string) ([]int64, error) {
	fields := splitList(value)
	if len(fields) == 0 {
		return nil, nil
	}
	out := make([]int64, 0, len(fields))
	seen := make(map[int64]struct{}, len(fields))
	for _, f := range fields {
		n, err := strconv.ParseInt(f, 10, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("allowed_user_ids must be telegram user ids, got %q", f)
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out, nil
}

func formatAllowedUserIDs(ids []int64) string {
	if len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}
