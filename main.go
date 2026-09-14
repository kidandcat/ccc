package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const version = "3.0.0"

// Config is the bootstrap configuration (<config_dir>/config.json). Everything
// that changes at runtime lives in SQLite instead (DESIGN §5); this file only
// holds what ccc needs before the database exists.
type Config struct {
	BotToken          string              `json:"bot_token"`
	ChatID            int64               `json:"chat_id"`                      // the owner's Telegram user id — also their DM chat
	GroupID           int64               `json:"group_id,omitempty"`           // the forum group the bots live in
	TranscriptionLang string              `json:"transcription_lang,omitempty"` // language code for whisper (e.g. "es")
	RelayURL          string              `json:"relay_url,omitempty"`          // relay server for files over 50 MB
	Profiles          map[string]*Profile `json:"profiles,omitempty"`           // identity -> account (engine + isolated home)
	DefaultProfile    string              `json:"default_profile,omitempty"`    // default account; new bots inherit its engine unless default_engine is set
	DataDir           string              `json:"data_dir,omitempty"`           // runtime root (default ~/.local/share/ccc)
	Model             string              `json:"model,omitempty"`              // model every bot runs on (default: claude's own)
	DefaultEngine     string              `json:"default_engine,omitempty"`     // engine assigned to new bots (default: claude)
	EnvPassthrough    []string            `json:"env_passthrough,omitempty"`    // extra env var names bots inherit (DESIGN §3.1)
	// Tuning knobs. They are pointers where 0 is a meaningful value, so an
	// absent key means "use the default" rather than "set it to zero".
	DebounceMS      *int   `json:"debounce_ms,omitempty"`      // ms an idle bot waits for more messages (default 2500; 0 disables)
	CompactionModel string `json:"compaction_model,omitempty"` // model the memory compaction turn runs on (default haiku)
	MaintenanceHour *int   `json:"maintenance_hour,omitempty"` // local hour the daily maintenance job runs at (default 4)
}

// TelegramMessage represents a Telegram message.
type TelegramMessage struct {
	MessageID       int   `json:"message_id"`
	MessageThreadID int64 `json:"message_thread_id,omitempty"` // topic id
	Chat            struct {
		ID   int64  `json:"id"`
		Type string `json:"type"` // "private", "group", "supergroup"
	} `json:"chat"`
	From struct {
		ID        int64  `json:"id"`
		Username  string `json:"username"`
		FirstName string `json:"first_name"`
	} `json:"from"`
	Text string `json:"text"`
	// EditDate is set on an edited_message: the unix time of THAT edit. It is
	// what makes an edited command dispatchable exactly once (listenv3.go).
	EditDate       int64             `json:"edit_date,omitempty"`
	ReplyToMessage *TelegramMessage  `json:"reply_to_message,omitempty"`
	Voice          *TelegramVoice    `json:"voice,omitempty"`
	Photo          []TelegramPhoto   `json:"photo,omitempty"`
	Document       *TelegramDocument `json:"document,omitempty"`
	Caption        string            `json:"caption,omitempty"`
	// ForumTopicEdited is the service message Telegram posts into a topic when
	// somebody renames it in the app. ccc follows the title with the bot's name.
	ForumTopicEdited *ForumTopicEdited `json:"forum_topic_edited,omitempty"`
}

// ForumTopicEdited carries the new title of a renamed forum topic. Only the
// changed fields are present, so an icon-only edit has an empty Name.
type ForumTopicEdited struct {
	Name string `json:"name,omitempty"`
}

type TelegramVoice struct {
	FileID   string `json:"file_id"`
	Duration int    `json:"duration"`
}

type TelegramPhoto struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int    `json:"file_size"`
}

type TelegramDocument struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	FileSize int    `json:"file_size"`
}

// CallbackQuery represents an inline-button tap.
type CallbackQuery struct {
	ID   string `json:"id"`
	From struct {
		ID        int64  `json:"id"`
		Username  string `json:"username"`
		FirstName string `json:"first_name"`
	} `json:"from"`
	Message *TelegramMessage `json:"message"`
	Data    string           `json:"data"`
}

// telegramUpdateEntry is one element of a getUpdates result.
type telegramUpdateEntry struct {
	UpdateID      int              `json:"update_id"`
	Message       TelegramMessage  `json:"message"`
	EditedMessage *TelegramMessage `json:"edited_message"`
	CallbackQuery *CallbackQuery   `json:"callback_query"`
}

// TelegramUpdate represents a getUpdates response.
type TelegramUpdate struct {
	OK          bool                  `json:"ok"`
	Description string                `json:"description"`
	Result      []telegramUpdateEntry `json:"result"`
}

// TelegramResponse represents a generic Bot API response.
type TelegramResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
}

// TopicResult is the result of creating a forum topic.
type TopicResult struct {
	MessageThreadID int64  `json:"message_thread_id"`
	Name            string `json:"name"`
}

// InlineKeyboardButton represents a Telegram inline keyboard button.
type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

func init() {
	initProfiles()
	initPaths()
}

func main() {
	if len(os.Args) < 2 {
		printHelp()
		return
	}

	switch os.Args[1] {
	case "-h", "--help", "help":
		printHelp()

	case "-v", "--version", "version":
		fmt.Printf("ccc version %s\n", version)

	case "setup":
		if len(os.Args) < 3 {
			fail("Usage: ccc setup <bot_token>")
		}
		must(setup(os.Args[2]))

	case "doctor":
		doctor(hasFlag(os.Args[2:], "--fix"))

	case "maintain":
		// The growth-control job of DESIGN §7, on demand. It is the same pass
		// the listener runs nightly, against the same database.
		must(runMaintainCommand())

	case "profile":
		must(profileCommand(os.Args[2:]))

	case "config":
		must(configCommand(os.Args[2:]))

	case "setgroup":
		config, err := loadConfig()
		must(err)
		must(setGroup(config))

	case "mcp":
		// Stdio MCP server for one turn; spawned by Claude Code, never by hand.
		must(runMCPServer(os.Args[2:]))

	case "listen":
		must(listenV3())

	case "install":
		must(installService())

	case "env":
		// `ccc env sync` snapshots the env_passthrough secrets for the service.
		must(runEnvSyncCommand(os.Args[2:]))

	case "send":
		if len(os.Args) < 3 {
			fail("Usage: ccc send <file>")
		}
		must(handleSendFile(os.Args[2]))

	case "relay":
		port := "8080"
		if len(os.Args) >= 3 {
			port = os.Args[2]
		}
		runRelayServer(port)

	default:
		fail("Unknown command %q. Run `ccc --help`.", os.Args[1])
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// ---------------------------------------------------------------------------
// `ccc config`
// ---------------------------------------------------------------------------

// configCommand is the non-interactive bootstrap path: on a headless VM,
// `ccc config set bot_token …`, `chat_id …` and `group_id …` are enough to
// bring an instance up without ever attaching a terminal to Telegram.
func configCommand(args []string) error {
	config, err := loadConfig()
	if err != nil || config == nil {
		config = &Config{} // not configured yet: `config set` is how it starts existing
	}
	if len(args) == 0 {
		printConfig(config)
		return nil
	}
	switch args[0] {
	case "get":
		if len(args) < 2 {
			return fmt.Errorf("usage: ccc config get <key>")
		}
		value, err := configGet(config, args[1])
		if err != nil {
			return err
		}
		fmt.Println(value)
		return nil
	case "set":
		if len(args) < 3 {
			return fmt.Errorf("usage: ccc config set <key> <value>")
		}
		if err := configSet(config, args[1], strings.Join(args[2:], " ")); err != nil {
			return err
		}
		if err := saveConfig(config); err != nil {
			return fmt.Errorf("save config: %w", err)
		}
		value, err := configGet(config, args[1])
		if err != nil {
			return err
		}
		fmt.Printf("✅ %s = %s\n", args[1], value)
		if args[1] == "env_passthrough" {
			// The names just changed, so the file the service reads is stale.
			// Re-syncing here is also the moment the owner finds out that a
			// name they listed is not actually exported in this shell.
			res, err := syncEnvFile(config)
			if err != nil {
				return fmt.Errorf("write the env file: %w", err)
			}
			fmt.Print(res.String())
		}
		return nil
	default:
		return fmt.Errorf("usage: ccc config [get <key> | set <key> <value>]")
	}
}

// parseRange reads a bounded integer setting, so a typo cannot park every bot
// or push maintenance to an hour that never arrives.
func parseRange(key, value string, lo, hi int) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number, got %q", key, value)
	}
	if n < lo || n > hi {
		return 0, fmt.Errorf("%s must be between %d and %d, got %d", key, lo, hi, n)
	}
	return n, nil
}

// withDefaultNote marks a value the owner has never set, so `ccc config` shows
// what ccc will actually do instead of an empty line.
func withDefaultNote(value string, isDefault bool) string {
	if isDefault {
		return value + " (default)"
	}
	return value
}

// configKeys are the keys `ccc config` understands. Secrets are never printed
// back (DESIGN §12): the bot token reads as "configured".
var configKeys = []string{"bot_token", "chat_id", "group_id", "model", "data_dir", "env_passthrough", "relay_url",
	"transcription_lang", "default_profile", "default_engine", "debounce_ms", "compaction_model", "maintenance_hour"}

func configGet(config *Config, key string) (string, error) {
	switch key {
	case "bot_token":
		if config.BotToken == "" {
			return "not set", nil
		}
		return "configured", nil
	case "chat_id":
		return fmt.Sprint(config.ChatID), nil
	case "group_id":
		return fmt.Sprint(config.GroupID), nil
	case "model":
		return firstNonEmpty(config.Model, "(claude default)"), nil
	case "data_dir":
		return dataDir(config), nil
	case "env_passthrough":
		return strings.Join(config.EnvPassthrough, ","), nil
	case "relay_url":
		return firstNonEmpty(config.RelayURL, defaultRelayURL), nil
	case "transcription_lang":
		return firstNonEmpty(config.TranscriptionLang, "(auto-detect)"), nil
	case "default_profile":
		return firstNonEmpty(config.DefaultProfile, "(first by name)"), nil
	case "default_engine":
		return withDefaultNote(defaultEngine(config), strings.TrimSpace(config.DefaultEngine) == ""), nil
	case "debounce_ms":
		return withDefaultNote(fmt.Sprint(debounceMS(config)), config.DebounceMS == nil), nil
	case "compaction_model":
		return withDefaultNote(compactionModel(config), strings.TrimSpace(config.CompactionModel) == ""), nil
	case "maintenance_hour":
		return withDefaultNote(fmt.Sprint(maintenanceHour(config)), config.MaintenanceHour == nil), nil
	}
	return "", fmt.Errorf("unknown config key %q (known: %s)", key, strings.Join(configKeys, ", "))
}

func configSet(config *Config, key, value string) error {
	value = strings.TrimSpace(value)
	parseID := func() (int64, error) {
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s must be a number, got %q", key, value)
		}
		return n, nil
	}
	switch key {
	case "bot_token":
		config.BotToken = value
	case "chat_id":
		n, err := parseID()
		if err != nil {
			return err
		}
		config.ChatID = n
	case "group_id":
		n, err := parseID()
		if err != nil {
			return err
		}
		config.GroupID = n
	case "model":
		config.Model = value
	case "data_dir":
		config.DataDir = value
	case "env_passthrough":
		config.EnvPassthrough = splitList(value)
	case "relay_url":
		config.RelayURL = value
	case "transcription_lang":
		config.TranscriptionLang = value
	case "default_profile":
		config.DefaultProfile = value
	case "default_engine":
		e, err := parseEngine(value)
		if err != nil {
			return err
		}
		if e == engineClaude {
			config.DefaultEngine = ""
		} else {
			config.DefaultEngine = e
		}
	case "debounce_ms":
		n, err := parseRange(key, value, 0, maxDebounceMS)
		if err != nil {
			return err
		}
		config.DebounceMS = &n
	case "compaction_model":
		config.CompactionModel = value
	case "maintenance_hour":
		n, err := parseRange(key, value, 0, 23)
		if err != nil {
			return err
		}
		config.MaintenanceHour = &n
	default:
		return fmt.Errorf("unknown config key %q (known: %s)", key, strings.Join(configKeys, ", "))
	}
	return nil
}

// splitList parses a comma- or space-separated list, dropping empties.
func splitList(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func printConfig(config *Config) {
	fmt.Printf("config file: %s\n\n", getConfigPath())
	for _, key := range configKeys {
		value, err := configGet(config, key)
		if err != nil {
			continue
		}
		fmt.Printf("%-19s %s\n", key+":", value)
	}
	fmt.Println("\nUsage: ccc config set <key> <value>")
}
