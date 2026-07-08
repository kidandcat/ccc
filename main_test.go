package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestGetSessionByTopic tests the getSessionByTopic function
func TestGetSessionByTopic(t *testing.T) {
	config := &Config{
		Sessions: map[string]*SessionInfo{
			"project1":   {TopicID: 100, Path: "/home/user/project1"},
			"project2":   {TopicID: 200, Path: "/home/user/project2"},
			"money/shop": {TopicID: 300, Path: "/home/user/money/shop"},
		},
	}

	tests := []struct {
		name     string
		topicID  int64
		expected string
	}{
		{"existing topic", 100, "project1"},
		{"another existing", 200, "project2"},
		{"nested path", 300, "money/shop"},
		{"non-existent", 999, ""},
		{"zero", 0, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getSessionByTopic(config, tt.topicID)
			if result != tt.expected {
				t.Errorf("getSessionByTopic(config, %d) = %q, want %q", tt.topicID, result, tt.expected)
			}
		})
	}
}

// TestGetSessionByTopicNilSessions tests with nil sessions map
func TestGetSessionByTopicNilSessions(t *testing.T) {
	config := &Config{
		Sessions: nil,
	}
	result := getSessionByTopic(config, 100)
	if result != "" {
		t.Errorf("getSessionByTopic with nil sessions = %q, want empty string", result)
	}
}

// TestConfigSaveLoad tests saving and loading config
func TestConfigSaveLoad(t *testing.T) {
	// Create temp directory for test
	tmpDir, err := os.MkdirTemp("", "ccc-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Override config path for test
	originalHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", originalHome)

	// Test config
	config := &Config{
		BotToken: "test-token-123",
		ChatID:   12345,
		GroupID:  -67890,
		Sessions: map[string]*SessionInfo{
			"project1":   {TopicID: 100, Path: "/home/user/project1"},
			"money/shop": {TopicID: 200, Path: "/home/user/money/shop"},
		},
		Away: true,
	}

	// Save config
	if err := saveConfig(config); err != nil {
		t.Fatalf("saveConfig failed: %v", err)
	}

	// Verify file exists
	configPath := filepath.Join(tmpDir, ".config", "ccc", "config.json")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		t.Fatal("Config file was not created")
	}

	// Load config
	loaded, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig failed: %v", err)
	}

	// Verify loaded config matches
	if loaded.BotToken != config.BotToken {
		t.Errorf("BotToken = %q, want %q", loaded.BotToken, config.BotToken)
	}
	if loaded.ChatID != config.ChatID {
		t.Errorf("ChatID = %d, want %d", loaded.ChatID, config.ChatID)
	}
	if loaded.GroupID != config.GroupID {
		t.Errorf("GroupID = %d, want %d", loaded.GroupID, config.GroupID)
	}
	if loaded.Away != config.Away {
		t.Errorf("Away = %v, want %v", loaded.Away, config.Away)
	}
	if len(loaded.Sessions) != len(config.Sessions) {
		t.Errorf("Sessions length = %d, want %d", len(loaded.Sessions), len(config.Sessions))
	}
	for name, info := range config.Sessions {
		loadedInfo := loaded.Sessions[name]
		if loadedInfo == nil || loadedInfo.TopicID != info.TopicID {
			t.Errorf("Sessions[%q].TopicID mismatch", name)
		}
	}
}

// TestConfigLoadNonExistent tests loading non-existent config
func TestConfigLoadNonExistent(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ccc-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	originalHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", originalHome)

	_, err = loadConfig()
	if err == nil {
		t.Error("loadConfig should fail for non-existent file")
	}
}

// TestConfigSessionsInitialized tests that Sessions map is initialized on load
func TestConfigSessionsInitialized(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ccc-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	originalHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", originalHome)

	// Write config without sessions field
	configPath := filepath.Join(tmpDir, ".ccc.json")
	data := []byte(`{"bot_token": "test", "chat_id": 123}`)
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatalf("Failed to write test config: %v", err)
	}

	loaded, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig failed: %v", err)
	}

	if loaded.Sessions == nil {
		t.Error("Sessions should be initialized to non-nil map")
	}
}

// TestExtractRecentAssistantTexts tests parsing transcript JSONL files
func TestExtractRecentAssistantTexts(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ccc-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	tests := []struct {
		name     string
		content  string
		expected []string // expected texts in order
	}{
		{
			name: "simple response with one text block",
			content: `{"type":"assistant","requestId":"req_2","message":{"role":"assistant","content":[{"type":"text","text":"Hello! How can I help?"}]}}`,
			expected: []string{"Hello! How can I help?"},
		},
		{
			name: "multiple text blocks in one entry",
			content: `{"type":"assistant","requestId":"req_2","message":{"role":"assistant","content":[{"type":"text","text":"First part"},{"type":"text","text":"Second part"}]}}`,
			expected: []string{"First part", "Second part"},
		},
		{
			name: "filters thinking and tool_use",
			content: `{"type":"assistant","requestId":"req_2","message":{"role":"assistant","content":[{"type":"thinking","thinking":"let me think..."},{"type":"text","text":"Here is my answer"},{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`,
			expected: []string{"Here is my answer"},
		},
		{
			name: "streaming dedup same requestId keeps last",
			content: `{"type":"assistant","requestId":"req_2","message":{"role":"assistant","content":[{"type":"text","text":"partial response..."}]}}
{"type":"assistant","requestId":"req_2","message":{"role":"assistant","content":[{"type":"text","text":"complete response with more detail"}]}}`,
			expected: []string{"complete response with more detail"},
		},
		{
			name: "returns ALL turns (not just last)",
			content: `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"first question"}]}}
{"type":"assistant","requestId":"req_2","message":{"role":"assistant","content":[{"type":"text","text":"first answer"}]}}
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"second question"}]}}
{"type":"assistant","requestId":"req_4","message":{"role":"assistant","content":[{"type":"text","text":"second answer"}]}}`,
			expected: []string{"first answer", "second answer"},
		},
		{
			name:     "empty file returns nil",
			content:  "",
			expected: nil,
		},
		{
			name:    "no assistant messages returns nil",
			content: `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`,
			expected: nil,
		},
		{
			name: "filters no content",
			content: `{"type":"assistant","requestId":"req_2","message":{"role":"assistant","content":[{"type":"text","text":"(no content)"},{"type":"text","text":"real content"}]}}`,
			expected: []string{"real content"},
		},
		{
			name: "skips error entries without requestId",
			content: `{"type":"assistant","requestId":"req_2","message":{"role":"assistant","content":[{"type":"text","text":"good"}]}}
{"type":"assistant","isApiErrorMessage":true,"message":{"role":"assistant","content":[{"type":"text","text":"No response requested."}]}}`,
			expected: []string{"good"},
		},
		{
			name: "multiple requestIds all returned",
			content: `{"type":"assistant","requestId":"req_2","message":{"role":"assistant","content":[{"type":"text","text":"running tool"}]}}
{"type":"assistant","requestId":"req_4","message":{"role":"assistant","content":[{"type":"text","text":"tool completed"}]}}`,
			expected: []string{"running tool", "tool completed"},
		},
		{
			name: "tail count limits results",
			content: `{"type":"assistant","requestId":"req_1","message":{"role":"assistant","content":[{"type":"text","text":"old message"}]}}
{"type":"assistant","requestId":"req_2","message":{"role":"assistant","content":[{"type":"text","text":"recent message"}]}}`,
			expected: []string{"recent message"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filePath := filepath.Join(tmpDir, tt.name+".jsonl")
			if err := os.WriteFile(filePath, []byte(tt.content), 0644); err != nil {
				t.Fatalf("Failed to write test file: %v", err)
			}

			tailCount := 80
			if tt.name == "tail count limits results" {
				tailCount = 1 // only keep last entry
			}
			blocks := extractRecentAssistantTexts(filePath, tailCount)
			var result []string
			for _, b := range blocks {
				result = append(result, b.text)
			}
			if tt.expected == nil {
				if result != nil {
					t.Errorf("got %v, want nil", result)
				}
				return
			}
			if len(result) != len(tt.expected) {
				t.Errorf("returned %d blocks, want %d: %v", len(result), len(tt.expected), result)
				return
			}
			for i, exp := range tt.expected {
				if result[i] != exp {
					t.Errorf("block %d = %q, want %q", i, result[i], exp)
				}
			}
		})
	}
}

// TestExtractRecentNonExistent tests with non-existent file
func TestExtractRecentNonExistent(t *testing.T) {
	result := extractRecentAssistantTexts("/nonexistent/path/file.jsonl", 80)
	if result != nil {
		t.Errorf("non-existent file = %v, want nil", result)
	}
}

// TestExtractRecentEmptyPath tests with empty path
func TestExtractRecentEmptyPath(t *testing.T) {
	result := extractRecentAssistantTexts("", 80)
	if result != nil {
		t.Errorf("empty path = %v, want nil", result)
	}
}

// TestExecuteCommand tests the executeCommand function
func TestExecuteCommand(t *testing.T) {
	tests := []struct {
		name        string
		cmd         string
		wantContain string
		wantErr     bool
	}{
		{"echo", "echo hello", "hello", false},
		{"pwd", "pwd", "/", false},
		{"invalid command", "nonexistentcommand123", "", true},
		{"exit code", "exit 1", "", true},
		{"stderr output", "echo error >&2", "error", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, err := executeCommand(tt.cmd)
			if (err != nil) != tt.wantErr {
				t.Errorf("executeCommand(%q) error = %v, wantErr %v", tt.cmd, err, tt.wantErr)
			}
			if tt.wantContain != "" && !contains(output, tt.wantContain) {
				t.Errorf("executeCommand(%q) output = %q, want to contain %q", tt.cmd, output, tt.wantContain)
			}
		})
	}
}

// TestConfigJSON tests JSON marshaling/unmarshaling
func TestConfigJSON(t *testing.T) {
	config := &Config{
		BotToken: "token123",
		ChatID:   12345,
		GroupID:  -67890,
		Sessions: map[string]*SessionInfo{
			"test": {TopicID: 100, Path: "/home/user/test"},
		},
		Away: true,
	}

	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var loaded Config
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if loaded.BotToken != config.BotToken {
		t.Errorf("BotToken mismatch")
	}
}

// TestHookDataJSON tests HookData JSON parsing
func TestHookDataJSON(t *testing.T) {
	jsonStr := `{"cwd":"/Users/test/project","transcript_path":"/tmp/transcript.jsonl","session_id":"abc123"}`

	var hookData HookData
	if err := json.Unmarshal([]byte(jsonStr), &hookData); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if hookData.Cwd != "/Users/test/project" {
		t.Errorf("Cwd = %q, want %q", hookData.Cwd, "/Users/test/project")
	}
	if hookData.TranscriptPath != "/tmp/transcript.jsonl" {
		t.Errorf("TranscriptPath = %q, want %q", hookData.TranscriptPath, "/tmp/transcript.jsonl")
	}
	if hookData.SessionID != "abc123" {
		t.Errorf("SessionID = %q, want %q", hookData.SessionID, "abc123")
	}
}

// TestTelegramMessageJSON tests TelegramMessage JSON parsing
func TestTelegramMessageJSON(t *testing.T) {
	jsonStr := `{
		"message_id": 123,
		"message_thread_id": 456,
		"chat": {"id": 789, "type": "supergroup"},
		"from": {"id": 111, "username": "testuser"},
		"text": "Hello world"
	}`

	var msg TelegramMessage
	if err := json.Unmarshal([]byte(jsonStr), &msg); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if msg.MessageID != 123 {
		t.Errorf("MessageID = %d, want 123", msg.MessageID)
	}
	if msg.MessageThreadID != 456 {
		t.Errorf("MessageThreadID = %d, want 456", msg.MessageThreadID)
	}
	if msg.Chat.ID != 789 {
		t.Errorf("Chat.ID = %d, want 789", msg.Chat.ID)
	}
	if msg.Chat.Type != "supergroup" {
		t.Errorf("Chat.Type = %q, want supergroup", msg.Chat.Type)
	}
	if msg.From.Username != "testuser" {
		t.Errorf("From.Username = %q, want testuser", msg.From.Username)
	}
	if msg.Text != "Hello world" {
		t.Errorf("Text = %q, want 'Hello world'", msg.Text)
	}
}

// TestMessageTruncation tests that long messages are truncated
func TestMessageTruncation(t *testing.T) {
	// The sendMessage function truncates at 4000 chars
	// We test the truncation logic directly
	const maxLen = 4000

	tests := []struct {
		name       string
		inputLen   int
		shouldTrim bool
	}{
		{"short message", 100, false},
		{"exactly max", maxLen, false},
		{"over max", maxLen + 100, true},
		{"way over max", 10000, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create message of specified length
			text := make([]byte, tt.inputLen)
			for i := range text {
				text[i] = 'a'
			}
			msg := string(text)

			// Apply same truncation logic as sendMessage
			if len(msg) > maxLen {
				msg = msg[:maxLen] + "\n... (truncated)"
			}

			if tt.shouldTrim {
				if len(msg) <= tt.inputLen {
					// Should have been truncated
					if len(msg) != maxLen+len("\n... (truncated)") {
						t.Errorf("truncated length = %d, want %d", len(msg), maxLen+len("\n... (truncated)"))
					}
				}
			} else {
				if len(msg) != tt.inputLen {
					t.Errorf("message was unexpectedly modified")
				}
			}
		})
	}
}

// TestConfigFilePermissions tests that config is saved with correct permissions
func TestConfigFilePermissions(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ccc-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	originalHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", originalHome)

	config := &Config{
		BotToken: "secret-token",
		ChatID:   12345,
		Sessions: make(map[string]*SessionInfo),
	}

	if err := saveConfig(config); err != nil {
		t.Fatalf("saveConfig failed: %v", err)
	}

	configPath := filepath.Join(tmpDir, ".config", "ccc", "config.json")
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("Failed to stat config file: %v", err)
	}

	// Check permissions are 0600 (owner read/write only)
	perm := info.Mode().Perm()
	if perm != 0600 {
		t.Errorf("Config file permissions = %o, want 0600", perm)
	}
}

// TestEmptySessionsMap tests behavior with empty sessions
func TestEmptySessionsMap(t *testing.T) {
	config := &Config{
		Sessions: make(map[string]*SessionInfo),
	}

	result := getSessionByTopic(config, 100)
	if result != "" {
		t.Errorf("getSessionByTopic with empty sessions = %q, want empty", result)
	}
}

// TestTopicResultJSON tests TopicResult JSON parsing
func TestTopicResultJSON(t *testing.T) {
	jsonStr := `{"message_thread_id": 12345, "name": "test-topic"}`

	var topic TopicResult
	if err := json.Unmarshal([]byte(jsonStr), &topic); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if topic.MessageThreadID != 12345 {
		t.Errorf("MessageThreadID = %d, want 12345", topic.MessageThreadID)
	}
	if topic.Name != "test-topic" {
		t.Errorf("Name = %q, want test-topic", topic.Name)
	}
}

// TestTelegramResponseJSON tests TelegramResponse JSON parsing
func TestTelegramResponseJSON(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		wantOK  bool
		wantErr string
	}{
		{
			name:   "success response",
			json:   `{"ok": true, "result": {}}`,
			wantOK: true,
		},
		{
			name:    "error response",
			json:    `{"ok": false, "description": "Bad Request"}`,
			wantOK:  false,
			wantErr: "Bad Request",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var resp TelegramResponse
			if err := json.Unmarshal([]byte(tt.json), &resp); err != nil {
				t.Fatalf("Unmarshal failed: %v", err)
			}

			if resp.OK != tt.wantOK {
				t.Errorf("OK = %v, want %v", resp.OK, tt.wantOK)
			}
			if resp.Description != tt.wantErr {
				t.Errorf("Description = %q, want %q", resp.Description, tt.wantErr)
			}
		})
	}
}

// TestReplyToMessage tests nested message parsing
func TestReplyToMessage(t *testing.T) {
	jsonStr := `{
		"message_id": 100,
		"text": "Reply text",
		"chat": {"id": 123, "type": "private"},
		"from": {"id": 456, "username": "user"},
		"reply_to_message": {
			"message_id": 99,
			"text": "Original text",
			"chat": {"id": 123, "type": "private"},
			"from": {"id": 456, "username": "user"}
		}
	}`

	var msg TelegramMessage
	if err := json.Unmarshal([]byte(jsonStr), &msg); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if msg.ReplyToMessage == nil {
		t.Fatal("ReplyToMessage should not be nil")
	}
	if msg.ReplyToMessage.MessageID != 99 {
		t.Errorf("ReplyToMessage.MessageID = %d, want 99", msg.ReplyToMessage.MessageID)
	}
	if msg.ReplyToMessage.Text != "Original text" {
		t.Errorf("ReplyToMessage.Text = %q, want 'Original text'", msg.ReplyToMessage.Text)
	}
}

// TestLedgerAppendAndRead tests basic ledger operations
func TestLedgerAppendAndRead(t *testing.T) {
	// Use a unique session name with temp suffix so the ledger file doesn't collide
	session := "test-ledger-" + filepath.Base(t.TempDir())
	// Clean up after test
	defer os.Remove(ledgerPath(session))

	// Append a message
	rec := &MessageRecord{
		ID:                "test:1",
		Session:           session,
		Type:              "user_prompt",
		Text:              "hello world",
		Origin:            "telegram",
		TerminalDelivered: false,
		TelegramDelivered: true,
	}
	if err := appendMessage(rec); err != nil {
		t.Fatalf("appendMessage failed: %v", err)
	}

	// Read back
	records := readLedger(session)
	if len(records) != 1 {
		t.Fatalf("readLedger returned %d records, want 1", len(records))
	}
	if records[0].ID != "test:1" {
		t.Errorf("ID = %q, want test:1", records[0].ID)
	}
	if records[0].TerminalDelivered {
		t.Error("TerminalDelivered should be false")
	}

	// Update delivery
	if err := updateDelivery(session, "test:1", "terminal_delivered", true); err != nil {
		t.Fatalf("updateDelivery failed: %v", err)
	}

	// Read again — should be merged
	records = readLedger(session)
	if len(records) != 1 {
		t.Fatalf("readLedger returned %d records after update, want 1", len(records))
	}
	if !records[0].TerminalDelivered {
		t.Error("TerminalDelivered should be true after update")
	}

	// Test isDelivered
	if !isDelivered(session, "test:1", "terminal") {
		t.Error("isDelivered(terminal) should be true")
	}
	if !isDelivered(session, "test:1", "telegram") {
		t.Error("isDelivered(telegram) should be true")
	}

	// Test findUndelivered
	appendMessage(&MessageRecord{
		ID:                "test:2",
		Session:           session,
		Type:              "assistant_text",
		Text:              "response",
		Origin:            "claude",
		TerminalDelivered: true,
		TelegramDelivered: false,
	})

	undelivered := findUndelivered(session, "telegram")
	if len(undelivered) != 1 {
		t.Fatalf("findUndelivered(telegram) returned %d, want 1", len(undelivered))
	}
	if undelivered[0].ID != "test:2" {
		t.Errorf("undelivered ID = %q, want test:2", undelivered[0].ID)
	}
}

// TestLedgerDedup tests that contentHash produces consistent hashes
func TestLedgerDedup(t *testing.T) {
	h1 := contentHash("hello world")
	h2 := contentHash("hello world")
	h3 := contentHash("different text")

	if h1 != h2 {
		t.Errorf("same content produced different hashes: %s vs %s", h1, h2)
	}
	if h1 == h3 {
		t.Error("different content produced same hash")
	}
}

// Helper function
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// TestTitleFromPrompt tests the topic title derived from a /new prompt
func TestTitleFromPrompt(t *testing.T) {
	tests := []struct {
		prompt string
		want   string
	}{
		{"fix the swipe decoder", "fix the swipe decoder"},
		{"  fix   the\nswipe  decoder ", "fix the swipe decoder"},
		{"", "session"},
		{"   ", "session"},
		{"arregla el bug del parser de fechas en el backend", "arregla el bug del parser de fechas en e…"},
		{"añade soporte para emojis 🎉 en el título de la sesión", "añade soporte para emojis 🎉 en el título…"},
	}
	for _, tt := range tests {
		if got := titleFromPrompt(tt.prompt); got != tt.want {
			t.Errorf("titleFromPrompt(%q) = %q, want %q", tt.prompt, got, tt.want)
		}
	}
}

// TestUniqueSessionName tests collision handling for session names
func TestUniqueSessionName(t *testing.T) {
	config := &Config{Sessions: map[string]*SessionInfo{
		"deploy":     {TopicID: 1},
		"deploy (2)": {TopicID: 2},
	}}
	if got := uniqueSessionName(config, "build"); got != "build" {
		t.Errorf("uniqueSessionName free name = %q, want %q", got, "build")
	}
	if got := uniqueSessionName(config, "deploy"); got != "deploy (3)" {
		t.Errorf("uniqueSessionName collision = %q, want %q", got, "deploy (3)")
	}
}

// TestAgentDisplayName tests that the fleet name follows the topic title
func TestAgentDisplayName(t *testing.T) {
	if got := agentDisplayName(&SessionInfo{Title: "~/ccc: fix decoder"}, "fix decoder"); got != "~/ccc: fix decoder" {
		t.Errorf("agentDisplayName with title = %q", got)
	}
	if got := agentDisplayName(&SessionInfo{}, "fix decoder"); got != "fix decoder" {
		t.Errorf("agentDisplayName without title = %q", got)
	}
	if got := agentDisplayName(nil, "fix decoder"); got != "fix decoder" {
		t.Errorf("agentDisplayName nil = %q", got)
	}
}

// TestSessionWorkDir tests the $HOME default for /new sessions
func TestSessionWorkDir(t *testing.T) {
	home, _ := os.UserHomeDir()
	if got := sessionWorkDir(&SessionInfo{Path: "/tmp/x"}); got != "/tmp/x" {
		t.Errorf("sessionWorkDir explicit = %q", got)
	}
	if got := sessionWorkDir(&SessionInfo{}); got != home {
		t.Errorf("sessionWorkDir default = %q, want %q", got, home)
	}
}
