package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const maxResponseSize = 10 * 1024 * 1024 // 10MB

// telegramChunkLimit is the per-message budget ccc sends with. Telegram's hard
// cap is 4096; the slack absorbs the tags splitTelegramHTML has to reopen.
const telegramChunkLimit = 4000

// telegramBaseURL is the Bot API root. It is a variable so tests can point the
// whole Telegram surface at an httptest server; production never changes it.
var telegramBaseURL = "https://api.telegram.org"

// telegramURL builds a Bot API method URL.
func telegramURL(token, method string) string {
	return fmt.Sprintf("%s/bot%s/%s", telegramBaseURL, token, method)
}

// redactTokenError replaces the bot token in error messages with "***"
func redactTokenError(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), token, "***"))
}

// telegramGet performs an HTTP GET and redacts the bot token from any errors
func telegramGet(token string, url string) (*http.Response, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, redactTokenError(err, token)
	}
	return resp, nil
}

// telegramClientGet performs an HTTP GET with a custom client and redacts the bot token from any errors
func telegramClientGet(client *http.Client, token string, url string) (*http.Response, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, redactTokenError(err, token)
	}
	return resp, nil
}

// updateCCC downloads the latest ccc binary from GitHub releases and restarts
func updateCCC(config *Config, chatID, threadID int64, offset int) {
	sendMessage(config, chatID, threadID, "🔄 Updating ccc...")

	binaryName := fmt.Sprintf("ccc-%s-%s", runtime.GOOS, runtime.GOARCH)
	downloadURL := fmt.Sprintf("https://github.com/kidandcat/ccc/releases/latest/download/%s", binaryName)

	resp, err := http.Get(downloadURL)
	if err != nil {
		sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Download failed: %v", err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Download failed: HTTP %d (no release for %s?)", resp.StatusCode, binaryName))
		return
	}

	tmpPath := cccPath + ".new"
	f, err := os.Create(tmpPath)
	if err != nil {
		sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Failed to create temp file: %v", err))
		return
	}

	written, err := io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		os.Remove(tmpPath)
		sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Failed to write binary: %v", err))
		return
	}

	// Validate downloaded binary size (ccc should be > 1MB)
	if written < 1000000 {
		os.Remove(tmpPath)
		sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Downloaded file too small (%d bytes), aborting", written))
		return
	}

	if err := os.Chmod(tmpPath, 0755); err != nil {
		os.Remove(tmpPath)
		sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Failed to chmod: %v", err))
		return
	}

	// Test the new binary before replacing
	testCmd := exec.Command(tmpPath, "version")
	if err := testCmd.Run(); err != nil {
		os.Remove(tmpPath)
		sendMessage(config, chatID, threadID, fmt.Sprintf("❌ New binary failed validation: %v", err))
		return
	}

	// Backup old binary
	backupPath := cccPath + ".bak"
	os.Remove(backupPath) // Remove old backup if exists
	if err := os.Rename(cccPath, backupPath); err != nil {
		os.Remove(tmpPath)
		sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Failed to backup old binary: %v", err))
		return
	}

	// Replace with new binary
	if err := os.Rename(tmpPath, cccPath); err != nil {
		// Restore backup
		os.Rename(backupPath, cccPath)
		sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Failed to replace binary: %v", err))
		return
	}

	// Codesign on macOS
	if runtime.GOOS == "darwin" {
		if err := exec.Command("codesign", "-f", "-s", "-", cccPath).Run(); err != nil {
			// Restore backup if codesign fails
			os.Remove(cccPath)
			os.Rename(backupPath, cccPath)
			sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Codesign failed: %v", err))
			return
		}
	}

	// Success - remove backup
	os.Remove(backupPath)

	sendMessage(config, chatID, threadID, "✅ Updated. Restarting...")
	// Confirm offset so the /update message is not reprocessed after restart
	client := &http.Client{Timeout: 10 * time.Second}
	if resp, err := client.Get(fmt.Sprintf("%s?offset=%d&timeout=1", telegramURL(config.BotToken, "getUpdates"), offset)); err == nil {
		resp.Body.Close() // safe-ignore: this is the last call before os.Exit
	}
	os.Exit(0)
}

func telegramAPI(config *Config, method string, params url.Values) (*TelegramResponse, error) {
	apiURL := telegramURL(config.BotToken, method)
	resp, err := http.PostForm(apiURL, params)
	if err != nil {
		return nil, redactTokenError(err, config.BotToken)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	var result TelegramResponse
	json.Unmarshal(body, &result)
	return &result, nil
}

func sendMessage(config *Config, chatID int64, threadID int64, text string) error {
	_, err := sendMessageGetID(config, chatID, threadID, text)
	return err
}

// sendMessageGetID sends a message and returns the message ID for later editing
func sendMessageGetID(config *Config, chatID int64, threadID int64, text string) (int64, error) {
	return sendMessageWithMode(config, chatID, threadID, text, "Markdown")
}

// sendMessageHTMLGetID sends a message with HTML parse mode and returns the message ID
func sendMessageHTMLGetID(config *Config, chatID int64, threadID int64, text string) (int64, error) {
	return sendMessageWithMode(config, chatID, threadID, text, "HTML")
}

// sendMessageHTMLGetIDSilent posts HTML without a Telegram notification. Used
// for the in-progress "⏳ working" message so the owner is only pinged when
// the turn's final answer lands.
func sendMessageHTMLGetIDSilent(config *Config, chatID int64, threadID int64, text string) (int64, error) {
	return sendMessageOpts(config, chatID, threadID, text, "HTML", true)
}

func sendMessageWithMode(config *Config, chatID int64, threadID int64, text string, parseMode string) (int64, error) {
	return sendMessageOpts(config, chatID, threadID, text, parseMode, false)
}

func sendMessageOpts(config *Config, chatID int64, threadID int64, text string, parseMode string, silent bool) (int64, error) {
	messages := splitForMode(text, parseMode)
	var lastMsgID int64

	for _, msg := range messages {
		params := url.Values{
			"chat_id":    {fmt.Sprintf("%d", chatID)},
			"text":       {msg},
			"parse_mode": {parseMode},
		}
		if threadID > 0 {
			params.Set("message_thread_id", fmt.Sprintf("%d", threadID))
		}
		if silent {
			params.Set("disable_notification", "true")
		}

		result, err := telegramAPI(config, "sendMessage", params)
		if err != nil {
			return 0, err
		}
		if !result.OK {
			// Telegram rejects the whole message when the markup does not
			// parse, so retry once as plain text rather than lose it.
			if isParseEntitiesError(result.Description) && parseMode != "" {
				log.Printf("telegram sendMessage rejected the %s markup (%s); retrying as plain text", parseMode, result.Description)
				params.Del("parse_mode")
				params.Set("text", plainFallbackText(msg, parseMode))
				result, err = telegramAPI(config, "sendMessage", params)
				if err != nil {
					return 0, err
				}
				if !result.OK {
					log.Printf("telegram sendMessage plain-text retry also failed: %s", result.Description)
					return 0, fmt.Errorf("telegram error: %s", result.Description)
				}
			} else {
				return 0, fmt.Errorf("telegram error: %s", result.Description)
			}
		}

		// Extract message_id from result
		if len(result.Result) > 0 {
			var msgResult struct {
				MessageID int64 `json:"message_id"`
			}
			if json.Unmarshal(result.Result, &msgResult) == nil {
				lastMsgID = msgResult.MessageID
			}
		}

		// Small delay between messages to maintain order
		if len(messages) > 1 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	return lastMsgID, nil
}

// editMessage edits an existing message, sending overflow as new messages
func editMessage(config *Config, chatID int64, messageID int64, threadID int64, text string) error {
	return editMessageWithMode(config, chatID, messageID, threadID, text, "Markdown")
}

// editMessageHTML edits a message using HTML parse mode
func editMessageHTML(config *Config, chatID int64, messageID int64, threadID int64, text string) error {
	return editMessageWithMode(config, chatID, messageID, threadID, text, "HTML")
}

func editMessageWithMode(config *Config, chatID int64, messageID int64, threadID int64, text string, parseMode string) error {
	// Split message - first part goes to edit, rest as new messages
	messages := splitForMode(text, parseMode)

	// Edit existing message with first part
	params := url.Values{
		"chat_id":    {fmt.Sprintf("%d", chatID)},
		"message_id": {fmt.Sprintf("%d", messageID)},
		"text":       {messages[0]},
		"parse_mode": {parseMode},
	}

	result, err := telegramAPI(config, "editMessageText", params)
	if err != nil {
		return err
	}
	if !result.OK {
		switch {
		case strings.Contains(result.Description, "message is not modified"):
			// Nothing changed; the message on screen is already correct.
		case isParseEntitiesError(result.Description) && parseMode != "":
			log.Printf("telegram editMessageText rejected the %s markup (%s); retrying as plain text", parseMode, result.Description)
			params.Del("parse_mode")
			params.Set("text", plainFallbackText(messages[0], parseMode))
			result, err = telegramAPI(config, "editMessageText", params)
			if err != nil {
				return err
			}
			if !result.OK && !strings.Contains(result.Description, "message is not modified") {
				log.Printf("telegram editMessageText plain-text retry also failed: %s", result.Description)
				return fmt.Errorf("telegram error: %s", result.Description)
			}
		default:
			return fmt.Errorf("telegram error: %s", result.Description)
		}
	}

	// Send remaining parts as new messages, in the same mode the edit used.
	for i := 1; i < len(messages); i++ {
		time.Sleep(100 * time.Millisecond)
		if _, err := sendMessageWithMode(config, chatID, threadID, messages[i], parseMode); err != nil {
			log.Printf("telegram overflow message %d/%d failed: %v", i+1, len(messages), err)
		}
	}

	return nil
}

// splitForMode chunks a message the way its parse mode needs: HTML is split
// tag-aware so no chunk is left with a half tag, everything else by text.
func splitForMode(text, parseMode string) []string {
	if strings.EqualFold(parseMode, "HTML") {
		return splitTelegramHTML(text, telegramChunkLimit)
	}
	return splitMessage(text, telegramChunkLimit)
}

// isParseEntitiesError matches the Telegram 400 raised when the markup in a
// message does not parse ("Bad Request: can't parse entities: ...").
func isParseEntitiesError(description string) bool {
	return strings.Contains(description, "parse entities")
}

// plainFallbackText is what to send when the markup was rejected: the text
// with its markup removed, so nothing of the reply is lost.
func plainFallbackText(msg, parseMode string) string {
	if strings.EqualFold(parseMode, "HTML") {
		return plainTextFromHTML(msg)
	}
	return msg
}

// deleteMessage removes a message; used to retire a progress message that
// could not be turned into the final reply.
func deleteMessage(config *Config, chatID int64, messageID int64) error {
	result, err := telegramAPI(config, "deleteMessage", url.Values{
		"chat_id":    {fmt.Sprintf("%d", chatID)},
		"message_id": {fmt.Sprintf("%d", messageID)},
	})
	if err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("telegram error: %s", result.Description)
	}
	return nil
}

func sendMessageWithKeyboard(config *Config, chatID int64, threadID int64, text string, buttons [][]InlineKeyboardButton) error {
	const maxLen = 4000

	// Split long messages - send all but last as regular messages, last with keyboard
	messages := splitMessage(text, maxLen)

	// Send all but the last message as regular messages
	for i := 0; i < len(messages)-1; i++ {
		sendMessage(config, chatID, threadID, messages[i])
		time.Sleep(100 * time.Millisecond)
	}

	// Send the last message with keyboard
	keyboard := map[string]interface{}{
		"inline_keyboard": buttons,
	}
	keyboardJSON, _ := json.Marshal(keyboard)

	params := url.Values{
		"chat_id":      {fmt.Sprintf("%d", chatID)},
		"text":         {messages[len(messages)-1]},
		"reply_markup": {string(keyboardJSON)},
	}
	if threadID > 0 {
		params.Set("message_thread_id", fmt.Sprintf("%d", threadID))
	}

	result, err := telegramAPI(config, "sendMessage", params)
	if err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("telegram error: %s", result.Description)
	}
	return nil
}

func answerCallbackQuery(config *Config, callbackID string) {
	params := url.Values{
		"callback_query_id": {callbackID},
	}
	telegramAPI(config, "answerCallbackQuery", params)
}

func editMessageRemoveKeyboard(config *Config, chatID int64, messageID int, newText string) {
	const maxLen = 4000
	if len(newText) > maxLen {
		newText = newText[:maxLen-3] + "..."
	}

	params := url.Values{
		"chat_id":    {fmt.Sprintf("%d", chatID)},
		"message_id": {fmt.Sprintf("%d", messageID)},
		"text":       {newText},
	}
	telegramAPI(config, "editMessageText", params)
}

func sendTypingAction(config *Config, chatID int64, threadID int64) {
	params := url.Values{
		"chat_id": {fmt.Sprintf("%d", chatID)},
		"action":  {"typing"},
	}
	if threadID > 0 {
		params.Set("message_thread_id", fmt.Sprintf("%d", threadID))
	}
	telegramAPI(config, "sendChatAction", params)
}

// topicDeleted probes whether a forum topic still exists. It calls
// reopenForumTopic, which is a no-op on an already-open topic (returns
// TOPIC_NOT_MODIFIED) but returns TOPIC_ID_INVALID once the topic is deleted —
// so it detects deletion with no visible side effect. Used to retire the
// session behind a deleted topic.
func topicDeleted(config *Config, topicID int64) bool {
	params := url.Values{
		"chat_id":           {fmt.Sprintf("%d", config.GroupID)},
		"message_thread_id": {fmt.Sprintf("%d", topicID)},
	}
	resp, err := telegramAPI(config, "reopenForumTopic", params)
	if err != nil || resp == nil || resp.OK {
		return false
	}
	return strings.Contains(resp.Description, "TOPIC_ID_INVALID")
}

func splitMessage(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}

	var messages []string
	remaining := text

	for len(remaining) > 0 {
		if len(remaining) <= maxLen {
			messages = append(messages, remaining)
			break
		}

		// Find a good split point (newline or space)
		splitAt := maxLen

		// Try to split at a newline first
		if idx := strings.LastIndex(remaining[:maxLen], "\n"); idx > maxLen/2 {
			splitAt = idx + 1
		} else if idx := strings.LastIndex(remaining[:maxLen], " "); idx > maxLen/2 {
			// Fall back to space
			splitAt = idx + 1
		}

		messages = append(messages, strings.TrimRight(remaining[:splitAt], " \n"))
		remaining = remaining[splitAt:]
	}

	return messages
}

// sendFile sends a file to Telegram (max 50MB)
func sendFile(config *Config, chatID int64, threadID int64, filePath string, caption string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	// Add chat_id
	writer.WriteField("chat_id", fmt.Sprintf("%d", chatID))
	if threadID > 0 {
		writer.WriteField("message_thread_id", fmt.Sprintf("%d", threadID))
	}
	if caption != "" {
		writer.WriteField("caption", caption)
	}

	// Add file
	part, err := writer.CreateFormFile("document", filepath.Base(filePath))
	if err != nil {
		return err
	}
	io.Copy(part, file)
	writer.Close()

	resp, err := http.Post(
		telegramURL(config.BotToken, "sendDocument"),
		writer.FormDataContentType(),
		body,
	)
	if err != nil {
		return redactTokenError(err, config.BotToken)
	}
	defer resp.Body.Close()

	var result TelegramResponse
	json.NewDecoder(resp.Body).Decode(&result)
	if !result.OK {
		return fmt.Errorf("telegram error: %s", result.Description)
	}
	return nil
}

// downloadTelegramFile downloads a file from Telegram
func downloadTelegramFile(config *Config, fileID string, destPath string) error {
	// Get file path from Telegram
	resp, err := telegramGet(config.BotToken, fmt.Sprintf("%s?file_id=%s", telegramURL(config.BotToken, "getFile"), url.QueryEscape(fileID)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			FilePath string `json:"file_path"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("failed to get file path")
	}

	// Download the file
	fileURL := fmt.Sprintf("%s/file/bot%s/%s", telegramBaseURL, config.BotToken, result.Result.FilePath)
	fileResp, err := telegramGet(config.BotToken, fileURL)
	if err != nil {
		return err
	}
	defer fileResp.Body.Close()

	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, fileResp.Body)
	return err
}

func createForumTopic(config *Config, name string) (int64, error) {
	if config.GroupID == 0 {
		return 0, fmt.Errorf("no group configured. Add bot to a group with topics enabled and run: ccc setgroup")
	}

	params := url.Values{
		"chat_id": {fmt.Sprintf("%d", config.GroupID)},
		"name":    {name},
	}

	result, err := telegramAPI(config, "createForumTopic", params)
	if err != nil {
		return 0, err
	}
	if !result.OK {
		return 0, fmt.Errorf("failed to create topic: %s", result.Description)
	}

	var topic TopicResult
	if err := json.Unmarshal(result.Result, &topic); err != nil {
		return 0, fmt.Errorf("failed to parse topic result: %w", err)
	}

	return topic.MessageThreadID, nil
}

// TopicIconSticker is one entry of getForumTopicIconStickers: an emoji and the
// custom-emoji id editForumTopic accepts for it. Topic icons are NOT arbitrary
// emoji — only ids from this list work — and the set is free for bots.
type TopicIconSticker struct {
	CustomEmojiID string `json:"custom_emoji_id"`
	Emoji         string `json:"emoji"`
}

// fetchForumTopicIcons asks Telegram for the emoji allowed as topic icons.
// Callers go through topicIcons (store.go), which caches the answer.
func fetchForumTopicIcons(config *Config) ([]TopicIconSticker, error) {
	result, err := telegramAPI(config, "getForumTopicIconStickers", url.Values{})
	if err != nil {
		return nil, err
	}
	if !result.OK {
		return nil, fmt.Errorf("getForumTopicIconStickers: %s", result.Description)
	}
	var stickers []TopicIconSticker
	if err := json.Unmarshal(result.Result, &stickers); err != nil {
		return nil, fmt.Errorf("failed to parse icon stickers: %w", err)
	}
	return stickers, nil
}

// editForumTopic renames an existing forum topic and/or changes its icon. An
// empty name or icon id is omitted from the request, which is how the Bot API
// spells "keep the current one".
func editForumTopic(config *Config, topicID int64, name, iconCustomEmojiID string) error {
	if config.GroupID == 0 {
		return fmt.Errorf("no group configured")
	}

	params := url.Values{
		"chat_id":           {fmt.Sprintf("%d", config.GroupID)},
		"message_thread_id": {fmt.Sprintf("%d", topicID)},
	}
	if name != "" {
		params.Set("name", name)
	}
	if iconCustomEmojiID != "" {
		params.Set("icon_custom_emoji_id", iconCustomEmojiID)
	}
	if len(params) == 2 {
		return nil // nothing to change
	}

	result, err := telegramAPI(config, "editForumTopic", params)
	if err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("failed to edit topic: %s", result.Description)
	}

	return nil
}

func deleteForumTopic(config *Config, topicID int64) error {
	if config.GroupID == 0 {
		return fmt.Errorf("no group configured")
	}

	params := url.Values{
		"chat_id":           {fmt.Sprintf("%d", config.GroupID)},
		"message_thread_id": {fmt.Sprintf("%d", topicID)},
	}

	result, err := telegramAPI(config, "deleteForumTopic", params)
	if err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("failed to delete topic: %s", result.Description)
	}

	return nil
}

// setBotCommands sets the bot commands in Telegram
func setBotCommands(botToken string) {
	commands := []map[string]string{
		{"command": "new", "description": "New session: /new <prompt> (or reset in a topic)"},
		{"command": "stop", "description": "Stop this session's agent (keeps conversation)"},
		{"command": "delete", "description": "Delete current session and thread"},
		{"command": "cleanup", "description": "Delete ALL sessions and threads"},
		{"command": "list", "description": "List sessions and their status"},
		{"command": "profiles", "description": "List Claude accounts (profiles) and usage"},
		{"command": "profile", "description": "Pin this topic to a profile: /profile <name>"},
		{"command": "c", "description": "Execute shell command: /c <cmd>"},
		{"command": "update", "description": "Update ccc binary from GitHub"},
		{"command": "version", "description": "Show ccc version"},
		{"command": "stats", "description": "Show system stats (RAM, disk, etc)"},
	}

	// Set for default scope
	defaultBody, _ := json.Marshal(map[string]interface{}{
		"commands": commands,
	})
	resp, err := http.Post(
		telegramURL(botToken, "setMyCommands"),
		"application/json",
		bytes.NewReader(defaultBody),
	)
	if err == nil {
		resp.Body.Close()
	}

	// Set for all group chats (makes the / button appear)
	groupBody, _ := json.Marshal(map[string]interface{}{
		"commands": commands,
		"scope":    map[string]string{"type": "all_group_chats"},
	})
	resp, err = http.Post(
		telegramURL(botToken, "setMyCommands"),
		"application/json",
		bytes.NewReader(groupBody),
	)
	if err == nil {
		resp.Body.Close()
	}
}

// setMessageReaction puts a single emoji reaction on a message. ccc uses it to
// tick (✅) the message that triggered a turn once the turn completes
// (DESIGN §3.2). Reactions are cosmetic: failures are logged, never surfaced.
func setMessageReaction(config *Config, chatID int64, messageID int64, emoji string) error {
	reaction, err := json.Marshal([]map[string]string{{"type": "emoji", "emoji": emoji}})
	if err != nil {
		return err
	}
	params := url.Values{
		"chat_id":    {fmt.Sprintf("%d", chatID)},
		"message_id": {fmt.Sprintf("%d", messageID)},
		"reaction":   {string(reaction)},
	}
	result, err := telegramAPI(config, "setMessageReaction", params)
	if err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("telegram error: %s", result.Description)
	}
	return nil
}

// sendMessageKeyboardGetID sends one HTML message with an inline keyboard and
// returns its message id, which ask_owner needs to match a tap to a question.
func sendMessageKeyboardGetID(config *Config, chatID int64, threadID int64, text string, buttons [][]InlineKeyboardButton) (int64, error) {
	keyboardJSON, err := json.Marshal(map[string]any{"inline_keyboard": buttons})
	if err != nil {
		return 0, err
	}
	params := url.Values{
		"chat_id":      {fmt.Sprintf("%d", chatID)},
		"text":         {truncate(text, 4000)},
		"parse_mode":   {"HTML"},
		"reply_markup": {string(keyboardJSON)},
	}
	if threadID > 0 {
		params.Set("message_thread_id", fmt.Sprintf("%d", threadID))
	}
	result, err := telegramAPI(config, "sendMessage", params)
	if err != nil {
		return 0, err
	}
	if !result.OK {
		return 0, fmt.Errorf("telegram error: %s", result.Description)
	}
	var msg struct {
		MessageID int64 `json:"message_id"`
	}
	if err := json.Unmarshal(result.Result, &msg); err != nil {
		return 0, nil // safe-ignore: the message went out; only its id is unknown
	}
	return msg.MessageID, nil
}

// closeForumTopic closes a topic without deleting it, which is what archiving a
// bot does: the conversation stays readable, but nothing new lands in it.
func closeForumTopic(config *Config, topicID int64) error {
	if config == nil || config.BotToken == "" || config.GroupID == 0 || topicID == 0 {
		return nil
	}
	params := url.Values{}
	params.Set("chat_id", fmt.Sprintf("%d", config.GroupID))
	params.Set("message_thread_id", fmt.Sprintf("%d", topicID))
	resp, err := telegramAPI(config, "closeForumTopic", params)
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("closeForumTopic: %s", resp.Description)
	}
	return nil
}
