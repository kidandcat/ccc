package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tests for the bootstrap config file and the wire types. Everything the v2
// session map, ledger and hooks used to cover went away with them (DESIGN §11).

func TestConfigSaveLoad(t *testing.T) {
	isolateConfigEnv(t)
	t.Setenv("HOME", t.TempDir())

	config := &Config{
		BotToken:       "test-token-123",
		ChatID:         12345,
		Model:          "sonnet",
		DataDir:        "/var/lib/ccc",
		EnvPassthrough: []string{"GH_TOKEN"},
	}
	if err := saveConfig(config); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}

	loaded, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if loaded.BotToken != config.BotToken || loaded.ChatID != config.ChatID ||
		loaded.Model != config.Model || loaded.DataDir != config.DataDir {
		t.Errorf("round trip lost data: %+v", loaded)
	}
	if len(loaded.EnvPassthrough) != 1 || loaded.EnvPassthrough[0] != "GH_TOKEN" {
		t.Errorf("EnvPassthrough = %v", loaded.EnvPassthrough)
	}
}

func TestGetConfigPathHonorsCCCConfig(t *testing.T) {
	isolateConfigEnv(t)
	t.Setenv("HOME", t.TempDir())
	custom := filepath.Join(t.TempDir(), "custom.json")
	t.Setenv("CCC_CONFIG", custom)
	if got := getConfigPath(); got != custom {
		t.Errorf("getConfigPath() = %q, want CCC_CONFIG %q", got, custom)
	}
}

// A config written by ccc v2 still carries `sessions` (and other dead keys).
// v3 must load it and ignore them rather than refusing to start.
func TestConfigLoadToleratesLegacyKeys(t *testing.T) {
	isolateConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "ccc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{
		"bot_token": "T",
		"chat_id": 42,
		"group_id": -100,
		"away": true,
		"projects_dir": "~/Projects",
		"oauth_token": "dead",
		"sessions": {"ccc": {"topic_id": 7, "path": "/home/u/ccc", "session_id": "abc"}}
	}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	config, err := loadConfig()
	if err != nil {
		t.Fatalf("a v2 config must still load: %v", err)
	}
	if config.BotToken != "T" || config.ChatID != 42 {
		t.Errorf("the keys v3 still uses were lost: %+v", config)
	}

	// Saving drops the dead keys for good.
	if err := saveConfig(config); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "sessions") {
		t.Errorf("saveConfig kept the legacy sessions map: %s", data)
	}
	if strings.Contains(string(data), "group_id") {
		t.Errorf("saveConfig kept leftover group_id: %s", data)
	}
}

func TestConfigLoadNonExistent(t *testing.T) {
	isolateConfigEnv(t)
	t.Setenv("HOME", t.TempDir())
	if _, err := loadConfig(); err == nil {
		t.Error("loadConfig should fail when there is no config file")
	}
}

func TestConfigFilePermissions(t *testing.T) {
	isolateConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := saveConfig(&Config{BotToken: "secret-token", ChatID: 12345}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	info, err := os.Stat(filepath.Join(home, ".config", "ccc", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("config permissions = %o, want 0600 (it holds the bot token)", perm)
	}
}

func TestConfigCommandSetAndGet(t *testing.T) {
	isolateConfigEnv(t)
	t.Setenv("HOME", t.TempDir())

	if err := configCommand([]string{"set", "chat_id", "777"}); err != nil {
		t.Fatalf("config set chat_id: %v", err)
	}
	if err := configCommand([]string{"set", "bot_token", "123:abc"}); err != nil {
		t.Fatalf("config set bot_token: %v", err)
	}
	if err := configCommand([]string{"set", "env_passthrough", "GH_TOKEN, LINEAR_API_KEY"}); err != nil {
		t.Fatalf("config set env_passthrough: %v", err)
	}
	if err := configCommand([]string{"set", "allowed_user_ids", "4242, 99"}); err != nil {
		t.Fatalf("config set allowed_user_ids: %v", err)
	}

	config, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.ChatID != 777 || config.BotToken != "123:abc" {
		t.Errorf("values not persisted: %+v", config)
	}
	if len(config.EnvPassthrough) != 2 || config.EnvPassthrough[1] != "LINEAR_API_KEY" {
		t.Errorf("env_passthrough = %v", config.EnvPassthrough)
	}
	if len(config.AllowedUserIDs) != 2 || config.AllowedUserIDs[0] != 4242 || config.AllowedUserIDs[1] != 99 {
		t.Errorf("allowed_user_ids = %v", config.AllowedUserIDs)
	}

	// The token is never echoed back.
	got, err := configGet(config, "bot_token")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "123:abc") {
		t.Errorf("config get bot_token leaked the token: %q", got)
	}

	if err := configCommand([]string{"set", "chat_id", "not-a-number"}); err == nil {
		t.Error("a non-numeric chat_id should be rejected")
	}
	if err := configCommand([]string{"set", "allowed_user_ids", "nope"}); err == nil {
		t.Error("a non-numeric allowed_user_ids should be rejected")
	}
	if err := configCommand([]string{"set", "nonsense", "x"}); err == nil {
		t.Error("an unknown key should be rejected")
	}
}

// The tuning knobs that used to be edited from Telegram with /set are plain
// config.json keys: `ccc config` reports the default in force until one is set,
// and out-of-range values are refused rather than parking every bot.
func TestConfigCommandTuningKnobs(t *testing.T) {
	isolateConfigEnv(t)
	t.Setenv("HOME", t.TempDir())

	fresh := &Config{}
	for key, want := range map[string]string{
		"debounce_ms":      "2500 (default)",
		"compaction_model": "haiku (default)",
		"maintenance_hour": "4 (default)",
		"idle_compact_s":   "3600 (default)",
		"watch_ttl_s":      "14400 (default)",
	} {
		got, err := configGet(fresh, key)
		if err != nil {
			t.Fatalf("config get %s: %v", key, err)
		}
		if got != want {
			t.Errorf("config get %s = %q, want %q", key, got, want)
		}
	}

	for _, kv := range [][2]string{{"debounce_ms", "0"}, {"compaction_model", "sonnet"}, {"maintenance_hour", "22"}, {"idle_compact_s", "0"}, {"watch_ttl_s", "7200"}} {
		if err := configCommand([]string{"set", kv[0], kv[1]}); err != nil {
			t.Fatalf("config set %s %s: %v", kv[0], kv[1], err)
		}
	}
	config, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	// 0 must survive as "the owner turned debouncing off", not as "unset".
	if debounceMS(config) != 0 {
		t.Errorf("debounce_ms = %d, want 0", debounceMS(config))
	}
	if compactionModel(config) != "sonnet" {
		t.Errorf("compaction_model = %q, want sonnet", compactionModel(config))
	}
	if maintenanceHour(config) != 22 {
		t.Errorf("maintenance_hour = %d, want 22", maintenanceHour(config))
	}
	if idleCompact(config) != 0 {
		t.Errorf("idle_compact_s = %s, want 0 (disabled)", idleCompact(config))
	}
	if watchTTL(config) != 2*time.Hour {
		t.Errorf("watch_ttl_s = %s, want 2h", watchTTL(config))
	}

	for _, kv := range [][2]string{{"debounce_ms", "-1"}, {"debounce_ms", "600000"}, {"maintenance_hour", "25"}, {"maintenance_hour", "x"}, {"idle_compact_s", "-1"}, {"watch_ttl_s", "x"}} {
		if err := configCommand([]string{"set", kv[0], kv[1]}); err == nil {
			t.Errorf("config set %s %s should have been rejected", kv[0], kv[1])
		}
	}
}

func TestSplitList(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want int
	}{{"", 0}, {"A", 1}, {"A,B", 2}, {"A, B  C", 3}, {" , , ", 0}} {
		if got := splitList(tt.in); len(got) != tt.want {
			t.Errorf("splitList(%q) = %v, want %d entries", tt.in, got, tt.want)
		}
	}
}

func TestTelegramMessageJSON(t *testing.T) {
	raw := `{
		"message_id": 100,
		"message_thread_id": 55,
		"text": "Reply text",
		"chat": {"id": 123, "type": "supergroup"},
		"from": {"id": 456, "username": "user", "first_name": "Jairo"},
		"reply_to_message": {"message_id": 99, "text": "Original text",
			"chat": {"id": 123, "type": "supergroup"}, "from": {"id": 456}}
	}`
	var msg TelegramMessage
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if msg.MessageThreadID != 55 || msg.From.FirstName != "Jairo" {
		t.Errorf("message = %+v", msg)
	}
	if msg.ReplyToMessage == nil || msg.ReplyToMessage.MessageID != 99 {
		t.Fatalf("reply_to_message not parsed: %+v", msg.ReplyToMessage)
	}
}

func TestTelegramUpdateJSON(t *testing.T) {
	raw := `{"ok":true,"result":[
		{"update_id":1,"message":{"message_id":2,"text":"hi","chat":{"id":9,"type":"private"},"from":{"id":9}}},
		{"update_id":2,"callback_query":{"id":"cb","data":"q:1:0","from":{"id":9}}},
		{"update_id":3,"edited_message":{"message_id":2,"text":"hi there","chat":{"id":9,"type":"private"},"from":{"id":9}}}
	]}`
	var upd TelegramUpdate
	if err := json.Unmarshal([]byte(raw), &upd); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !upd.OK || len(upd.Result) != 3 {
		t.Fatalf("update = %+v", upd)
	}
	if upd.Result[1].CallbackQuery == nil || upd.Result[1].CallbackQuery.Data != "q:1:0" {
		t.Error("callback_query not parsed")
	}
	if upd.Result[2].EditedMessage == nil || upd.Result[2].EditedMessage.Text != "hi there" {
		t.Error("edited_message not parsed (access control must see edits too)")
	}
}

func TestSplitMessageChunksAtTheLimit(t *testing.T) {
	long := strings.Repeat("a", 9000)
	chunks := splitMessage(long, 4000)
	if len(chunks) < 3 {
		t.Fatalf("got %d chunks, want the text split across at least 3", len(chunks))
	}
	total := 0
	for _, c := range chunks {
		if len(c) > 4000 {
			t.Errorf("chunk of %d bytes exceeds the limit", len(c))
		}
		total += len(c)
	}
	if total != len(long) {
		t.Errorf("chunks total %d bytes, want %d", total, len(long))
	}
}

func TestRenderSystemdUnit(t *testing.T) {
	t.Setenv("GH_TOKEN", `ghp_secret"with\quotes`)

	unit := renderSystemdUnit("/home/u/bin/ccc", &Config{
		EnvPassthrough: []string{"GH_TOKEN", "CLAUDE_CONFIG_DIR", "ANTHROPIC_API_KEY", ""},
	})

	for _, want := range []string{
		"ExecStart=/home/u/bin/ccc listen",
		"Restart=always",
		"WantedBy=default.target",
		// The secrets live in a 0600 file the unit reads, not in the unit.
		"EnvironmentFile=-%h/.config/ccc/env",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("the unit is missing %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "Environment=\"") {
		t.Errorf("a secret was baked into the unit:\n%s", unit)
	}
	if strings.Contains(unit, "ghp_secret") {
		t.Errorf("the unit leaked a value:\n%s", unit)
	}

	// No passthrough names at all means no EnvironmentFile line to read.
	if strings.Contains(renderSystemdUnit("/bin/ccc", &Config{}), "EnvironmentFile") {
		t.Error("an instance with no env_passthrough should not reference an env file")
	}
}
