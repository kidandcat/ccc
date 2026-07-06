package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Session backend built on Claude Code background agents (see agents.go).
// A ccc session == one Telegram topic == one dedicated bg Claude agent.
//
// Sessions are lazy: `/new <name>` just registers the topic + config entry;
// the FIRST message sent to the topic dispatches the bg agent with that
// message as the initial prompt. Subsequent messages resume the conversation.

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

// persistAgentIDs stores the current short id (and resolved conversation UUID)
// for a session after a dispatch or resume.
func persistAgentIDs(sessName, shortID string) {
	config, err := loadConfig()
	if err != nil || config == nil {
		return
	}
	info := config.Sessions[sessName]
	if info == nil {
		return
	}
	info.ShortID = shortID
	if uuid := resolveSessionUUID(shortID, 8*time.Second); uuid != "" {
		info.SessionID = uuid
	}
	saveConfig(config)
}

// launchSessionAgent dispatches a brand-new bg agent for a session with an
// initial prompt, then persists its ids. workDir is created if missing.
func launchSessionAgent(config *Config, sessName, prompt string) error {
	info := config.Sessions[sessName]
	if info == nil {
		return fmt.Errorf("session '%s' not found", sessName)
	}
	workDir := info.Path
	if workDir == "" {
		workDir = resolveProjectPath(config, sessName)
	}
	if _, err := os.Stat(workDir); os.IsNotExist(err) {
		os.MkdirAll(workDir, 0755)
	}
	shortID, err := dispatchAgent(sessName, workDir, prompt)
	if err != nil {
		return err
	}
	persistAgentIDs(sessName, shortID)
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
	workDir := info.Path
	if workDir == "" {
		workDir = resolveProjectPath(config, sessName)
	}
	if _, err := os.Stat(workDir); os.IsNotExist(err) {
		os.MkdirAll(workDir, 0755)
	}

	// No prior conversation → dispatch a fresh agent with this message.
	if info.SessionID == "" {
		short, err := dispatchAgent(sessName, workDir, text)
		if err != nil {
			return err
		}
		persistAgentIDs(sessName, short)
		return nil
	}

	// Resume the existing conversation by its stable UUID. A still-resident bg
	// agent (even idle) counts as "running", so stop it first before resuming.
	agents, _ := listAgents(true)
	stopShort := ""
	if a, ok := agentBySessionID(agents, info.SessionID); ok {
		stopShort = a.ID
	}
	newShort, err := resumeAgent(stopShort, info.SessionID, sessName, workDir, text)
	if err != nil {
		return err
	}
	persistAgentIDs(sessName, newShort)
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

	config.Sessions[name] = &SessionInfo{TopicID: topicID, Path: workDir}
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
