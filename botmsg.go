package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"gorm.io/gorm"
)

// queueBotMessage is the one path a bot-to-bot message takes: an inbox row
// the target reads on its next turn. The body is not posted to Telegram —
// the owner must not see General↔session prompts or reports (DESIGN §3.2).
func queueBotMessage(db *gorm.DB, from *Bot, toName, body string, wake bool) (*Bot, *InboxMessage, error) {
	return enqueueBotMessage(db, from, toName, body, wake, false)
}

// queueOwnerRelay is a worker → General report the owner must hear. Quiet
// stays: the body is not dumped into Telegram. listen wakes General; if
// General does not post the result, listen posts a short fallback.
func queueOwnerRelay(db *gorm.DB, from *Bot, body string) (*Bot, *InboxMessage, error) {
	chief, err := generalBot(db)
	if err != nil {
		return nil, nil, err
	}
	return enqueueBotMessage(db, from, chief.Name, body, true, true)
}

func enqueueBotMessage(db *gorm.DB, from *Bot, toName, body string, wake, relay bool) (*Bot, *InboxMessage, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, nil, fmt.Errorf("message text is empty")
	}
	if from == nil {
		return nil, nil, fmt.Errorf("unknown sender")
	}
	target, err := botByName(db, strings.TrimSpace(toName))
	if err != nil {
		return nil, nil, fmt.Errorf("no live bot named %q", toName)
	}
	if target.ID == from.ID {
		return nil, nil, fmt.Errorf("you cannot send a message to yourself")
	}
	msg := InboxMessage{ToBotID: target.ID, FromBotID: &from.ID, Text: body, Wake: wake, Relay: relay}
	if err := db.Create(&msg).Error; err != nil {
		return nil, nil, fmt.Errorf("could not queue the message: %w", err)
	}
	return target, &msg, nil
}

// runTellCommand is `ccc tell [--no-wake] <bot> [text…]` — the Grok/agy
// substitute for send_to_bot. Sender is CCC_BOT_ID (set on every turn) or the
// bot that owns the working directory.
func runTellCommand(args []string) error {
	wake := true
	var rest []string
	for _, a := range args {
		if a == "--no-wake" {
			wake = false
			continue
		}
		rest = append(rest, a)
	}
	if len(rest) < 1 {
		return fmt.Errorf("usage: ccc tell [--no-wake] <bot> [text]")
	}
	target := rest[0]
	body := strings.TrimSpace(strings.Join(rest[1:], " "))
	if body == "" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		body = strings.TrimSpace(string(b))
	}
	if body == "" {
		return fmt.Errorf("usage: ccc tell [--no-wake] <bot> [text]")
	}

	config, err := loadConfig()
	if err != nil || config == nil {
		return fmt.Errorf("no config found: %w", err)
	}
	path := os.Getenv("CCC_DB")
	if path == "" {
		path = dbPath(config)
	}
	db, err := openStore(path)
	if err != nil {
		return fmt.Errorf("open the ccc database: %w", err)
	}
	defer closeStore(db)

	from, err := senderBot(db)
	if err != nil {
		return err
	}
	to, _, err := queueBotMessage(db, from, target, body, wake)
	if err != nil {
		return err
	}
	fmt.Printf("queued for %s\n", to.Name)
	return nil
}

func senderBot(db *gorm.DB) (*Bot, error) {
	if id := strings.TrimSpace(os.Getenv("CCC_BOT_ID")); id != "" {
		n, err := strconv.ParseInt(id, 10, 64)
		if err == nil && n > 0 {
			return botByID(db, n)
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("cannot resolve the working directory: %w", err)
	}
	b, err := botByCwd(db, cwd)
	if err != nil {
		return nil, fmt.Errorf("no bot owns %s — run this from a bot's working directory (or set CCC_BOT_ID)", cwd)
	}
	return b, nil
}
