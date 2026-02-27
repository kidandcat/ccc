package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// MessageRecord tracks the delivery state of a single message
type MessageRecord struct {
	ID                string `json:"id"`
	Session           string `json:"session"`
	Type              string `json:"type"`               // user_prompt / assistant_text / tool_call / notification
	Text              string `json:"text"`
	Origin            string `json:"origin"`              // terminal / telegram / claude
	TerminalDelivered bool   `json:"terminal_delivered"`
	TelegramDelivered bool   `json:"telegram_delivered"`
	TelegramMsgID     int64  `json:"telegram_msg_id,omitempty"`
	Timestamp         int64  `json:"timestamp"`
}

var (
	dbOnce     sync.Once
	dbInstance *sql.DB
	dbPath     = func() string { return filepath.Join(cacheDir(), "ccc.db") }
)

// openDB opens (or creates) the SQLite database and ensures tables exist.
// Safe to call multiple times — uses sync.Once internally.
func openDB() *sql.DB {
	dbOnce.Do(func() {
		path := dbPath()
		os.MkdirAll(filepath.Dir(path), 0755)


		db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)")
		if err != nil {
			hookLog("db: open failed: %v", err)
			return
		}

		// Create tables
		for _, stmt := range []string{
			`CREATE TABLE IF NOT EXISTS messages (
				id                 TEXT PRIMARY KEY,
				session            TEXT NOT NULL,
				type               TEXT NOT NULL,
				text               TEXT,
				origin             TEXT,
				terminal_delivered INTEGER DEFAULT 0,
				telegram_delivered INTEGER DEFAULT 0,
				telegram_msg_id    INTEGER DEFAULT 0,
				created_at         INTEGER NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session)`,
			`CREATE INDEX IF NOT EXISTS idx_messages_undelivered_tg ON messages(session, telegram_delivered) WHERE telegram_delivered = 0`,
			`CREATE INDEX IF NOT EXISTS idx_messages_undelivered_tm ON messages(session, terminal_delivered) WHERE terminal_delivered = 0`,
			`CREATE TABLE IF NOT EXISTS tool_state (
				session         TEXT PRIMARY KEY,
				telegram_msg_id INTEGER DEFAULT 0,
				tools_json      TEXT DEFAULT '[]'
			)`,
		} {
			if _, err := db.Exec(stmt); err != nil {
				hookLog("db: create table failed: %v", err)
			}
		}

		dbInstance = db
	})
	return dbInstance
}

// closeDB closes the database connection
func closeDB() {
	if dbInstance != nil {
		dbInstance.Close()
	}
}

// appendMessage inserts a message record. Duplicate IDs are silently ignored.
func appendMessage(rec *MessageRecord) error {
	db := openDB()
	if db == nil {
		return fmt.Errorf("db not open")
	}
	if rec.Timestamp == 0 {
		rec.Timestamp = time.Now().UnixMilli()
	}
	_, err := db.Exec(
		`INSERT OR IGNORE INTO messages (id, session, type, text, origin, terminal_delivered, telegram_delivered, telegram_msg_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.Session, rec.Type, rec.Text, rec.Origin,
		boolToInt(rec.TerminalDelivered), boolToInt(rec.TelegramDelivered),
		rec.TelegramMsgID, rec.Timestamp,
	)
	return err
}

// updateDelivery updates a specific delivery field for a message
func updateDelivery(session, msgID, field string, value any) error {
	db := openDB()
	if db == nil {
		return fmt.Errorf("db not open")
	}
	var col string
	switch field {
	case "terminal_delivered":
		col = "terminal_delivered"
	case "telegram_delivered":
		col = "telegram_delivered"
	case "telegram_msg_id":
		col = "telegram_msg_id"
	default:
		return fmt.Errorf("unknown field: %s", field)
	}
	_, err := db.Exec(
		fmt.Sprintf(`UPDATE messages SET %s = ? WHERE id = ?`, col),
		value, msgID,
	)
	return err
}

// isDelivered checks if a message has been delivered to the given target
func isDelivered(session, msgID, target string) bool {
	db := openDB()
	if db == nil {
		return false
	}
	var col string
	switch target {
	case "telegram":
		col = "telegram_delivered"
	case "terminal":
		col = "terminal_delivered"
	default:
		return false
	}
	var delivered int
	err := db.QueryRow(
		fmt.Sprintf(`SELECT %s FROM messages WHERE id = ?`, col),
		msgID,
	).Scan(&delivered)
	if err != nil {
		return false
	}
	return delivered != 0
}

// findUndelivered returns messages not yet delivered to the given target
func findUndelivered(session, target string) []*MessageRecord {
	db := openDB()
	if db == nil {
		return nil
	}
	var col string
	switch target {
	case "telegram":
		col = "telegram_delivered"
	case "terminal":
		col = "terminal_delivered"
	default:
		return nil
	}
	rows, err := db.Query(
		fmt.Sprintf(`SELECT id, session, type, text, origin, terminal_delivered, telegram_delivered, telegram_msg_id, created_at
		 FROM messages WHERE session = ? AND %s = 0 ORDER BY created_at`, col),
		session,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var result []*MessageRecord
	for rows.Next() {
		var r MessageRecord
		var termDel, tgDel int
		if err := rows.Scan(&r.ID, &r.Session, &r.Type, &r.Text, &r.Origin,
			&termDel, &tgDel, &r.TelegramMsgID, &r.Timestamp); err != nil {
			continue
		}
		r.TerminalDelivered = termDel != 0
		r.TelegramDelivered = tgDel != 0
		result = append(result, &r)
	}
	return result
}

// confirmTerminalDelivery marks the most recent unconfirmed Telegram message as delivered to terminal.
// Called from UserPromptSubmit hook when Claude confirms it received a prompt.
func confirmTerminalDelivery(session, promptText string) {
	db := openDB()
	if db == nil {
		return
	}
	cutoff := time.Now().Add(-30 * time.Second).UnixMilli()
	_, err := db.Exec(
		`UPDATE messages SET terminal_delivered = 1
		 WHERE session = ? AND origin = 'telegram' AND type = 'user_prompt'
		 AND terminal_delivered = 0 AND created_at > ?`,
		session, cutoff,
	)
	if err != nil {
		hookLog("db: confirmTerminalDelivery failed: %v", err)
	}
}

// hasUnconfirmedPrompt checks if a Telegram prompt is still unconfirmed for terminal delivery
func hasUnconfirmedPrompt(session, msgID string) bool {
	db := openDB()
	if db == nil {
		return false
	}
	var termDel int
	err := db.QueryRow(
		`SELECT terminal_delivered FROM messages WHERE id = ?`, msgID,
	).Scan(&termDel)
	if err != nil {
		return false
	}
	return termDel == 0
}

// --- Tool State ---

// ToolState tracks tool calls and the Telegram message ID for live updates
type ToolState struct {
	MsgID int64      `json:"msg_id"`
	Tools []ToolCall `json:"tools"`
}

type ToolCall struct {
	Name   string `json:"name"`
	Input  string `json:"input"`
	IsText bool   `json:"is_text,omitempty"`
	Time   int64  `json:"time,omitempty"`
}

func loadToolState(session string) *ToolState {
	db := openDB()
	if db == nil {
		return &ToolState{}
	}
	var msgID int64
	var toolsJSON string
	err := db.QueryRow(
		`SELECT telegram_msg_id, tools_json FROM tool_state WHERE session = ?`, session,
	).Scan(&msgID, &toolsJSON)
	if err != nil {
		return &ToolState{}
	}
	var tools []ToolCall
	json.Unmarshal([]byte(toolsJSON), &tools)
	return &ToolState{MsgID: msgID, Tools: tools}
}

func saveToolState(session string, state *ToolState) {
	db := openDB()
	if db == nil {
		return
	}
	toolsJSON, _ := json.Marshal(state.Tools)
	db.Exec(
		`INSERT OR REPLACE INTO tool_state (session, telegram_msg_id, tools_json) VALUES (?, ?, ?)`,
		session, state.MsgID, string(toolsJSON),
	)
}

func clearToolState(session string) {
	db := openDB()
	if db == nil {
		return
	}
	db.Exec(`DELETE FROM tool_state WHERE session = ?`, session)
}

// --- Helpers ---

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// contentHash returns a short hash of content for dedup IDs
func contentHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h[:4])
}
