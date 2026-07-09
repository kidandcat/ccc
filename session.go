package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Session backend built on Claude Code background agents (see agents.go).
// A ccc session == one Telegram topic == one dedicated bg Claude agent.
//
// `/new <prompt>` creates the topic and immediately dispatches a bg agent in
// $HOME with that prompt. Subsequent messages to the topic resume the
// conversation. Sessions created another way (discovery, `ccc start`) keep
// their own working dir.

// titleFromPrompt derives a topic title from a session's initial prompt: one
// line, collapsed whitespace, clipped to a Telegram-friendly length. It's only
// the starting title — the poller renames the topic once the agent names itself.
func titleFromPrompt(prompt string) string {
	title := strings.Join(strings.Fields(prompt), " ")
	const maxRunes = 40
	if r := []rune(title); len(r) > maxRunes {
		title = strings.TrimRight(string(r[:maxRunes]), " ") + "…"
	}
	if title == "" {
		title = "session"
	}
	return title
}

// cccMarker is the stable per-session tag ccc embeds in a conversation so it
// can re-identify the session after a resume. `claude --bg --resume` mints a
// brand-new conversation UUID (and short id, and bridge id) and records no link
// back to the parent, so neither the fleet snapshot nor the job state can tie a
// resumed agent to the session it continues. The marker rides inside the
// conversation itself — copied into every resumed transcript — so it survives
// any resume, whether ccc or the PC agents view triggered it, and unlike the
// title (which Claude renames freely) it never changes. The stable id is the
// Telegram TopicID, which ccc owns and never reuses.
func cccMarker(topicID int64) string {
	return fmt.Sprintf("ccc-session:t%d", topicID)
}

// tagPrompt appends the hidden ccc marker to a prompt as an HTML comment, so it
// lands in the transcript with minimal noise to the model.
func tagPrompt(prompt string, topicID int64) string {
	return prompt + "\n\n<!-- " + cccMarker(topicID) + " -->"
}

// uniqueSessionName returns a config-map key based on title that doesn't
// collide with an existing session.
func uniqueSessionName(config *Config, title string) string {
	name := title
	for i := 2; ; i++ {
		if _, exists := config.Sessions[name]; !exists {
			return name
		}
		name = fmt.Sprintf("%s (%d)", title, i)
	}
}

// sessionWorkDir returns the working dir for a session. Sessions always run in
// $HOME unless they carry an explicit path (discovered agents, `ccc start`).
func sessionWorkDir(info *SessionInfo) string {
	if info != nil && info.Path != "" {
		return info.Path
	}
	home, _ := os.UserHomeDir() // safe-ignore: $HOME is always resolvable; "" would just mean "inherit cwd"
	return home
}

// agentDisplayName is the name a session's bg agent shows in the fleet view.
// It tracks the topic title (which itself mirrors whatever Claude renamed the
// session to), so resuming never resets the name back to the initial prompt.
func agentDisplayName(info *SessionInfo, sessName string) string {
	if info != nil && info.Title != "" {
		return info.Title
	}
	return sessName
}

// getSessionByTopic returns the session name mapped to a Telegram topic id.
func getSessionByTopic(config *Config, topicID int64) string {
	for name, info := range config.Sessions {
		if info != nil && info.TopicID == topicID {
			return name
		}
	}
	return ""
}

// getSessionByPath returns the session name whose working dir matches path.
func getSessionByPath(config *Config, path string) string {
	for name, info := range config.Sessions {
		if info != nil && info.Path == path {
			return name
		}
	}
	return ""
}

// liveShortID returns the current live/known short id for a session, preferring
// the fleet snapshot (authoritative) and falling back to the stored id.
func liveShortID(config *Config, sessName string, agents []AgentInfo) string {
	info := config.Sessions[sessName]
	if info == nil {
		return ""
	}
	if a, ok := agentBySessionID(agents, info.SessionID); ok {
		return a.ID
	}
	return info.ShortID
}

// appendOldSessionID records uuid in info.OldSessionIDs (dedup, most-recent-20
// cap). The lineage lets discovery skip a resumed conversation's pid-less parent
// so it never earns a duplicate discovery topic.
func appendOldSessionID(info *SessionInfo, uuid string) {
	if info == nil || uuid == "" {
		return
	}
	for _, id := range info.OldSessionIDs {
		if id == uuid {
			return
		}
	}
	info.OldSessionIDs = append(info.OldSessionIDs, uuid)
	if len(info.OldSessionIDs) > 20 {
		info.OldSessionIDs = info.OldSessionIDs[len(info.OldSessionIDs)-20:]
	}
}

// persistAgentIDs stores the current short id (and resolved conversation UUID)
// for a session after a dispatch or resume.
//
// The short id is persisted *immediately*, before the (up to 8s) UUID resolve:
// this is the short-id handoff that stops the mirror poller from racing a
// resume. A resume mints a new short id + UUID; until this returns, the tracked
// entry still holds the OLD UUID, so the poller would see the session "gone"
// (reap its topic) and the new agent "unknown" (spawn a duplicate topic).
// Recording the new short id up front lets the poller match the session by
// short id (liveShortID / discovery) and adopt the fresh UUID before either
// mis-fires.
func persistAgentIDs(sessName, shortID string) {
	// Phase 1: persist the fresh short id up front (the short-id handoff).
	updateConfig(func(config *Config) bool {
		info := config.Sessions[sessName]
		if info == nil {
			return false
		}
		info.ShortID = shortID
		return true
	})

	uuid := resolveSessionUUID(shortID, 8*time.Second)
	if uuid == "" {
		return
	}
	// Phase 2: adopt the resolved UUID. Record the previous UUID as lineage,
	// carry the human label across the new UUID, and clear the resume marker —
	// all in one mutation so the poller never sees a half-updated entry.
	var oldUUID string
	updateConfig(func(config *Config) bool {
		info := config.Sessions[sessName]
		if info == nil {
			return false
		}
		oldUUID = info.SessionID
		if info.SessionID != "" && info.SessionID != uuid {
			appendOldSessionID(info, info.SessionID)
		}
		info.ShortID = shortID
		info.SessionID = uuid
		info.ResumingAt = 0
		return true
	})
	carrySessionLabel(oldUUID, uuid)
}

// markSession records that ccc has embedded its stable marker in a session's
// conversation, so we don't re-tag every subsequent message.
func markSession(sessName string) {
	updateConfig(func(config *Config) bool {
		if info := config.Sessions[sessName]; info != nil && !info.Marked {
			info.Marked = true
			return true
		}
		return false
	})
}

// launchSessionAgent dispatches a brand-new bg agent for a session with an
// initial prompt, then persists its ids. workDir is created if missing.
func launchSessionAgent(config *Config, sessName, prompt string) error {
	info := config.Sessions[sessName]
	if info == nil {
		return fmt.Errorf("session '%s' not found", sessName)
	}
	workDir := sessionWorkDir(info)
	if _, err := os.Stat(workDir); os.IsNotExist(err) {
		os.MkdirAll(workDir, 0755)
	}
	// Embed the stable ccc marker in the opening prompt so the session stays
	// re-identifiable across resumes (see cccMarker).
	prompt = tagPrompt(prompt, info.TopicID)
	shortID, err := dispatchAgent(agentDisplayName(info, sessName), workDir, prompt)
	if err != nil {
		return err
	}
	persistAgentIDs(sessName, shortID)
	markSession(sessName)
	return nil
}

// sendToSession delivers a follow-up user message to a session's bg agent.
// If no agent is running yet (lazy session, or it settled), it dispatches a
// fresh one using the message as the initial prompt.
func sendToSession(config *Config, sessName, text string) error {
	info := config.Sessions[sessName]
	if info == nil {
		return fmt.Errorf("session '%s' not found", sessName)
	}
	workDir := sessionWorkDir(info)
	if _, err := os.Stat(workDir); os.IsNotExist(err) {
		os.MkdirAll(workDir, 0755)
	}

	// Tag the outgoing message with the stable ccc marker until the conversation
	// carries one — for sessions discovered from the fleet (ccc didn't write the
	// opening prompt), the first follow-up is where the marker gets embedded.
	prompt := text
	if !info.Marked {
		prompt = tagPrompt(text, info.TopicID)
	}

	// No prior conversation → dispatch a fresh agent with this message.
	if info.SessionID == "" {
		short, err := dispatchAgent(agentDisplayName(info, sessName), workDir, prompt)
		if err != nil {
			return err
		}
		persistAgentIDs(sessName, short)
		markSession(sessName)
		return nil
	}

	// Resume the existing conversation by its stable UUID. A still-resident bg
	// agent (even idle) counts as "running", so stop it first before resuming.
	agents, _ := listAgents(true)
	stopShort := ""
	if a, ok := agentBySessionID(agents, info.SessionID); ok {
		stopShort = a.ID
	}
	// Mark the entry mid-resume so the reaper doesn't delete the live topic while
	// the (slow) stop + wait + dispatch runs and the agent is briefly absent from
	// the fleet. Cleared once the new UUID is adopted (persistAgentIDs phase 2).
	updateConfig(func(c *Config) bool {
		if si := c.Sessions[sessName]; si != nil {
			si.ResumingAt = time.Now().Unix()
			return true
		}
		return false
	})
	newShort, err := resumeAgent(stopShort, info.SessionID, agentDisplayName(info, sessName), workDir, prompt)
	if err != nil {
		updateConfig(func(c *Config) bool {
			if si := c.Sessions[sessName]; si != nil {
				si.ResumingAt = 0
				return true
			}
			return false
		})
		return err
	}
	persistAgentIDs(sessName, newShort)
	markSession(sessName)
	return nil
}

// startSession handles the local `ccc` command: attach to this directory's bg
// session if one is live, otherwise fall back to a normal interactive claude.
func startSession(continueSession bool) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	name := filepath.Base(cwd)

	config, cfgErr := loadConfig()
	if cfgErr == nil && config != nil {
		sessName := getSessionByPath(config, cwd)
		if sessName == "" {
			if _, ok := config.Sessions[name]; ok {
				sessName = name
			}
		}
		if sessName != "" {
			agents, _ := listAgents(true)
			if short := liveShortID(config, sessName, agents); short != "" {
				fmt.Printf("Attaching to background session '%s' (%s)...\n", sessName, short)
				return runClaude2("attach", short)
			}
		}
	}

	// No live bg session for this dir → normal interactive claude.
	if continueSession {
		return runClaude2("--continue")
	}
	return runClaude2()
}

// startDetached creates a Telegram topic + bg agent for a session and sends an
// initial prompt, without attaching. Used by `ccc start <name> <dir> <prompt>`.
func startDetached(name string, workDir string, prompt string) error {
	config, err := loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if config.Sessions == nil {
		config.Sessions = make(map[string]*SessionInfo)
	}

	topicID, err := ensureTopic(config, name)
	if err != nil {
		return fmt.Errorf("failed to create topic: %w", err)
	}

	config.Sessions[name] = &SessionInfo{TopicID: topicID, Path: workDir, Title: name}
	if err := saveConfig(config); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	if err := launchSessionAgent(config, name, prompt); err != nil {
		return fmt.Errorf("failed to start agent: %w", err)
	}

	fmt.Printf("Session '%s' started (topic %d)\n", name, topicID)
	return nil
}

// ensureTopic returns the topic id for a session, creating it if needed.
func ensureTopic(config *Config, name string) (int64, error) {
	if info, ok := config.Sessions[name]; ok && info != nil && info.TopicID != 0 {
		return info.TopicID, nil
	}
	if config.GroupID == 0 {
		return 0, fmt.Errorf("no group configured (run: ccc setgroup)")
	}
	return createForumTopic(config, name)
}

// runClaude2 execs the claude binary interactively, inheriting stdio.
func runClaude2(args ...string) error {
	cmd := exec.Command(claudeBin(), args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
