package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
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

const probeInterval = 15 * time.Second

// reapAfterTicks is how many consecutive polls a started session must be absent
// from the fleet before its topic is deleted (grace to ride out resume/respawn
// transients). At mirrorInterval=3s this is ~9s.
const reapAfterTicks = 3

// resumeGraceSeconds is how long a session's ResumingAt is treated as "still
// resuming" (agent legitimately absent from the fleet). A resume (stop + up to
// 8s wait + dispatch) outlasts reapAfterTicks, so without this the reaper could
// delete a live topic mid-resume. A stale marker (crashed resume) expires here.
const resumeGraceSeconds = 45

var (
	mirrorMu     sync.Mutex
	lastStatus   = map[int64]string{}    // topicID -> last posted status
	lastProbe    = map[int64]time.Time{} // topicID -> last deletion probe
	missingTicks = map[int64]int{}       // topicID -> consecutive polls absent from fleet
)

// runMirror is the poller loop, launched as a goroutine from listen().
func runMirror() {
	for {
		time.Sleep(mirrorInterval)
		config, err := loadConfig()
		if err != nil || config == nil || config.GroupID == 0 {
			continue
		}
		// Full snapshot: includes done/idle-resident sessions (what the agents
		// view shows), excludes only killed ones — used for both discovery and
		// mirroring.
		agents, err := listAgents(true)
		if err != nil {
			continue
		}
		if c := dedupSessions(); c != nil {
			config = c
		}
		if c := discoverFleet(agents); c != nil {
			config = c
		}
		for sessName, info := range config.Sessions {
			if info == nil || info.TopicID == 0 {
				continue
			}
			mirrorSession(config, agents, sessName, info)
		}
	}
}

// dedupSessions removes duplicate topics that map to the same conversation
// (e.g. a /new session and a discovery topic racing for the same agent),
// keeping the earliest-created topic and reaping the rest.
func dedupSessions() *Config {
	return updateConfig(func(config *Config) bool {
		bySid := map[string][]string{}
		for name, info := range config.Sessions {
			if info != nil && info.SessionID != "" {
				bySid[info.SessionID] = append(bySid[info.SessionID], name)
			}
		}
		changed := false
		for _, names := range bySid {
			if len(names) < 2 {
				continue
			}
			keep := names[0]
			for _, n := range names {
				if config.Sessions[n].TopicID < config.Sessions[keep].TopicID {
					keep = n
				}
			}
			for _, n := range names {
				if n == keep {
					continue
				}
				info := config.Sessions[n]
				deleteForumTopic(config, info.TopicID)
				mirrorMu.Lock()
				delete(lastStatus, info.TopicID)
				delete(lastProbe, info.TopicID)
				delete(missingTicks, info.TopicID)
				mirrorMu.Unlock()
				delete(config.Sessions, n)
				changed = true
				hookLog("deduped session %s (dup of %s) — deleted topic %d", n, keep, info.TopicID)
			}
		}
		return changed
	})
}

// discoverFleet auto-creates a Telegram topic for every active bg agent that
// isn't already mapped to one, so sessions started anywhere (the PC, the agents
// view) show up in ccc. Keyed by the conversation UUID; `claude attach` keeps
// the UUID stable, and ccc updates the mapping on its own resumes. Returns true
// if it created any topic (config was saved).
func discoverFleet(live []AgentInfo) *Config {
	return updateConfig(func(config *Config) bool {
		mapped := map[string]bool{}      // conversation UUID -> tracked
		mappedShort := map[string]bool{} // short id -> tracked (short-id handoff)
		pendingCwd := map[string]bool{}  // cwd of /new sessions awaiting their agent
		byTopic := map[int64]string{}    // TopicID -> session name (for marker adoption)
		for name, info := range config.Sessions {
			if info == nil {
				continue
			}
			byTopic[info.TopicID] = name
			if info.SessionID != "" {
				mapped[info.SessionID] = true
			} else if info.Path != "" {
				pendingCwd[info.Path] = true
			}
			// A stopped parent conversation lingers as a pid-less "done" fleet
			// entry after a resume; never let it earn its own discovery topic.
			for _, old := range info.OldSessionIDs {
				mapped[old] = true
			}
			if info.ShortID != "" {
				mappedShort[info.ShortID] = true
			}
		}
		changed := false
		for i := range live {
			a := live[i]
			if a.SessionID == "" || mapped[a.SessionID] || mappedShort[a.ID] {
				continue
			}
			// A pending /new session in this cwd will claim this agent — don't
			// race it with a duplicate discovery topic.
			if pendingCwd[a.Cwd] {
				continue
			}
			// Skip killed sessions (stopped/failed) — the agents view drops them.
			// Active and done/idle-resident sessions are what we mirror.
			st := strings.ToLower(a.State)
			if st == "stopped" || st == "failed" || st == "error" {
				continue
			}
			// A resume (by ccc OR the PC agents view) mints a new UUID/short with no
			// server-side link to the parent. If this agent's conversation carries a
			// ccc marker for a topic we already track, it's that session resumed —
			// adopt it into the existing topic instead of spawning a duplicate.
			if topicID := transcriptTopicMarker(transcriptPathForUUID(a.SessionID)); topicID != 0 {
				if name, ok := byTopic[topicID]; ok {
					info := config.Sessions[name]
					carrySessionLabel(info.SessionID, a.SessionID)
					appendOldSessionID(info, info.SessionID)
					info.SessionID = a.SessionID
					info.ShortID = a.ID
					info.Marked = true
					mapped[a.SessionID] = true
					mappedShort[a.ID] = true
					changed = true
					hookLog("adopted resumed agent %s into session %s (marker t%d)", a.ID, name, topicID)
					continue
				}
			}
			title := topicTitleFor(&a)
			topicID, err := createForumTopic(config, title)
			if err != nil {
				continue
			}
			key := uniqueSessionKey(config, &a)
			config.Sessions[key] = &SessionInfo{
				TopicID:   topicID,
				Path:      a.Cwd,
				SessionID: a.SessionID,
				ShortID:   a.ID,
				Title:     title,
			}
			// Mark the session's existing transcript as already delivered so we
			// don't back-spam its history — only new text after discovery is sent.
			seedDelivered(key, a.SessionID)
			mapped[a.SessionID] = true
			changed = true
			hookLog("discovered agent %s (%s) → topic %d %q", a.ID, a.SessionID, topicID, title)
			sendMessage(config, config.GroupID, topicID,
				"🔭 Discovered from the agents view — send a message here to talk to this session.")
		}
		return changed
	})
}

// reapSession deletes the Telegram topic for a session that has disappeared
// from the fleet (dismissed in the agents view or ended), and drops it from the
// map — the mirror image of retireSession.
//
// It is race-proof: the tick decided to reap off a stale snapshot, so we re-check
// under the config lock that the entry still exists AND still carries the exact
// SessionID/ShortID we decided to reap, and that a resume didn't start in the
// meantime (fresh ResumingAt). If any of that changed the reap is aborted
// silently. Only when the entry is actually removed do we delete the topic.
func reapSession(sessName, expectSessionID, expectShortID string, topicID int64) {
	removed := false
	config := updateConfig(func(config *Config) bool {
		info := config.Sessions[sessName]
		if info == nil || info.SessionID != expectSessionID || info.ShortID != expectShortID {
			return false
		}
		if info.ResumingAt != 0 && time.Now().Unix()-info.ResumingAt < resumeGraceSeconds {
			return false
		}
		delete(config.Sessions, sessName)
		removed = true
		return true
	})
	if !removed || config == nil {
		return
	}
	deleteForumTopic(config, topicID)
	mirrorMu.Lock()
	delete(lastStatus, topicID)
	delete(lastProbe, topicID)
	delete(missingTicks, topicID)
	mirrorMu.Unlock()
	hookLog("reaped session %s (gone from fleet) — deleted topic %d", sessName, topicID)
}

// retireSession stops a session's agent and removes it from the map, in
// response to its Telegram topic being deleted.
func retireSession(config *Config, sessName string, info *SessionInfo, agents []AgentInfo) {
	if short := liveShortID(config, sessName, agents); short != "" {
		stopAgent(short)
	}
	updateConfig(func(c *Config) bool {
		if _, ok := c.Sessions[sessName]; !ok {
			return false
		}
		delete(c.Sessions, sessName)
		return true
	})
	mirrorMu.Lock()
	delete(lastStatus, info.TopicID)
	delete(lastProbe, info.TopicID)
	mirrorMu.Unlock()
	hookLog("retired session %s (topic %d deleted)", sessName, info.TopicID)
}

// seedDelivered records a session's current assistant text as already
// delivered, so discovering a long-running session doesn't back-spam its
// history to Telegram.
func seedDelivered(sessName, sessionID string) {
	tp := transcriptPathForUUID(sessionID)
	if tp == "" {
		return
	}
	for _, b := range extractRecentAssistantTexts(tp, 200) {
		id := fmt.Sprintf("reply:%s:%s", b.requestID, contentHash(b.text))
		if isDelivered(sessName, id, "telegram") {
			continue
		}
		appendMessage(&MessageRecord{
			ID: id, Session: sessName, Type: "assistant_text",
			Text: "(pre-existing)", Origin: "claude", TelegramDelivered: true,
		})
	}
}

// titlePathPrefixRe matches a leading "<path>: " prefix where the path is
// shaped like a filesystem location (~, ~/…, /…, .../…). The user's fleet-view
// hook prepends "<dir>: " to the display name to locate a session on a crowded
// PC screen, but as a Telegram topic title (one session per topic) it's noise.
// Only path-shaped prefixes are stripped, so plain names like "fix: swipe
// decoder" pass through unchanged.
var titlePathPrefixRe = regexp.MustCompile(`^(?:~|~/\S*|/\S*|\.\.\./\S*): `)

// stripTitlePathPrefix removes a leading path-shaped "<dir>: " prefix (see
// titlePathPrefixRe).
func stripTitlePathPrefix(name string) string {
	return titlePathPrefixRe.ReplaceAllString(name, "")
}

// topicTitleFor builds a human-readable Telegram topic title for an agent. It
// prefers the user's session label (which survives resumes via carrySessionLabel);
// otherwise the fleet display name with its "<dir>: " prefix stripped; otherwise
// the cwd's basename; otherwise the short id.
func topicTitleFor(a *AgentInfo) string {
	name := sessionLabel(a.SessionID)
	if name == "" {
		name = stripTitlePathPrefix(strings.TrimSpace(a.Name))
	}
	if name == "" {
		name = filepath.Base(a.Cwd)
		if name == "" || name == "." {
			name = a.ID
		}
	}
	if len(name) > 120 {
		name = name[:120]
	}
	return name
}

// syncTopicTitle renames the Telegram topic when the agent renames itself in
// the fleet view. `/new` topics are born titled after their initial prompt;
// once Claude picks a proper session name, the topic follows it.
func syncTopicTitle(config *Config, info *SessionInfo, a *AgentInfo) {
	name := topicTitleFor(a)
	if name == "" || name == info.Title {
		return
	}
	// TOPIC_NOT_MODIFIED = the topic already carries this name (e.g. it was
	// created from it): record it so we stop re-issuing the edit every tick.
	if err := editForumTopic(config, info.TopicID, name); err != nil && !strings.Contains(err.Error(), "TOPIC_NOT_MODIFIED") {
		return
	}
	topicID := info.TopicID
	updateConfig(func(c *Config) bool {
		for _, si := range c.Sessions {
			if si != nil && si.TopicID == topicID {
				si.Title = name
				return true
			}
		}
		return false
	})
	info.Title = name
}

// uniqueSessionKey returns a config-map key that doesn't collide with an
// existing session name.
func uniqueSessionKey(config *Config, a *AgentInfo) string {
	base := filepath.Base(a.Cwd)
	if base == "" || base == "." || base == "/" {
		base = "session"
	}
	key := base
	if _, exists := config.Sessions[key]; exists {
		key = base + "-" + a.ID
	}
	return key
}

func mirrorSession(config *Config, agents []AgentInfo, sessName string, info *SessionInfo) {
	defer func() { recover() }()

	// If the topic was deleted from Telegram, retire the session (stop its
	// agent — like Ctrl+X in the agents view) and drop it from the map.
	mirrorMu.Lock()
	due := time.Since(lastProbe[info.TopicID]) > probeInterval
	if due {
		lastProbe[info.TopicID] = time.Now()
	}
	mirrorMu.Unlock()
	if due && topicDeleted(config, info.TopicID) {
		retireSession(config, sessName, info, agents)
		return
	}

	a, live := agentBySessionID(agents, info.SessionID)
	// Short-id handoff: right after a resume the tracked UUID is stale (the new
	// agent has a fresh UUID), but its short id is already persisted. Match by
	// short id and adopt the fresh UUID so we neither reap the topic nor let
	// discovery spawn a duplicate for the same session.
	if !live && info.ShortID != "" {
		if a2, ok := agentByID(agents, info.ShortID); ok {
			a, live = a2, true
		}
	}
	if live && a.SessionID != "" && a.SessionID != info.SessionID {
		newUUID := a.SessionID
		newShort := a.ID
		topicID := info.TopicID
		carrySessionLabel(info.SessionID, newUUID)
		updateConfig(func(c *Config) bool {
			for _, si := range c.Sessions {
				if si != nil && si.TopicID == topicID {
					appendOldSessionID(si, si.SessionID)
					si.SessionID = newUUID
					si.ShortID = newShort
					return true
				}
			}
			return false
		})
		info.SessionID = newUUID
		info.ShortID = newShort
	}

	// Reap: a session that was started (has a UUID) but has vanished from the
	// fleet was dismissed in the agents view (Ctrl+X) or ended — delete its
	// topic so ccc mirrors the agents view. Grace period avoids reaping during
	// a resume/respawn (when the agent briefly disappears).
	if info.SessionID != "" && !live {
		// Mid-resume: the agent is legitimately absent while it stops+relaunches.
		// Don't accrue missing ticks (and reset any accrued) until the resume
		// marker goes stale.
		if info.ResumingAt != 0 && time.Now().Unix()-info.ResumingAt < resumeGraceSeconds {
			mirrorMu.Lock()
			delete(missingTicks, info.TopicID)
			mirrorMu.Unlock()
			return
		}
		mirrorMu.Lock()
		missingTicks[info.TopicID]++
		gone := missingTicks[info.TopicID] >= reapAfterTicks
		mirrorMu.Unlock()
		if gone {
			reapSession(sessName, info.SessionID, info.ShortID, info.TopicID)
		}
		return
	}
	mirrorMu.Lock()
	delete(missingTicks, info.TopicID)
	mirrorMu.Unlock()

	if live {
		syncTopicTitle(config, info, a)
	}

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
		// No session-name prefix: each session has its own topic, so the label
		// is redundant (and reads as if the message came from the user).
		tgID, err := sendMessageGetID(config, config.GroupID, topicID, b.text)
		if err != nil {
			time.Sleep(400 * time.Millisecond)
			tgID, _ = sendMessageGetID(config, config.GroupID, topicID, b.text)
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
