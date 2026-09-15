package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// The v3 runtime state lives in one SQLite file (<data_dir>/ccc.db) accessed
// through GORM. The driver is github.com/glebarez/sqlite (modernc-based, pure
// Go): ccc ships cross-compiled release binaries, so a cgo driver is not an
// option. The schema is DESIGN.md §5.
//
// Two processes touch this database: `ccc listen` and the short-lived
// `ccc mcp` server spawned per turn. WAL + busy_timeout is what makes that
// safe; every write is short.

// Bot is one forum topic: an identity (name + role) with its own memory scope,
// workspace, engine (claude|grok|antigravity) and conversation session.
type Bot struct {
	ID          int64  `gorm:"primaryKey"`
	Name        string `gorm:"uniqueIndex;not null"`
	TopicID     int64  `gorm:"index"`
	Role        string
	Cwd         string
	SessionID   string
	Engine      string `gorm:"not null;default:claude"` // claude|grok|antigravity
	Status      string `gorm:"not null;default:idle"`   // idle|running|waiting|disabled
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ArchivedAt  *time.Time
	ParentBotID *int64
}

// Bot statuses.
const (
	botIdle     = "idle"
	botRunning  = "running"
	botWaiting  = "waiting"
	botDisabled = "disabled"
)

// Turn is one `claude -p` invocation: one input, one result.
//
// The (bot_id, created_at) index is what makes the maintenance job cheap: turn
// retention walks one bot's history at a time, newest first (DESIGN §7).
type Turn struct {
	ID         int64 `gorm:"primaryKey"`
	BotID      int64 `gorm:"index;index:idx_turn_bot_created,priority:1;not null"`
	SessionID  string
	Profile    string
	Source     string `gorm:"not null"` // user|bot|schedule|watch|routine|system|background
	Input      string
	Output     string
	Status     string `gorm:"index;not null"` // queued|running|done|failed
	StopReason string
	ErrorClass string
	StartedAt  *time.Time
	EndedAt    *time.Time
	UsageJSON  string
	CreatedAt  time.Time `gorm:"index:idx_turn_bot_created,priority:2"`
	// TriggerMessageID is the Telegram message that produced this turn, so the
	// runner can react to it with ✅ when the turn completes. 0 = no message.
	TriggerMessageID int64
}

// Turn statuses and sources.
const (
	turnQueued  = "queued"
	turnRunning = "running"
	turnDone    = "done"
	turnFailed  = "failed"

	sourceUser       = "user"
	sourceBot        = "bot"
	sourceSchedule   = "schedule"
	sourceWatch      = "watch"
	sourceRoutine    = "routine"
	sourceSystem     = "system"
	sourceBackground = "background"
)

// InboxMessage is a message addressed to a bot that has not been folded into a
// turn yet. FromBotID nil means the owner or ccc itself.
type InboxMessage struct {
	ID          int64 `gorm:"primaryKey"`
	ToBotID     int64 `gorm:"index;not null"`
	FromBotID   *int64
	Text        string
	Wake        bool
	CreatedAt   time.Time
	DeliveredAt *time.Time `gorm:"index"` // indexed: the maintenance job deletes by it
	TurnID      *int64
}

func (InboxMessage) TableName() string { return "inbox" }

// Memory is one remembered fact. Scope is user (everyone), project (keyed by
// path) or bot (keyed by bot id).
type Memory struct {
	ID             int64  `gorm:"primaryKey"`
	Scope          string `gorm:"not null;uniqueIndex:idx_mem_key,priority:1"`
	ScopeKey       string `gorm:"not null;uniqueIndex:idx_mem_key,priority:2"`
	Key            string `gorm:"column:key;not null;uniqueIndex:idx_mem_key,priority:3"`
	Text           string
	CreatedByBotID *int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Memory scopes.
const (
	scopeUser    = "user"
	scopeProject = "project"
	scopeBot     = "bot"
)

// Project is the registry entry for a code base a bot works on.
type Project struct {
	ID          int64  `gorm:"primaryKey"`
	Path        string `gorm:"uniqueIndex;not null"`
	Name        string
	Description string
	Stack       string
	DeployNotes string
	UpdatedAt   time.Time
}

// Watch is a deterministic command re-run on an interval (Phase 2b).
type Watch struct {
	ID         int64 `gorm:"primaryKey"`
	BotID      int64 `gorm:"index;not null"`
	Name       string
	Command    string
	IntervalS  int
	LastHash   string
	LastOutput string
	LastRunAt  *time.Time
	Enabled    bool
}

// Schedule is a self-wakeup (Phase 2b). A non-empty Name makes it a routine:
// timezone-aware cron, upserted by name, ⏰ in the topic when it fires.
type Schedule struct {
	ID            int64  `gorm:"primaryKey"`
	BotID         int64  `gorm:"index;not null"`
	Name          string `gorm:"index"`
	FireAt        time.Time
	Note          string
	RecurringCron string
	Timezone      string
	FiredAt       *time.Time
}

// Question is an ask_owner round trip: asked during a turn, answered later by
// a button tap or a reply in the topic.
type Question struct {
	ID             int64 `gorm:"primaryKey"`
	BotID          int64 `gorm:"index;not null"`
	TurnID         *int64
	Question       string
	OptionsJSON    string
	Answer         string
	AskedMessageID int64 `gorm:"index"`
	CreatedAt      time.Time
	AnsweredAt     *time.Time `gorm:"index"` // indexed: the maintenance job deletes by it
}

// MemoryArchive holds the memories one compaction replaced, so `/memory
// restore <compaction_id>` can put them back (DESIGN §7). One compaction is
// one scope and one CompactionID; the ids are dense integers because the owner
// types them into Telegram.
type MemoryArchive struct {
	ID              int64 `gorm:"primaryKey"`
	CompactionID    int64 `gorm:"index;not null"`
	ArchivedAt      time.Time
	Scope           string `gorm:"not null"`
	ScopeKey        string `gorm:"not null"`
	Key             string `gorm:"column:key;not null"`
	Text            string
	CreatedByBotID  *int64
	MemoryCreatedAt time.Time
	MemoryUpdatedAt time.Time
}

func (MemoryArchive) TableName() string { return "memories_archive" }

// Access is the pairing/allowlist table (DESIGN §5/§8). Replies is not in
// DESIGN's column list: it is what implements "at most two replies to a
// stranger, then silence", which otherwise has nowhere to live.
type Access struct {
	TelegramUserID int64 `gorm:"primaryKey"`
	Display        string
	State          string `gorm:"index"` // pending|approved|blocked
	PairCode       string `gorm:"index"`
	CodeExpiresAt  *time.Time
	Replies        int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (Access) TableName() string { return "access" }

// BackgroundJob is a long-running shell command owned by a bot (DESIGN §7).
// It runs outside the conversational turn: status here is independent of
// turns.status, so the topic stays responsive while the job works.
type BackgroundJob struct {
	ID              int64 `gorm:"primaryKey"`
	BotID           int64 `gorm:"index;not null"`
	Name            string
	Status          string `gorm:"index;not null"` // queued|running|done|failed
	Kind            string // "shell"
	Command         string
	PID             int `gorm:"column:pid"`
	ExitCode        *int
	Output          string
	Error           string
	CancelRequested bool
	CreatedByTurnID *int64
	StartedAt       *time.Time
	EndedAt         *time.Time `gorm:"index"`
	CreatedAt       time.Time
}

func (BackgroundJob) TableName() string { return "background_jobs" }

// Background job statuses and the only kind v3 implements.
const (
	jobQueued    = "queued"
	jobRunning   = "running"
	jobDone      = "done"
	jobFailed    = "failed"
	jobKindShell = "shell"
)

// Setting is an instance setting editable from Telegram.
type Setting struct {
	Key   string `gorm:"column:key;primaryKey"`
	Value string
}

// allModels is the AutoMigrate list; the whole DESIGN §5 schema is created up
// front even where Phase 2a does not use a table yet.
func allModels() []any {
	return []any{
		&Bot{}, &Turn{}, &InboxMessage{}, &Memory{}, &MemoryArchive{}, &Project{},
		&Watch{}, &Schedule{}, &Question{}, &Access{}, &Setting{}, &BackgroundJob{},
	}
}

// ftsAvailable records whether the memories FTS5 index exists. modernc's SQLite
// build normally has FTS5, but the driver is swappable and an older build may
// not, so recall() falls back to LIKE when this is false.
var ftsAvailable bool

// dataDir is the root for everything runtime: the database, bot workspaces and
// the shared projects/ link. Configurable so tests get their own.
func dataDir(config *Config) string {
	if config != nil && strings.TrimSpace(config.DataDir) != "" {
		return expandPath(config.DataDir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".ccc" // safe-ignore: no home means a relative fallback is the least-bad option
	}
	return filepath.Join(home, ".local", "share", "ccc")
}

func dbPath(config *Config) string { return filepath.Join(dataDir(config), "ccc.db") }

// botsDir holds one workspace per bot: <data_dir>/bots/<name>/workspace.
func botsDir(config *Config) string { return filepath.Join(dataDir(config), "bots") }

func botWorkspace(config *Config, name string) string {
	return filepath.Join(botsDir(config), name, "workspace")
}

// openStore opens (creating if needed) the SQLite database at path and applies
// the schema. WAL lets the `ccc mcp` child read and write while `ccc listen`
// holds the file; busy_timeout absorbs the short write contention that causes;
// foreign_keys is on because DESIGN §5 models real references.
func openStore(path string) (*gorm.DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
	}
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := db.AutoMigrate(allModels()...); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := ensureMemoryFTS(db); err != nil {
		ftsAvailable = false
		hookLog("FTS5 unavailable (%v); recall falls back to LIKE search", err)
	} else {
		ftsAvailable = true
	}
	return db, nil
}

// ensureMemoryFTS creates the FTS5 index over memories. AutoMigrate cannot
// express a virtual table, so it is raw SQL; the triggers keep it in sync with
// whatever writes the memories table (including the separate `ccc mcp`
// process, which is why this is done in SQL and not in Go callbacks).
func ensureMemoryFTS(db *gorm.DB) error {
	stmts := []string{
		`CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(key, text, content='memories', content_rowid='id')`,
		`CREATE TRIGGER IF NOT EXISTS memories_fts_ai AFTER INSERT ON memories BEGIN
			INSERT INTO memories_fts(rowid, key, text) VALUES (new.id, new.key, new.text);
		END`,
		`CREATE TRIGGER IF NOT EXISTS memories_fts_ad AFTER DELETE ON memories BEGIN
			INSERT INTO memories_fts(memories_fts, rowid, key, text) VALUES ('delete', old.id, old.key, old.text);
		END`,
		`CREATE TRIGGER IF NOT EXISTS memories_fts_au AFTER UPDATE ON memories BEGIN
			INSERT INTO memories_fts(memories_fts, rowid, key, text) VALUES ('delete', old.id, old.key, old.text);
			INSERT INTO memories_fts(rowid, key, text) VALUES (new.id, new.key, new.text);
		END`,
	}
	for _, s := range stmts {
		if err := db.Exec(s).Error; err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Bots
// ---------------------------------------------------------------------------

// botByTopic finds the live bot behind a forum topic.
func botByTopic(db *gorm.DB, topicID int64) (*Bot, error) {
	var b Bot
	err := db.Where("topic_id = ? AND archived_at IS NULL", topicID).First(&b).Error
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func botByID(db *gorm.DB, id int64) (*Bot, error) {
	var b Bot
	if err := db.First(&b, id).Error; err != nil {
		return nil, err
	}
	return &b, nil
}

func botByName(db *gorm.DB, name string) (*Bot, error) {
	var b Bot
	if err := db.Where("name = ? AND archived_at IS NULL", name).First(&b).Error; err != nil {
		return nil, err
	}
	return &b, nil
}

func liveBots(db *gorm.DB) ([]Bot, error) {
	var bots []Bot
	err := db.Where("archived_at IS NULL").Order("id").Find(&bots).Error
	return bots, err
}

// ---------------------------------------------------------------------------
// Forum topic icons
// ---------------------------------------------------------------------------

// Topic icons are the emoji Telegram shows beside a topic title. They are not
// free-form: editForumTopic only accepts a custom-emoji id out of
// getForumTopicIconStickers. The list is identical for every bot and changes
// rarely, so ccc caches it in memory and in the settings table and refreshes it
// once a day — a stale list is much better than a rename that fails.
const (
	settingTopicIcons = "topic_icons"
	topicIconsTTL     = 24 * time.Hour
)

type topicIconCache struct {
	FetchedAt time.Time          `json:"fetched_at"`
	Stickers  []TopicIconSticker `json:"stickers"`
}

var topicIconMem struct {
	mu    sync.Mutex
	cache topicIconCache
}

// resetTopicIconCache drops the in-memory copy. Only the tests need it: the
// cache is process-wide and they each run against their own fake Bot API.
func resetTopicIconCache() {
	topicIconMem.mu.Lock()
	topicIconMem.cache = topicIconCache{}
	topicIconMem.mu.Unlock()
}

// topicIcons returns the emoji usable as topic icons, hitting Telegram at most
// once a day. It never fails: when the fetch fails the last known list is
// returned (possibly empty, which simply means "leave icons alone").
func topicIcons(db *gorm.DB, config *Config) []TopicIconSticker {
	topicIconMem.mu.Lock()
	defer topicIconMem.mu.Unlock()
	now := time.Now()
	if len(topicIconMem.cache.Stickers) > 0 && now.Sub(topicIconMem.cache.FetchedAt) < topicIconsTTL {
		return topicIconMem.cache.Stickers
	}
	if db != nil {
		var stored topicIconCache
		if raw := getSetting(db, settingTopicIcons, ""); raw != "" {
			if err := json.Unmarshal([]byte(raw), &stored); err == nil {
				topicIconMem.cache = stored
				if len(stored.Stickers) > 0 && now.Sub(stored.FetchedAt) < topicIconsTTL {
					return stored.Stickers
				}
			}
		}
	}
	if config == nil || config.BotToken == "" {
		return topicIconMem.cache.Stickers
	}
	stickers, err := fetchForumTopicIcons(config)
	if err != nil || len(stickers) == 0 {
		hookLog("getForumTopicIconStickers: %v", err)
		return topicIconMem.cache.Stickers
	}
	topicIconMem.cache = topicIconCache{FetchedAt: now, Stickers: stickers}
	if db != nil {
		if raw, err := json.Marshal(topicIconMem.cache); err == nil {
			if err := setSetting(db, settingTopicIcons, string(raw)); err != nil {
				hookLog("cache topic icons: %v", err)
			}
		}
	}
	return stickers
}

// topicIconEmoji lists the emoji that may be used as icons, for the model and
// for error messages.
func topicIconEmoji(stickers []TopicIconSticker) []string {
	out := make([]string, 0, len(stickers))
	for _, s := range stickers {
		if s.Emoji != "" {
			out = append(out, s.Emoji)
		}
	}
	return out
}

// matchTopicIcon picks the icon id for an emoji: an exact match first, then the
// same emoji ignoring variation selectors and skin tones. Anything else returns
// false, and the caller leaves the icon exactly as it was.
func matchTopicIcon(stickers []TopicIconSticker, emoji string) (string, bool) {
	want := strings.TrimSpace(emoji)
	if want == "" {
		return "", false
	}
	for _, s := range stickers {
		if s.Emoji == want {
			return s.CustomEmojiID, true
		}
	}
	base := emojiBase(want)
	if base == "" {
		return "", false
	}
	for _, s := range stickers {
		if emojiBase(s.Emoji) == base {
			return s.CustomEmojiID, true
		}
	}
	return "", false
}

// emojiBase strips the runes that only decorate an emoji — variation selectors
// and skin-tone modifiers — so 👍 and 👍🏽 compare equal.
func emojiBase(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == 0xFE0E || r == 0xFE0F: // variation selectors
		case r >= 0x1F3FB && r <= 0x1F3FF: // skin tones
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// resolveTopicIcon turns a requested emoji into an icon id, plus a note to show
// when it is not one Telegram allows. An empty emoji is not a failure: it means
// "keep the current icon".
func resolveTopicIcon(db *gorm.DB, config *Config, emoji string) (iconID, note string) {
	emoji = strings.TrimSpace(emoji)
	if emoji == "" {
		return "", ""
	}
	stickers := topicIcons(db, config)
	if id, ok := matchTopicIcon(stickers, emoji); ok {
		return id, ""
	}
	available := topicIconEmoji(stickers)
	if len(available) == 0 {
		return "", fmt.Sprintf("the icon was left unchanged: Telegram did not return its list of topic icons, so %s could not be checked", emoji)
	}
	return "", fmt.Sprintf("%s is not one of Telegram's topic icons, so the icon is unchanged. Available: %s",
		emoji, strings.Join(available, " "))
}

// maxBotNameLen caps a bot name, in characters. A name is a topic title, the
// address send_to_bot/list_bots use, and part of the system prompt, so it stays
// short enough to read in a topic list.
const maxBotNameLen = 64

// validateBotName checks a name a human typed (/name) or set on the topic
// itself. It returns the trimmed name or an error whose text is meant to be
// shown in Telegram. selfID is excluded from the uniqueness check (0 = none).
//
// Uniqueness is case-insensitive among live bots, because the name is how bots
// address each other; archived bots are reported separately because the column
// is UNIQUE over every row, so their name is taken even though they are gone.
func validateBotName(db *gorm.DB, selfID int64, raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", errors.New("a name cannot be empty")
	}
	if n := utf8.RuneCountInString(name); n > maxBotNameLen {
		return "", fmt.Errorf("that name is %d characters long; the limit is %d", n, maxBotNameLen)
	}
	if strings.ContainsAny(name, `/\`) {
		return "", errors.New("a name cannot contain / or \\")
	}
	for _, r := range name {
		if r < 32 || r == 127 {
			return "", errors.New("a name cannot contain control characters")
		}
	}
	var clash Bot
	err := db.Where("id <> ? AND LOWER(name) = LOWER(?)", selfID, name).First(&clash).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return name, nil
	case err != nil:
		return "", err
	case clash.ArchivedAt != nil:
		return "", fmt.Errorf("an archived bot still holds the name %q", clash.Name)
	default:
		return "", fmt.Errorf("another bot is already called %q", clash.Name)
	}
}

// renameBot applies an already validated name. It also clears session_id: the
// name is part of the system prompt, which Claude Code records per
// conversation, so the new name only reaches the model in a new one — the same
// reason /role rotates (DESIGN §14.14). Memories are untouched.
func renameBot(db *gorm.DB, config *Config, b *Bot, name string) error {
	updates := map[string]any{"name": name, "session_id": ""}
	if strings.TrimSpace(b.Cwd) == "" {
		// An empty cwd resolves to <data_dir>/bots/<name>/workspace, so pin the
		// current directory before the name moves out from under it.
		updates["cwd"] = botWorkspace(config, b.Name)
	}
	if err := db.Model(&Bot{}).Where("id = ?", b.ID).Updates(updates).Error; err != nil {
		return err
	}
	b.Name = name
	b.SessionID = ""
	if cwd, ok := updates["cwd"].(string); ok {
		b.Cwd = cwd
	}
	return nil
}

// uniqueBotName makes a name unique among live bots by suffixing -2, -3, …
func uniqueBotName(db *gorm.DB, base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "bot"
	}
	name := base
	for i := 2; i < 1000; i++ {
		var n int64
		db.Model(&Bot{}).Where("name = ?", name).Count(&n)
		if n == 0 {
			return name
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return fmt.Sprintf("%s-%d", base, time.Now().Unix())
}

func setBotStatus(db *gorm.DB, botID int64, status string) {
	db.Model(&Bot{}).Where("id = ?", botID).Update("status", status)
}

// ---------------------------------------------------------------------------
// Memories
// ---------------------------------------------------------------------------

// memoryScopeKey normalizes the (scope, scope_key) pair. bot scope is always
// the calling bot; project scope needs an explicit path.
func memoryScopeKey(scope string, botID int64, projectPath string) (string, string, error) {
	switch scope {
	case scopeUser:
		return scopeUser, "", nil
	case scopeBot:
		return scopeBot, fmt.Sprintf("%d", botID), nil
	case scopeProject:
		p := strings.TrimSpace(projectPath)
		if p == "" {
			return "", "", errors.New("project scope needs project_path")
		}
		return scopeProject, expandPath(p), nil
	default:
		return "", "", fmt.Errorf("unknown scope %q (use user, project or bot)", scope)
	}
}

// upsertMemory writes one memory, replacing any existing text under the same
// (scope, scope_key, key).
func upsertMemory(db *gorm.DB, scope, scopeKey, key, text string, botID int64) error {
	m := Memory{Scope: scope, ScopeKey: scopeKey, Key: key, Text: text, CreatedByBotID: &botID}
	return db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "scope"}, {Name: "scope_key"}, {Name: "key"}},
		DoUpdates: clause.Assignments(map[string]any{"text": text, "updated_at": time.Now()}),
	}).Create(&m).Error
}

// visibleMemories returns the memories a bot may see: every user memory, every
// project memory, and its own bot memories (DESIGN §6 recall).
func visibleMemories(db *gorm.DB, botID int64) *gorm.DB {
	return db.Model(&Memory{}).Where(
		"scope = ? OR scope = ? OR (scope = ? AND scope_key = ?)",
		scopeUser, scopeProject, scopeBot, fmt.Sprintf("%d", botID))
}

// searchMemories runs the recall query. FTS5 when available, LIKE otherwise;
// an empty query lists the most recent visible memories.
func searchMemories(db *gorm.DB, botID int64, query string, scope string, limit int) ([]Memory, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	q := visibleMemories(db, botID)
	if scope != "" {
		q = q.Where("scope = ?", scope)
	}
	query = strings.TrimSpace(query)
	if query == "" {
		var out []Memory
		err := q.Order("updated_at DESC").Limit(limit).Find(&out).Error
		return out, err
	}
	if ftsAvailable {
		var out []Memory
		err := q.Where("id IN (SELECT rowid FROM memories_fts WHERE memories_fts MATCH ?)", ftsQuery(query)).
			Order("updated_at DESC").Limit(limit).Find(&out).Error
		if err == nil {
			return out, nil
		}
		// A malformed MATCH expression is a user-input problem, not a broken
		// index: fall through to LIKE rather than failing the tool call.
		hookLog("FTS query failed (%v); falling back to LIKE", err)
		q = visibleMemories(db, botID)
		if scope != "" {
			q = q.Where("scope = ?", scope)
		}
	}
	like := "%" + query + "%"
	var out []Memory
	err := q.Where("key LIKE ? OR text LIKE ?", like, like).Order("updated_at DESC").Limit(limit).Find(&out).Error
	return out, err
}

// ftsQuery turns free text into a safe FTS5 MATCH expression: every word is
// quoted, so user text can never inject FTS operators.
func ftsQuery(s string) string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return !(r == '_' || r == '-' || r == '.' || r == '/' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	})
	if len(fields) == 0 {
		return `""`
	}
	quoted := make([]string, 0, len(fields))
	for _, f := range fields {
		quoted = append(quoted, `"`+f+`"`)
	}
	return strings.Join(quoted, " OR ")
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

// The settings table (DESIGN §5) holds ccc's own bookkeeping only. What the
// owner tunes lives in config.json (`ccc config set debounce_ms 0`); there is
// deliberately no Telegram command for it.
const (
	// settingLastMaintenance is the date (YYYY-MM-DD) maintenance last ran, so
	// a restart does not re-run it and a missed day is caught up on.
	settingLastMaintenance = "last_maintenance"
)

func getSetting(db *gorm.DB, key, def string) string {
	var s Setting
	if err := db.First(&s, "key = ?", key).Error; err != nil {
		return def
	}
	if s.Value == "" {
		return def
	}
	return s.Value
}

// getSettingInt reads a numeric setting. An unparseable or negative value is
// not an error worth failing a turn over: the default is used instead.
func getSettingInt(db *gorm.DB, key string, def int) int {
	raw := strings.TrimSpace(getSetting(db, key, ""))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		hookLog("setting %s = %q is not a number; using %d", key, raw, def)
		return def
	}
	return n
}

func setSetting(db *gorm.DB, key, value string) error {
	return db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.Assignments(map[string]any{"value": value}),
	}).Create(&Setting{Key: key, Value: value}).Error
}

// ---------------------------------------------------------------------------
// Profile load
// ---------------------------------------------------------------------------

// runningTurnsByProfile counts the turns currently executing on each account.
// This replaces the v2 "working background agents" input to chooseProfile
// (DESIGN §4): v3 has no fleet to ask, and its own turn table is both cheaper
// and exactly right — one running turn is one `claude -p` process.
func runningTurnsByProfile(config *Config) map[string]int {
	db, err := openStore(dbPath(config))
	if err != nil {
		return nil
	}
	defer closeStore(db)
	return runningTurnsByProfileDB(db)
}

func runningTurnsByProfileDB(db *gorm.DB) map[string]int {
	var rows []struct {
		Profile string
		N       int
	}
	if err := db.Model(&Turn{}).Select("profile, count(*) as n").
		Where("status = ? AND profile <> ''", turnRunning).Group("profile").Scan(&rows).Error; err != nil {
		return nil
	}
	out := make(map[string]int, len(rows))
	for _, r := range rows {
		out[r.Profile] = r.N
	}
	return out
}

// profileHasLiveTurns names the bots whose turn is running on a profile right
// now, so `ccc profile remove` / `/account remove` refuse to pull it away.
func profileHasLiveTurns(config *Config, profile string) ([]string, error) {
	db, err := openStore(dbPath(config))
	if err != nil {
		return nil, nil // safe-ignore: no database means no instance has ever run, so nothing is live
	}
	defer closeStore(db)
	var names []string
	err = db.Model(&Turn{}).Joins("JOIN bots ON bots.id = turns.bot_id").
		Where("turns.status = ? AND turns.profile = ?", turnRunning, profile).
		Distinct().Pluck("bots.name", &names).Error
	return names, err
}

// closeStore releases the underlying sqlite handle. The long-lived listener
// never calls it; the short CLI paths above must, or they leak a file handle
// (and a WAL reader) per invocation.
func closeStore(db *gorm.DB) {
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.Close() // safe-ignore: the process is about to move on either way
	}
}

// botByCwd finds the live bot whose working directory contains path. It is how
// `ccc send <file>`, run from inside a bot's workspace, knows which topic to
// post into.
func botByCwd(db *gorm.DB, path string) (*Bot, error) {
	bots, err := liveBots(db)
	if err != nil {
		return nil, err
	}
	best := -1
	for i := range bots {
		cwd := strings.TrimRight(bots[i].Cwd, string(filepath.Separator))
		if cwd == "" {
			continue
		}
		if path != cwd && !strings.HasPrefix(path, cwd+string(filepath.Separator)) {
			continue
		}
		// Prefer the most specific match when workspaces nest.
		if best < 0 || len(cwd) > len(bots[best].Cwd) {
			best = i
		}
	}
	if best < 0 {
		return nil, errors.New("no bot owns this directory")
	}
	return &bots[best], nil
}

// ---------------------------------------------------------------------------
// Bot lifecycle
// ---------------------------------------------------------------------------

// createBotRow creates a bot end to end: a unique name, a forum topic, a
// workspace and the database row. Only the owner creates bots (plain text in
// General, or /bot); there is no MCP tool for it.
// A cwd of "" means the bot gets its own workspace under <data_dir>/bots.
func createBotRow(db *gorm.DB, config *Config, name, role, cwd string) (*Bot, error) {
	name = uniqueBotName(db, sanitizeBotName(name))
	topicID, err := createForumTopic(config, name)
	if err != nil {
		return nil, err
	}
	if cwd == "" {
		cwd = botWorkspace(config, name)
	}
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		return nil, err
	}
	engine := defaultEngine(config)
	b := &Bot{Name: name, TopicID: topicID, Role: role, Cwd: cwd, Engine: engine, Status: botIdle}
	if err := db.Create(b).Error; err != nil {
		return nil, err
	}
	return b, nil
}

// archiveBotRow retires a bot: the row is marked archived, its queue is
// dropped and its automation stops. Memories are deliberately kept — a bot's
// notes outliving it is the point of them being in the database.
func archiveBotRow(db *gorm.DB, botID int64) error {
	now := time.Now()
	if err := db.Model(&Bot{}).Where("id = ?", botID).
		Updates(map[string]any{"archived_at": now, "status": botDisabled}).Error; err != nil {
		return err
	}
	db.Model(&Turn{}).Where("bot_id = ? AND status = ?", botID, turnQueued).
		Updates(map[string]any{"status": turnFailed, "stop_reason": "bot archived"})
	db.Model(&Watch{}).Where("bot_id = ?", botID).Update("enabled", false)
	db.Model(&Schedule{}).Where("bot_id = ? AND fired_at IS NULL", botID).Update("fired_at", now)
	var running []BackgroundJob
	db.Where("bot_id = ? AND status IN ?", botID, []string{jobQueued, jobRunning}).Find(&running)
	for i := range running {
		if running[i].PID > 0 {
			killProcessGroup(running[i].PID)
		}
	}
	db.Model(&BackgroundJob{}).Where("bot_id = ? AND status IN ?", botID, []string{jobQueued, jobRunning}).
		Updates(map[string]any{"status": jobFailed, "error": "bot archived", "ended_at": now, "cancel_requested": true})
	return nil
}
