package main

import (
	"fmt"
	"sync"
	"time"
)

// The mirror poller is the pull-based half of ccc. It periodically snapshots
// the fleet (`claude agents --json --all`) and for each tracked session:
//   - delivers new assistant text to the session's Telegram topic
//   - reports state transitions (working / needs input / failed) + typing
//
// AskUserQuestion prompts are handled separately by a PreToolUse hook (see
// hooks.go), because the question options are not written to the transcript
// while the agent is blocked waiting for an answer.

const mirrorInterval = 3 * time.Second

var (
	mirrorMu   sync.Mutex
	lastStatus = map[int64]string{} // topicID -> last posted status
)

// runMirror is the poller loop, launched as a goroutine from listen().
func runMirror() {
	for {
		time.Sleep(mirrorInterval)
		config, err := loadConfig()
		if err != nil || config == nil || config.GroupID == 0 {
			continue
		}
		agents, err := listAgents(true)
		if err != nil {
			continue
		}
		for sessName, info := range config.Sessions {
			if info == nil || info.TopicID == 0 {
				continue
			}
			mirrorSession(config, agents, sessName, info)
		}
	}
}

func mirrorSession(config *Config, agents []AgentInfo, sessName string, info *SessionInfo) {
	defer func() { recover() }()

	a, live := agentBySessionID(agents, info.SessionID)
	short := info.ShortID
	if live {
		short = a.ID
	}

	// Deliver any new assistant text from the transcript.
	transcript := transcriptPathForUUID(info.SessionID)
	js := readJobState(short)
	if transcript == "" && js != nil {
		transcript = js.LinkScanPath
	}
	if transcript != "" {
		deliverText(config, sessName, info.TopicID, transcript)
	}

	// Coarse status.
	status := "unknown"
	if live {
		status = classifyState(a.State, a.Status)
	} else if js != nil {
		status = classifyState(js.State, "")
	}

	postStatus(config, sessName, info.TopicID, info.SessionID, status, js)
	if status == "working" {
		sendTypingAction(config, config.GroupID, info.TopicID)
	}
}

// deliverText sends assistant text blocks not yet delivered to Telegram.
func deliverText(config *Config, sessName string, topicID int64, transcript string) {
	blocks := extractRecentAssistantTexts(transcript, 80)
	for _, b := range blocks {
		id := fmt.Sprintf("reply:%s:%s", b.requestID, contentHash(b.text))
		if isDelivered(sessName, id, "telegram") {
			continue
		}
		msg := fmt.Sprintf("*%s:*\n%s", sessName, b.text)
		tgID, err := sendMessageGetID(config, config.GroupID, topicID, msg)
		if err != nil {
			time.Sleep(400 * time.Millisecond)
			tgID, _ = sendMessageGetID(config, config.GroupID, topicID, msg)
		}
		appendMessage(&MessageRecord{
			ID: id, Session: sessName, Type: "assistant_text",
			Text: truncate(b.text, 500), Origin: "claude",
			TerminalDelivered: true, TelegramDelivered: tgID > 0, TelegramMsgID: tgID,
		})
	}
}

// postStatus emits a message when a session's status changes meaningfully.
func postStatus(config *Config, sessName string, topicID int64, sessionID, status string, js *jobState) {
	mirrorMu.Lock()
	prev := lastStatus[topicID]
	if status == prev || status == "unknown" {
		mirrorMu.Unlock()
		return
	}
	lastStatus[topicID] = status
	mirrorMu.Unlock()

	switch status {
	case "needs_input":
		// If the AskUserQuestion hook is handling this block, it already posted
		// the option buttons — don't double-notify.
		if questionPending(shortOf(sessionID)) {
			return
		}
		sendMessage(config, config.GroupID, topicID, fmt.Sprintf("⏸️ *%s* needs your input%s", sessName, detailSuffix(js)))
	case "failed":
		sendMessage(config, config.GroupID, topicID, fmt.Sprintf("❌ *%s* failed%s", sessName, detailSuffix(js)))
	}
}

func detailSuffix(js *jobState) string {
	if js != nil && js.Detail != "" && js.Detail != "stopped" {
		return "\n" + js.Detail
	}
	return ""
}

// shortOf returns the 8-char short id derived from a conversation UUID.
func shortOf(sessionID string) string {
	if len(sessionID) >= 8 {
		return sessionID[:8]
	}
	return sessionID
}
