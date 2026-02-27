package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// telegramActiveFlag returns the path of the flag file that indicates
// a Telegram message is being processed by a tmux session.
func telegramActiveFlag(tmuxName string) string {
	return filepath.Join(cacheDir(), "telegram-active-"+tmuxName)
}

// thinkingFlag returns the path of the flag file that indicates
// Claude is actively processing in a session (for typing indicator).
func thinkingFlag(sessionName string) string {
	return filepath.Join(cacheDir(), "thinking-"+sessionName)
}

func setThinking(sessionName string) {
	os.WriteFile(thinkingFlag(sessionName), []byte("1"), 0600)
}

func clearThinking(sessionName string) {
	os.Remove(thinkingFlag(sessionName))
}

// promptAckPath returns the path of the ack file that confirms
// Claude received a prompt sent from Telegram via tmux send-keys.
func promptAckPath(sessionName string) string {
	return filepath.Join(cacheDir(), "prompt-ack-"+sessionName)
}

func writePromptAck(sessionName string) {
	os.WriteFile(promptAckPath(sessionName), []byte("1"), 0600)
}

func clearPromptAck(sessionName string) {
	os.Remove(promptAckPath(sessionName))
}

// waitPromptAck polls for the ack file, returning true if it appears within timeout
func waitPromptAck(sessionName string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(promptAckPath(sessionName)); err == nil {
			os.Remove(promptAckPath(sessionName))
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}


// addTextToToolState adds an assistant text block to the tool state, ordered by timestamp.
func addTextToToolState(sessName string, text string, ts int64) {
	state := loadToolState(sessName)
	if state.MsgID == 0 {
		return
	}
	state.Tools = append(state.Tools, ToolCall{IsText: true, Input: text, Time: ts})
	// Sort all entries by timestamp
	sort.Slice(state.Tools, func(i, j int) bool {
		return state.Tools[i].Time < state.Tools[j].Time
	})
	saveToolState(sessName, state)
}

// collapseToolMessage is a no-op now (no folding).
func collapseToolMessage(config *Config, sessName string, topicID int64) {
}

// htmlEscape escapes special HTML characters
func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// formatToolLines builds tool lines without blockquote wrapper
func formatToolLines(state *ToolState) string {
	var lines []string
	for _, t := range state.Tools {
		if t.IsText {
			lines = append(lines, fmt.Sprintf("💬 %s", htmlEscape(t.Input)))
		} else if t.Name == "" {
			lines = append(lines, fmt.Sprintf("⚙️ %s", htmlEscape(t.Input)))
		} else if t.Input != "" {
			lines = append(lines, fmt.Sprintf("⚙️ %s: %s", htmlEscape(t.Name), htmlEscape(t.Input)))
		} else {
			lines = append(lines, fmt.Sprintf("⚙️ %s", htmlEscape(t.Name)))
		}
	}
	return strings.Join(lines, "\n")
}

// formatToolMessage builds blockquote (expanded during tool calls)
func formatToolMessage(state *ToolState) string {
	return "<blockquote>" + formatToolLines(state) + "</blockquote>"
}

// formatToolMessageCollapsed builds expandable blockquote (after tools complete)
func formatToolMessageCollapsed(state *ToolState) string {
	return "<blockquote expandable>" + formatToolLines(state) + "</blockquote>"
}

// toolInputSummary extracts a short description from tool input
func toolInputSummary(hookData HookData) string {
	truncAt := 80
	trunc := func(s string) string {
		if len(s) > truncAt {
			return s[:truncAt] + "..."
		}
		return s
	}

	switch hookData.ToolName {
	case "Bash":
		return trunc(hookData.ToolInput.Command)
	case "Read", "Write":
		return hookData.ToolInput.FilePath
	case "Edit":
		s := hookData.ToolInput.FilePath
		if hookData.ToolInput.OldString != "" {
			preview := hookData.ToolInput.OldString
			if len(preview) > 40 {
				preview = preview[:40] + "..."
			}
			s += " `" + strings.ReplaceAll(preview, "\n", "↵") + "`"
		}
		return s
	case "Grep":
		if hookData.ToolInput.Pattern != "" {
			return trunc(hookData.ToolInput.Pattern)
		}
		return hookData.ToolInput.Description
	case "Glob":
		if hookData.ToolInput.Pattern != "" {
			return trunc(hookData.ToolInput.Pattern)
		}
		return hookData.ToolInput.Description
	case "WebSearch":
		return trunc(hookData.ToolInput.Query)
	case "WebFetch":
		return trunc(hookData.ToolInput.URL)
	case "Task":
		return trunc(hookData.ToolInput.Description)
	default:
		if hookData.ToolInput.Description != "" {
			return trunc(hookData.ToolInput.Description)
		}
		return ""
	}
}

// readHookStdin reads stdin JSON with a timeout
func readHookStdin() ([]byte, error) {
	stdinData := make(chan []byte, 1)
	go func() {
		defer func() { recover() }()
		data, _ := io.ReadAll(os.Stdin)
		stdinData <- data
	}()

	select {
	case rawData := <-stdinData:
		return rawData, nil
	case <-time.After(2 * time.Second):
		return nil, nil
	}
}

// findSessionByClaudeID matches a claude session ID to a configured session
func findSessionByClaudeID(config *Config, claudeSessionID string) (string, int64) {
	if claudeSessionID == "" {
		return "", 0
	}
	for name, info := range config.Sessions {
		if name == "" || info == nil {
			continue
		}
		if info.ClaudeSessionID == claudeSessionID {
			return name, info.TopicID
		}
	}
	return "", 0
}

// findSessionByCwd matches a hook's cwd to a configured session (fallback)
func findSessionByCwd(config *Config, cwd string) (string, int64) {
	for name, info := range config.Sessions {
		if name == "" || info == nil {
			continue
		}
		if cwd == info.Path || strings.HasPrefix(cwd, info.Path+"/") || strings.HasSuffix(cwd, "/"+name) {
			return name, info.TopicID
		}
	}
	return "", 0
}

// findSession matches by claude_session_id first, then falls back to cwd
func findSession(config *Config, cwd string, claudeSessionID string) (string, int64) {
	if name, topicID := findSessionByClaudeID(config, claudeSessionID); name != "" {
		return name, topicID
	}
	return findSessionByCwd(config, cwd)
}

// persistClaudeSessionID saves the claude session ID to config if changed
func persistClaudeSessionID(config *Config, sessName string, claudeSessionID string) {
	if claudeSessionID == "" || sessName == "" {
		return
	}
	info, exists := config.Sessions[sessName]
	if !exists || info == nil {
		return
	}
	if info.ClaudeSessionID != claudeSessionID {
		info.ClaudeSessionID = claudeSessionID
		saveConfig(config)
		hookLog("persisted claude_session_id=%s for session=%s", claudeSessionID, sessName)
	}
}

func handleStopHook() error {
	defer func() { recover() }()

	rawData, _ := readHookStdin()
	if len(rawData) == 0 {
		return nil
	}

	hookData, err := parseHookData(rawData)
	if err != nil {
		return nil
	}

	config, err := loadConfig()
	if err != nil || config == nil {
		return nil
	}

	sessName, topicID := findSession(config, hookData.Cwd, hookData.SessionID)
	if sessName == "" || config.GroupID == 0 || topicID == 0 {
		return nil
	}

	// Persist claude session ID to config for future lookups
	persistClaudeSessionID(config, sessName, hookData.SessionID)

	hookLog("stop-hook: session=%s claude_session_id=%s transcript=%s", sessName, hookData.SessionID, hookData.TranscriptPath)

	// Clear flags when Claude stops
	tmuxName := tmuxSafeName(sessName)
	os.Remove(telegramActiveFlag(tmuxName))
	clearThinking(sessName)

	// Deliver unsent texts as separate messages (these come after all tools)
	hookLog("stop-hook: delivering unsent texts")
	sent := deliverUnsentTexts(config, sessName, topicID, hookData.TranscriptPath, false)
	hookLog("stop-hook: sent=%d", sent)
	clearToolState(sessName)

	// Background retry: transcript may not be flushed yet when stop hook fires.
	// Spawn a detached subprocess that retries 3 times at 2-second intervals.
	// (goroutines die when the hook process exits, so we need a separate process)
	cmd := exec.Command(cccPath, "hook-stop-retry", sessName, fmt.Sprintf("%d", topicID), hookData.TranscriptPath)
	cmd.Start()

	return nil
}

// deliverUnsentTexts scans transcript tail and sends any assistant text
// blocks not yet delivered to Telegram (using ledger dedup).
// If insertIntoToolMsg is true and tool state has a message, texts are inserted
// into the tool blockquote (for text before/between tools in PreToolUse).
// If false, texts are sent as separate messages (for text after tools in Stop hook).
func deliverUnsentTexts(config *Config, sessName string, topicID int64, transcriptPath string, insertIntoToolMsg bool) int {
	blocks := extractRecentAssistantTexts(transcriptPath, 80)
	lastPreview := ""
	if len(blocks) > 0 {
		lastPreview = truncate(blocks[len(blocks)-1].text, 60)
	}
	hookLog("deliver-unsent: found %d blocks, last=%s", len(blocks), lastPreview)

	sent := 0
	for _, block := range blocks {
		blockID := fmt.Sprintf("reply:%s:%s", block.requestID, contentHash(block.text))
		if isDelivered(sessName, blockID, "telegram") {
			continue
		}
		hookLog("deliver-text: rid=%s len=%d insert=%v preview=%s", block.requestID, len(block.text), insertIntoToolMsg, truncate(block.text, 80))

		state := loadToolState(sessName)
		if insertIntoToolMsg && state.MsgID != 0 {
			// Insert into tool blockquote at correct time position
			addTextToToolState(sessName, block.text, time.Now().UnixMilli())
			state = loadToolState(sessName)
			text := formatToolMessage(state)
			editMessageHTML(config, config.GroupID, state.MsgID, topicID, text)
			appendMessage(&MessageRecord{
				ID: blockID, Session: sessName, Type: "assistant_text",
				Text: truncate(block.text, 500), Origin: "claude",
				TerminalDelivered: true, TelegramDelivered: true, TelegramMsgID: state.MsgID,
			})
		} else {
			// Send as separate message
			msg := fmt.Sprintf("*%s:*\n%s", sessName, block.text)
			tgMsgID, err := sendMessageGetID(config, config.GroupID, topicID, msg)
			if err != nil {
				hookLog("deliver-text: send failed, retrying: %v", err)
				time.Sleep(500 * time.Millisecond)
				tgMsgID, _ = sendMessageGetID(config, config.GroupID, topicID, msg)
			}
			appendMessage(&MessageRecord{
				ID: blockID, Session: sessName, Type: "assistant_text",
				Text: truncate(block.text, 500), Origin: "claude",
				TerminalDelivered: true, TelegramDelivered: tgMsgID > 0, TelegramMsgID: tgMsgID,
			})
		}
		sent++
	}
	return sent
}

// assistantTextBlock pairs extracted text with its requestId for dedup
type assistantTextBlock struct {
	requestID string
	text      string
}

// extractRecentAssistantTexts reads the last N assistant entries from the
// transcript and returns their text blocks. The caller uses ledger dedup
// to avoid resending previously delivered messages.
func extractRecentAssistantTexts(transcriptPath string, tailCount int) []assistantTextBlock {
	if transcriptPath == "" {
		return nil
	}

	f, err := os.Open(transcriptPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	type transcriptLine struct {
		Type             string `json:"type"`
		RequestID        string `json:"requestId,omitempty"`
		IsApiErrorMessage bool  `json:"isApiErrorMessage,omitempty"`
		Message          struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}

	type contentBlock struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}

	// Read only the tail of the file (last 512KB) to avoid scanning the entire transcript
	const tailBytes = 512 * 1024
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	offset := int64(0)
	if fi.Size() > tailBytes {
		offset = fi.Size() - tailBytes
		f.Seek(offset, 0)
	}
	tailData, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	// If we seeked into the middle of a line, skip the first partial line
	if offset > 0 {
		if idx := bytes.IndexByte(tailData, '\n'); idx >= 0 {
			tailData = tailData[idx+1:]
		}
	}

	type entry struct {
		requestID string
		content   json.RawMessage
	}

	var entries []entry
	for _, line := range bytes.Split(tailData, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var tl transcriptLine
		if json.Unmarshal(line, &tl) != nil {
			continue
		}
		if tl.Type != "assistant" || tl.Message.Role != "assistant" {
			continue
		}
		if tl.IsApiErrorMessage || tl.RequestID == "" {
			continue
		}
		entries = append(entries, entry{
			requestID: tl.RequestID,
			content:   tl.Message.Content,
		})
	}

	// Take only the tail
	if len(entries) > tailCount {
		entries = entries[len(entries)-tailCount:]
	}

	// For each requestId, keep only the last entry's text (later entries
	// supersede earlier ones for the same request, e.g. streaming updates)
	type ridText struct {
		requestID string
		texts     []string
	}
	seen := make(map[string]int) // requestID -> index in result
	var ordered []ridText

	for _, e := range entries {
		var blocks []contentBlock
		if json.Unmarshal(e.content, &blocks) != nil {
			continue
		}
		var texts []string
		for _, b := range blocks {
			if b.Type != "text" {
				continue
			}
			t := strings.TrimSpace(b.Text)
			if t != "" && t != "(no content)" {
				texts = append(texts, t)
			}
		}
		if len(texts) == 0 {
			continue
		}
		if idx, ok := seen[e.requestID]; ok {
			ordered[idx].texts = texts // overwrite with later entry
		} else {
			seen[e.requestID] = len(ordered)
			ordered = append(ordered, ridText{requestID: e.requestID, texts: texts})
		}
	}

	var result []assistantTextBlock
	for _, rt := range ordered {
		for _, t := range rt.texts {
			result = append(result, assistantTextBlock{requestID: rt.requestID, text: t})
		}
	}
	return result
}


// handleStopRetry is a background process spawned by stop hook.
// It retries transcript reading 3 times at 2-second intervals to catch
// messages that weren't flushed when the stop hook first fired.
func handleStopRetry(sessName string, topicID int64, transcriptPath string) error {
	config, err := loadConfig()
	if err != nil || config == nil {
		return nil
	}
	for i := 0; i < 3; i++ {
		time.Sleep(2 * time.Second)
		n := deliverUnsentTexts(config, sessName, topicID, transcriptPath, false)
		hookLog("stop-retry: %d/3 sent=%d session=%s", i+1, n, sessName)
	}
	return nil
}

func handlePermissionHook() error {
	defer func() { recover() }()

	rawData, _ := readHookStdin()
	if len(rawData) == 0 {
		return nil
	}

	hookData, err := parseHookData(rawData)
	if err != nil {
		return nil
	}

	config, err := loadConfig()
	if err != nil || config == nil {
		return nil
	}

	sessName, topicID := findSession(config, hookData.Cwd, hookData.SessionID)
	if sessName == "" || config.GroupID == 0 {
		return nil
	}

	// Persist claude session ID to config for future lookups
	persistClaudeSessionID(config, sessName, hookData.SessionID)

	hookLog("pre-tool: session=%s tool=%s", sessName, hookData.ToolName)

	// Deliver any unsent assistant text before showing tool calls
	if topicID != 0 && hookData.TranscriptPath != "" {
		deliverUnsentTexts(config, sessName, topicID, hookData.TranscriptPath, true)
	}

	// Update tool call display
	if hookData.ToolName != "" && hookData.ToolName != "AskUserQuestion" && topicID != 0 {
		state := loadToolState(sessName)
		state.Tools = append(state.Tools, ToolCall{
			Name: hookData.ToolName,
			Input: toolInputSummary(hookData),
			Time: time.Now().UnixMilli(),
		})
		text := formatToolMessage(state)
		if state.MsgID == 0 {
			msgID, err := sendMessageHTMLGetID(config, config.GroupID, topicID, text)
			if err == nil && msgID > 0 {
				state.MsgID = msgID
			}
		} else {
			editMessageHTML(config, config.GroupID, state.MsgID, topicID, text)
		}
		saveToolState(sessName, state)

		// Record tool call in ledger
		appendMessage(&MessageRecord{
			ID:                fmt.Sprintf("tool:%s:%s:%d", hookData.SessionID, contentHash(hookData.ToolName+toolInputSummary(hookData)), time.Now().UnixNano()),
			Session:           sessName,
			Type:              "tool_call",
			Text:              hookData.ToolName + ": " + toolInputSummary(hookData),
			Origin:            "claude",
			TerminalDelivered: true,
			TelegramDelivered: state.MsgID != 0,
			TelegramMsgID:     state.MsgID,
		})
	}

	// Handle AskUserQuestion - forward to Telegram with buttons
	if hookData.ToolName == "AskUserQuestion" && len(hookData.ToolInput.Questions) > 0 {
		for qIdx, q := range hookData.ToolInput.Questions {
			if q.Question == "" {
				continue
			}
			msg := fmt.Sprintf("❓ %s\n\n%s", q.Header, q.Question)

			var buttons [][]InlineKeyboardButton
			for i, opt := range q.Options {
				if opt.Label == "" {
					continue
				}
				totalQuestions := len(hookData.ToolInput.Questions)
				callbackData := fmt.Sprintf("%s:%d:%d:%d", sessName, qIdx, totalQuestions, i)
				if len(callbackData) > 64 {
					callbackData = callbackData[:64]
				}
				buttons = append(buttons, []InlineKeyboardButton{
					{Text: opt.Label, CallbackData: callbackData},
				})
			}

			if len(buttons) > 0 {
				sendMessageWithKeyboard(config, config.GroupID, topicID, msg, buttons)
			}
		}
		return nil
	}

	// OTP permission check for all other tools
	if !isOTPEnabled(config) {
		// No OTP configured, auto-allow everything
		outputPermissionDecision("allow", "OTP not configured")
		return nil
	}

	// OTP only applies when input came from Telegram (flag file exists and is recent).
	// The listener sets this flag before forwarding Telegram messages to tmux.
	// Flag auto-expires after 5 minutes to handle cases where stop hook didn't fire.
	tmuxName := tmuxSafeName(sessName)
	flagInfo, err := os.Stat(telegramActiveFlag(tmuxName))
	if err != nil || time.Since(flagInfo.ModTime()) > otpGrantDuration {
		return nil // no flag or expired, let Claude handle permissions normally
	}

	// Check for a valid OTP grant (approved within the last 5 minutes)
	if hasValidOTPGrant(tmuxName) {
		outputPermissionDecision("allow", "OTP grant still valid")
		return nil
	}

	// Build a human-readable description of what Claude wants to do
	toolDesc := hookData.ToolName
	var inputStr string
	switch hookData.ToolName {
	case "Bash":
		if hookData.ToolInput.Command != "" {
			inputStr = hookData.ToolInput.Command
		}
	case "Read":
		if hookData.ToolInput.FilePath != "" {
			inputStr = hookData.ToolInput.FilePath
		}
	case "Write", "Edit":
		if hookData.ToolInput.FilePath != "" {
			inputStr = hookData.ToolInput.FilePath
		}
	}
	if inputStr == "" {
		inputStr = string(hookData.ToolInputRaw)
	}
	if len(inputStr) > 500 {
		inputStr = inputStr[:500] + "..."
	}

	// Use session_id from hook data as unique identifier
	sessionID := hookData.SessionID
	if sessionID == "" {
		sessionID = sessName
	}

	// Only the first parallel hook sends the Telegram message.
	// If a request file already exists (from another parallel hook), just wait.
	alreadyRequested := false
	if info, err := os.Stat(otpRequestPrefix + sessionID); err == nil {
		alreadyRequested = time.Since(info.ModTime()) < 30*time.Second
	}

	req := &OTPPermissionRequest{
		SessionName: sessName,
		ToolName:    hookData.ToolName,
		ToolInput:   inputStr,
		Timestamp:   time.Now().Unix(),
	}
	writeOTPRequest(sessionID, req)

	if !alreadyRequested {
		msg := fmt.Sprintf("🔐 Permission request:\n\n🔧 %s\n📋 %s\n\nSend your OTP code to approve:", toolDesc, inputStr)
		sendMessage(config, config.GroupID, topicID, msg)
	}

	hookLog("otp-request: waiting for OTP response for session=%s tool=%s already=%v", sessName, hookData.ToolName, alreadyRequested)

	// Wait for OTP response from listener
	approved, err := waitForOTPResponse(sessionID, tmuxName, otpPermissionTimeout)
	if err != nil {
		hookLog("otp-request: timeout or error: %v", err)
		sendMessage(config, config.GroupID, topicID, "⏰ OTP timeout - permission denied")
		outputPermissionDecision("deny", "OTP approval timed out")
		return nil
	}

	if approved {
		hookLog("otp-request: approved for session=%s tool=%s", sessName, hookData.ToolName)
		writeOTPGrant(tmuxName)
		outputPermissionDecision("allow", "Approved via OTP")
	} else {
		hookLog("otp-request: denied for session=%s tool=%s", sessName, hookData.ToolName)
		outputPermissionDecision("deny", "Denied via OTP")
	}

	return nil
}

// outputPermissionDecision writes the PreToolUse hook response to stdout
func outputPermissionDecision(decision, reason string) {
	response := map[string]interface{}{
		"hookSpecificOutput": map[string]interface{}{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       decision,
			"permissionDecisionReason": reason,
		},
	}
	data, _ := json.Marshal(response)
	fmt.Println(string(data))
}

func handleUserPromptHook() error {
	defer func() { recover() }()

	rawData, _ := readHookStdin()
	if len(rawData) == 0 {
		return nil
	}

	hookData, err := parseHookData(rawData)
	if err != nil || hookData.Prompt == "" {
		return nil
	}

	config, err := loadConfig()
	if err != nil || config == nil {
		return nil
	}

	sessName, topicID := findSession(config, hookData.Cwd, hookData.SessionID)
	if sessName == "" || config.GroupID == 0 || topicID == 0 {
		return nil
	}

	persistClaudeSessionID(config, sessName, hookData.SessionID)

	// Collapse tool message from previous turn
	collapseToolMessage(config, sessName, topicID)
	clearToolState(sessName)

	// Skip if this prompt came from Telegram (already visible in the chat).
	// The flag is consumed (deleted) so subsequent TUI prompts are not skipped.
	tmuxName := tmuxSafeName(sessName)
	if flagInfo, err := os.Stat(telegramActiveFlag(tmuxName)); err == nil {
		if time.Since(flagInfo.ModTime()) < 30*time.Second {
			os.Remove(telegramActiveFlag(tmuxName))
			writePromptAck(sessName)
			setThinking(sessName)
			// Confirm terminal delivery for the Telegram message
			confirmTerminalDelivery(sessName, hookData.Prompt)
			// Record: came from Telegram, both sides have it
			appendMessage(&MessageRecord{
				ID:                fmt.Sprintf("prompt:%s:%d", hookData.SessionID, time.Now().UnixNano()),
				Session:           sessName,
				Type:              "user_prompt",
				Text:              hookData.Prompt,
				Origin:            "telegram",
				TerminalDelivered: true,
				TelegramDelivered: true,
			})
			return nil
		}
	}

	setThinking(sessName)

	// Record: came from terminal, Telegram not yet delivered
	msgID := fmt.Sprintf("prompt:%s:%d", hookData.SessionID, time.Now().UnixNano())
	appendMessage(&MessageRecord{
		ID:                msgID,
		Session:           sessName,
		Type:              "user_prompt",
		Text:              hookData.Prompt,
		Origin:            "terminal",
		TerminalDelivered: true,
		TelegramDelivered: false,
	})

	sendMessage(config, config.GroupID, topicID, fmt.Sprintf("💬 %s", hookData.Prompt))
	updateDelivery(sessName, msgID, "telegram_delivered", true)
	return nil
}

func handlePostToolHook() error {
	// No-op: tool completion is implied by the next tool starting
	return nil
}

func handleNotificationHook() error {
	defer func() { recover() }()

	rawData, _ := readHookStdin()
	if len(rawData) == 0 {
		return nil
	}

	hookData, err := parseHookData(rawData)
	if err != nil {
		return nil
	}

	config, err := loadConfig()
	if err != nil || config == nil {
		return nil
	}

	sessName, topicID := findSession(config, hookData.Cwd, hookData.SessionID)
	if sessName == "" || config.GroupID == 0 || topicID == 0 {
		return nil
	}

	persistClaudeSessionID(config, sessName, hookData.SessionID)

	// idle_prompt means Claude is waiting for user input — clear typing indicator
	if hookData.NotificationType == "idle_prompt" {
		clearThinking(sessName)
		return nil
	}

	// Build notification message
	var msg string
	if hookData.Message != "" {
		msg = fmt.Sprintf("🔔 %s", hookData.Message)
	} else if hookData.Title != "" {
		msg = fmt.Sprintf("🔔 %s", hookData.Title)
	} else if hookData.NotificationType != "" {
		msg = fmt.Sprintf("🔔 %s", hookData.NotificationType)
	}

	if msg != "" {
		msgID := fmt.Sprintf("notif:%s:%d", hookData.SessionID, time.Now().UnixNano())
		appendMessage(&MessageRecord{
			ID:                msgID,
			Session:           sessName,
			Type:              "notification",
			Text:              msg,
			Origin:            "claude",
			TerminalDelivered: true,
			TelegramDelivered: false,
		})
		sendMessage(config, config.GroupID, topicID, msg)
		updateDelivery(sessName, msgID, "telegram_delivered", true)
	}

	return nil
}

// isCccHook checks if a hook entry contains a ccc command
func isCccHook(entry interface{}) bool {
	if m, ok := entry.(map[string]interface{}); ok {
		if cmd, ok := m["command"].(string); ok {
			return strings.Contains(cmd, "ccc hook")
		}
		if hooks, ok := m["hooks"].([]interface{}); ok {
			for _, h := range hooks {
				if hm, ok := h.(map[string]interface{}); ok {
					if cmd, ok := hm["command"].(string); ok {
						if strings.Contains(cmd, "ccc hook") {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

func removeCccHooks(hookArray []interface{}) []interface{} {
	var result []interface{}
	for _, entry := range hookArray {
		if !isCccHook(entry) {
			result = append(result, entry)
		}
	}
	return result
}

func installHook() error {
	home, _ := os.UserHomeDir()
	settingsPath := filepath.Join(home, ".claude", "settings.json")

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return fmt.Errorf("failed to read settings.json: %w", err)
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return fmt.Errorf("failed to parse settings.json: %w", err)
	}

	hooks, ok := settings["hooks"].(map[string]interface{})
	if !ok {
		hooks = make(map[string]interface{})
	}

	cccHooks := map[string][]interface{}{
		"PreToolUse": {
			map[string]interface{}{
				"hooks": []interface{}{
					map[string]interface{}{
						"command": cccPath + " hook-permission",
						"type":    "command",
						"timeout": 300000,
					},
				},
				"matcher": "",
			},
		},
		"Stop": {
			map[string]interface{}{
				"hooks": []interface{}{
					map[string]interface{}{
						"command": cccPath + " hook-stop",
						"type":    "command",
					},
				},
			},
		},
		"PostToolUse": {
			map[string]interface{}{
				"hooks": []interface{}{
					map[string]interface{}{
						"command": cccPath + " hook-post-tool",
						"type":    "command",
					},
				},
			},
		},
		"UserPromptSubmit": {
			map[string]interface{}{
				"hooks": []interface{}{
					map[string]interface{}{
						"command": cccPath + " hook-user-prompt",
						"type":    "command",
					},
				},
			},
		},
		"Notification": {
			map[string]interface{}{
				"hooks": []interface{}{
					map[string]interface{}{
						"command": cccPath + " hook-notification",
						"type":    "command",
					},
				},
			},
		},
	}

	// Remove ALL existing ccc hooks from all hook types
	allHookTypes := []string{"Stop", "Notification", "PermissionRequest", "PostToolUse", "PreToolUse", "UserPromptSubmit"}
	for _, hookType := range allHookTypes {
		if existing, ok := hooks[hookType].([]interface{}); ok {
			filtered := removeCccHooks(existing)
			if len(filtered) == 0 {
				delete(hooks, hookType)
			} else {
				hooks[hookType] = filtered
			}
		}
	}

	// Add only the hooks we need
	for hookType, newHooks := range cccHooks {
		var existingHooks []interface{}
		if existing, ok := hooks[hookType].([]interface{}); ok {
			existingHooks = existing
		}
		hooks[hookType] = append(newHooks, existingHooks...)
	}

	settings["hooks"] = hooks

	newData, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}

	if err := os.WriteFile(settingsPath, newData, 0600); err != nil {
		return fmt.Errorf("failed to write settings.json: %w", err)
	}

	fmt.Println("✅ Claude hooks installed!")
	return nil
}

func uninstallHook() error {
	home, _ := os.UserHomeDir()
	settingsPath := filepath.Join(home, ".claude", "settings.json")

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return fmt.Errorf("failed to read settings.json: %w", err)
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return fmt.Errorf("failed to parse settings.json: %w", err)
	}

	hooks, ok := settings["hooks"].(map[string]interface{})
	if !ok {
		fmt.Println("No hooks found")
		return nil
	}

	hookTypes := []string{"Stop", "Notification", "PermissionRequest", "PostToolUse", "PreToolUse", "UserPromptSubmit"}
	for _, hookType := range hookTypes {
		if existing, ok := hooks[hookType].([]interface{}); ok {
			filtered := removeCccHooks(existing)
			if len(filtered) == 0 {
				delete(hooks, hookType)
			} else {
				hooks[hookType] = filtered
			}
		}
	}

	settings["hooks"] = hooks

	newData, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}

	if err := os.WriteFile(settingsPath, newData, 0600); err != nil {
		return fmt.Errorf("failed to write settings.json: %w", err)
	}

	fmt.Println("✅ Claude hooks uninstalled!")
	return nil
}

func installSkill() error {
	home, _ := os.UserHomeDir()
	skillDir := filepath.Join(home, ".claude", "skills")
	skillPath := filepath.Join(skillDir, "ccc-send.md")

	if err := os.MkdirAll(skillDir, 0755); err != nil {
		return fmt.Errorf("failed to create skills directory: %w", err)
	}

	skillContent := `# CCC Send - File Transfer Skill

## Description
Send files to the user via Telegram using the ccc send command.

## Usage
When the user asks you to send them a file, or when you have generated/built a file that the user needs (like an APK, binary, or any other file), use this command:

` + "```bash" + `
ccc send <file_path>
` + "```" + `

## How it works
- **Small files (< 50MB)**: Sent directly via Telegram
- **Large files (≥ 50MB)**: Streamed via relay server with a one-time download link

## Examples

### Send a built APK
` + "```bash" + `
ccc send ./build/app.apk
` + "```" + `

### Send a generated file
` + "```bash" + `
ccc send ./output/report.pdf
` + "```" + `

### Send from subdirectory
` + "```bash" + `
ccc send ~/Downloads/large-file.zip
` + "```" + `

## Important Notes
- The command detects the current session from your working directory
- For large files, the command will wait up to 10 minutes for the user to download
- Each download link is one-time use only
- Use this proactively when you've created files the user needs!
`

	if err := os.WriteFile(skillPath, []byte(skillContent), 0644); err != nil {
		return fmt.Errorf("failed to write skill file: %w", err)
	}

	fmt.Println("✅ CCC send skill installed!")
	return nil
}

func uninstallSkill() error {
	home, _ := os.UserHomeDir()
	skillPath := filepath.Join(home, ".claude", "skills", "ccc-send.md")
	os.Remove(skillPath)
	return nil
}

// truncate shortens a string to n characters
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// hookLog writes debug log entries
func hookLog(format string, args ...interface{}) {
	f, err := os.OpenFile(filepath.Join(cacheDir(), "hook-debug.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[%s] %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}
