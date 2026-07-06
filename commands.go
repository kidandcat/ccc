package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// listenLog writes timestamped log entries to ccc.log AND stdout.
// This ensures logs are always persisted regardless of how the process is started.
var listenLogFile *os.File

func listenLog(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	line := fmt.Sprintf("[%s] [pid:%d] %s\n", time.Now().Format("2006-01-02 15:04:05"), os.Getpid(), msg)
	fmt.Print(line)
	if listenLogFile != nil {
		listenLogFile.WriteString(line)
	}
}

func initListenLog() {
	logPath := filepath.Join(cacheDir(), "ccc.log")
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		listenLogFile = f
	}
}

// getSystemStats returns machine stats (works on Linux and macOS)
func getSystemStats() string {
	var sb strings.Builder
	hostname, _ := os.Hostname()
	sb.WriteString(fmt.Sprintf("🖥 %s\n\n", hostname))

	// Uptime
	if out, err := exec.Command("uptime").Output(); err == nil {
		sb.WriteString(fmt.Sprintf("⏱ %s\n", strings.TrimSpace(string(out))))
	}

	// CPU info
	if out, err := exec.Command("uname", "-m").Output(); err == nil {
		arch := strings.TrimSpace(string(out))
		// Count cores: nproc on Linux, sysctl on macOS
		var cores string
		if c, err := exec.Command("nproc").Output(); err == nil {
			cores = strings.TrimSpace(string(c))
		} else if c, err := exec.Command("sysctl", "-n", "hw.ncpu").Output(); err == nil {
			cores = strings.TrimSpace(string(c))
		}
		sb.WriteString(fmt.Sprintf("🧠 CPU: %s cores (%s)\n", cores, arch))
	}

	// Memory: Linux uses free, macOS uses vm_stat + sysctl
	if out, err := exec.Command("free", "-h").Output(); err == nil {
		// Linux
		lines := strings.Split(string(out), "\n")
		for _, l := range lines {
			if strings.HasPrefix(l, "Mem:") {
				fields := strings.Fields(l)
				if len(fields) >= 4 {
					sb.WriteString(fmt.Sprintf("💾 RAM: %s used / %s total (available: %s)\n", fields[2], fields[1], fields[6]))
				}
				break
			}
		}
	} else {
		// macOS fallback
		total, _ := exec.Command("sysctl", "-n", "hw.memsize").Output()
		if len(total) > 0 {
			totalBytes := strings.TrimSpace(string(total))
			// Parse and convert to GB
			if tb, err := strconv.ParseUint(totalBytes, 10, 64); err == nil {
				totalGB := float64(tb) / (1024 * 1024 * 1024)
				sb.WriteString(fmt.Sprintf("💾 RAM: %.1f GB total\n", totalGB))
			}
		}
	}

	// Disk usage
	if out, err := exec.Command("df", "-h", "/").Output(); err == nil {
		lines := strings.Split(string(out), "\n")
		if len(lines) >= 2 {
			fields := strings.Fields(lines[1])
			if len(fields) >= 5 {
				sb.WriteString(fmt.Sprintf("💿 Disk /: %s used / %s (%s)\n", fields[2], fields[1], fields[4]))
			}
		}
	}
	if out, err := exec.Command("df", "-h", "/home").Output(); err == nil {
		lines := strings.Split(string(out), "\n")
		if len(lines) >= 2 {
			fields := strings.Fields(lines[1])
			if len(fields) >= 5 {
				// Only show if different from /
				sb.WriteString(fmt.Sprintf("💿 Disk /home: %s used / %s (%s)\n", fields[2], fields[1], fields[4]))
			}
		}
	}

	// Tmux sessions
	if out, err := exec.Command("tmux", "list-sessions").Output(); err == nil {
		sessions := strings.TrimSpace(string(out))
		if sessions != "" {
			count := len(strings.Split(sessions, "\n"))
			sb.WriteString(fmt.Sprintf("\n📟 Tmux sessions: %d\n", count))
			sb.WriteString(sessions)
		}
	}

	return sb.String()
}

// Execute shell command
func executeCommand(cmdStr string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	shell := "bash"
	if _, err := exec.LookPath("zsh"); err == nil {
		shell = "zsh"
	}
	cmd := exec.CommandContext(ctx, shell, "-l", "-c", cmdStr)
	cmd.Dir, _ = os.UserHomeDir()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	output := stdout.String()
	if stderr.Len() > 0 {
		if output != "" {
			output += "\n"
		}
		output += stderr.String()
	}

	if output == "" {
		if err != nil {
			output = fmt.Sprintf("Error: %v", err)
		} else {
			output = "(no output)"
		}
	}

	return strings.TrimSpace(output), err
}

// One-shot Claude run (for private chat)
func runClaude(prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	home, _ := os.UserHomeDir()
	workDir := home

	words := strings.Fields(prompt)
	if len(words) > 0 {
		firstWord := words[0]
		potentialDir := filepath.Join(home, firstWord)
		if info, err := os.Stat(potentialDir); err == nil && info.IsDir() {
			workDir = potentialDir
			prompt = strings.TrimSpace(strings.TrimPrefix(prompt, firstWord))
			if prompt == "" {
				return "Error: no prompt provided after directory name", nil
			}
		}
	}

	if claudePath == "" {
		return "Error: claude binary not found", fmt.Errorf("claude not found")
	}
	cmd := exec.CommandContext(ctx, claudePath, "--dangerously-skip-permissions", "-p", prompt)
	cmd.Dir = workDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	output := stdout.String()
	if stderr.Len() > 0 {
		if output != "" {
			output += "\n"
		}
		output += stderr.String()
	}

	if output == "" {
		if err != nil {
			output = fmt.Sprintf("Error: %v", err)
		} else {
			output = "(no output)"
		}
	}

	return strings.TrimSpace(output), err
}

func stopListenerService() {
	home, _ := os.UserHomeDir()
	if _, err := os.Stat("/Library"); err == nil {
		// macOS - launchd
		plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.ccc.plist")
		exec.Command("launchctl", "unload", plistPath).Run()
	} else {
		// Linux - systemd
		exec.Command("systemctl", "--user", "stop", "ccc").Run()
	}
	// Also kill any manual listener via lock file PID
	lockPath := filepath.Join(cacheDir(), "ccc.lock")
	if data, err := os.ReadFile(lockPath); err == nil {
		pidStr := strings.TrimSpace(string(data))
		if pidStr != "" {
			exec.Command("kill", pidStr).Run()
		}
	}
	time.Sleep(500 * time.Millisecond)
}

func startListenerService() {
	home, _ := os.UserHomeDir()
	if _, err := os.Stat("/Library"); err == nil {
		plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.ccc.plist")
		exec.Command("launchctl", "load", plistPath).Run()
	} else {
		exec.Command("systemctl", "--user", "start", "ccc").Run()
	}
}

func setup(botToken string) error {
	fmt.Println("🚀 Claude Code Companion Setup")
	fmt.Println("==============================")
	fmt.Println()

	// Load existing config if present (preserve sessions, group, etc.)
	config, _ := loadConfig()
	if config == nil {
		config = &Config{Sessions: make(map[string]*SessionInfo)}
	}
	config.BotToken = botToken

	// Stop listener to avoid getUpdates conflict (409 Conflict)
	fmt.Println("Stopping listener...")
	stopListenerService()

	// Step 1: Get chat ID
	fmt.Println("Step 1/4: Connecting to Telegram...")
	fmt.Println("   📱 Send any message to your bot in Telegram")
	fmt.Println("   Waiting...")

	offset := 0
	for {
		resp, err := telegramGet(botToken, fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=30", botToken, offset))
		if err != nil {
			return fmt.Errorf("failed to get updates: %w", err)
		}

		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
		resp.Body.Close()

		var updates TelegramUpdate
		if err := json.Unmarshal(body, &updates); err != nil {
			return fmt.Errorf("failed to parse response: %w", err)
		}

		if !updates.OK {
			return fmt.Errorf("telegram API error - check your bot token")
		}

		for _, update := range updates.Result {
			offset = update.UpdateID + 1
			if update.Message.Chat.ID != 0 {
				config.ChatID = update.Message.Chat.ID
				if err := saveConfig(config); err != nil {
					return fmt.Errorf("failed to save config: %w", err)
				}
				fmt.Printf("✅ Connected! (User: @%s)\n\n", update.Message.From.Username)
				goto step2
			}
		}

		time.Sleep(time.Second)
	}

step2:
	// Step 2: Group setup (optional)
	fmt.Println("Step 3/6: Group setup (optional)")
	fmt.Println("   For session topics, create a Telegram group with Topics enabled,")
	fmt.Println("   add your bot as admin, and send a message there.")
	fmt.Println("   Or press Enter to skip...")

	// Non-blocking check for group message with timeout
	fmt.Println("   Waiting 30 seconds for group message...")

	client := &http.Client{Timeout: 35 * time.Second}
	deadline := time.Now().Add(30 * time.Second)

	for time.Now().Before(deadline) {
		reqURL := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=5", config.BotToken, offset)
		resp, err := telegramClientGet(client, config.BotToken, reqURL)
		if err != nil {
			continue
		}

		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
		resp.Body.Close()

		var updates TelegramUpdate
		json.Unmarshal(body, &updates)

		for _, update := range updates.Result {
			offset = update.UpdateID + 1
			chat := update.Message.Chat
			if chat.Type == "supergroup" {
				config.GroupID = chat.ID
				saveConfig(config)
				fmt.Printf("✅ Group configured!\n\n")
				goto step3
			}
		}
	}
	fmt.Println("⏭️  Skipped (you can run 'ccc setgroup' later)")

step3:
	// Step 3: Install the ccc-send skill
	fmt.Println("Step 3/4: Installing ccc-send skill...")
	if err := installSkill(); err != nil {
		fmt.Printf("⚠️  Skill installation failed: %v\n", err)
	} else {
		fmt.Println()
	}

	// Step 4: Install service
	fmt.Println("Step 4/4: Installing background service...")
	if err := installService(); err != nil {
		fmt.Printf("⚠️  Service installation failed: %v\n", err)
		fmt.Println("   You can start manually with: ccc listen")
	} else {
		fmt.Println()
	}

	// Done!
	fmt.Println()
	fmt.Println("==============================")
	fmt.Println("✅ Setup complete!")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  ccc           Start Claude Code in current directory")
	fmt.Println("  ccc -c        Continue previous session")
	fmt.Println()
	if config.GroupID != 0 {
		fmt.Println("Telegram commands (in your group):")
		fmt.Println("  /new <name>   Create new session")
		fmt.Println("  /list         List sessions")
	} else {
		fmt.Println("To enable Telegram session topics:")
		fmt.Println("  1. Create a group with Topics enabled")
		fmt.Println("  2. Add bot as admin")
		fmt.Println("  3. Run: ccc setgroup")
	}

	// Restart listener service
	fmt.Println()
	fmt.Println("Restarting listener...")
	startListenerService()

	return nil
}

func setGroup(config *Config) error {
	fmt.Println("Send a message in the group where you want to use topics...")
	fmt.Println("(Make sure Topics are enabled in group settings)")

	offset := 0
	client := &http.Client{Timeout: 35 * time.Second}

	for {
		reqURL := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=30", config.BotToken, offset)
		resp, err := telegramClientGet(client, config.BotToken, reqURL)
		if err != nil {
			return err
		}

		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
		resp.Body.Close()

		var updates TelegramUpdate
		if err := json.Unmarshal(body, &updates); err != nil {
			continue
		}

		for _, update := range updates.Result {
			offset = update.UpdateID + 1
			chat := update.Message.Chat
			if chat.Type == "supergroup" && update.Message.From.ID == config.ChatID {
				config.GroupID = chat.ID
				if err := saveConfig(config); err != nil {
					return err
				}
				fmt.Printf("Group set: %d\n", chat.ID)
				fmt.Println("You can now create sessions with: /new <name>")
				return nil
			}
		}
	}
}

func doctor() {
	fmt.Println("🩺 ccc doctor")
	fmt.Println("=============")
	fmt.Println()

	allGood := true

	// Check claude
	fmt.Print("claude............ ")
	if claudePath != "" {
		fmt.Printf("✅ %s\n", claudePath)
	} else {
		fmt.Println("❌ not found")
		fmt.Println("   Install: npm install -g @anthropic-ai/claude-code")
		allGood = false
	}

	// Check background-agent support (claude agents)
	fmt.Print("claude agents..... ")
	if _, err := listAgents(true); err == nil {
		fmt.Println("✅ background agents available")
	} else {
		fmt.Printf("❌ %v\n", err)
		fmt.Println("   Update Claude Code: it must support `claude --bg` / `claude agents`")
		allGood = false
	}

	// Check ccc is in PATH (for hooks)
	fmt.Print("ccc in PATH....... ")
	home, _ := os.UserHomeDir()
	cccPaths := []string{
		filepath.Join(home, "bin", "ccc"),
		filepath.Join(home, "go", "bin", "ccc"),
	}
	foundCccPath := ""
	for _, p := range cccPaths {
		if _, err := os.Stat(p); err == nil {
			foundCccPath = p
			break
		}
	}
	if foundCccPath != "" {
		fmt.Printf("✅ %s\n", foundCccPath)
	} else {
		fmt.Println("❌ not found")
		fmt.Println("   Run: go install . (from ccc repo) or cp ccc ~/bin/")
		allGood = false
	}

	// Check config
	fmt.Print("config............ ")
	config, err := loadConfig()
	if err != nil {
		fmt.Println("❌ not found")
		fmt.Println("   Run: ccc setup <bot_token>")
		allGood = false
	} else {
		fmt.Printf("✅ %s\n", getConfigPath())

		// Check bot token
		fmt.Print("  bot_token....... ")
		if config.BotToken != "" {
			fmt.Println("✅ configured")
		} else {
			fmt.Println("❌ missing")
			allGood = false
		}

		// Check chat ID
		fmt.Print("  chat_id......... ")
		if config.ChatID != 0 {
			fmt.Printf("✅ %d\n", config.ChatID)
		} else {
			fmt.Println("❌ missing")
			allGood = false
		}

		// Check group ID (optional)
		fmt.Print("  group_id........ ")
		if config.GroupID != 0 {
			fmt.Printf("✅ %d\n", config.GroupID)
		} else {
			fmt.Println("⚠️  not set (optional, run: ccc setgroup)")
		}
	}

	// Check service
	fmt.Print("service........... ")
	if _, err := os.Stat("/Library"); err == nil {
		// macOS - check launchd
		plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.ccc.plist")
		if _, err := os.Stat(plistPath); err == nil {
			// Check if loaded
			cmd := exec.Command("launchctl", "list", "com.ccc")
			if cmd.Run() == nil {
				fmt.Println("✅ running (launchd)")
			} else {
				fmt.Println("⚠️  installed but not running")
				fmt.Println("   Run: launchctl load ~/Library/LaunchAgents/com.ccc.plist")
			}
		} else {
			fmt.Println("❌ not installed")
			fmt.Println("   Run: ccc setup <token> (or manually create plist)")
			allGood = false
		}
	} else {
		// Linux - check systemd
		cmd := exec.Command("systemctl", "--user", "is-active", "ccc")
		if output, err := cmd.Output(); err == nil && strings.TrimSpace(string(output)) == "active" {
			fmt.Println("✅ running (systemd)")
		} else {
			servicePath := filepath.Join(home, ".config", "systemd", "user", "ccc.service")
			if _, err := os.Stat(servicePath); err == nil {
				fmt.Println("⚠️  installed but not running")
				fmt.Println("   Run: systemctl --user start ccc")
			} else {
				fmt.Println("❌ not installed")
				fmt.Println("   Run: ccc setup <token> (or manually create service)")
				allGood = false
			}
		}
	}

	// Check transcription support
	doctorCheckWhisper()

	// Check OAuth token
	fmt.Print("oauth token....... ")
	if config != nil && config.OAuthToken != "" {
		fmt.Println("✅ configured (in config)")
	} else if os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" {
		fmt.Println("✅ configured (from environment)")
	} else {
		fmt.Println("⚠️  not set (optional)")
	}

	fmt.Println()
	if allGood {
		fmt.Println("✅ All checks passed!")
	} else {
		fmt.Println("❌ Some issues found. Fix them and run 'ccc doctor' again.")
	}
}

// Send notification (only if away)
func send(message string) error {
	config, err := loadConfig()
	if err != nil {
		return fmt.Errorf("not configured. Run: ccc setup <bot_token>")
	}

	if !config.Away {
		fmt.Println("Away mode off, skipping notification.")
		return nil
	}

	// Try to send to session topic if we're in a session directory
	if config.GroupID != 0 {
		cwd, _ := os.Getwd()
		for name, info := range config.Sessions {
			if info == nil {
				continue
			}
			// Match against saved path, subdirectories of saved path, or suffix
			if cwd == info.Path || strings.HasPrefix(cwd, info.Path+"/") || strings.HasSuffix(cwd, "/"+name) {
				return sendMessage(config, config.GroupID, info.TopicID, message)
			}
		}
	}

	// Fallback to private chat
	return sendMessage(config, config.ChatID, 0, message)
}

// Main listen loop: polls Telegram and mirrors the Claude fleet into topics.
func listen() error {
	// Small random delay to avoid race conditions when multiple instances start
	time.Sleep(time.Duration(os.Getpid()%500) * time.Millisecond)

	// Use a lock file to ensure only one instance runs
	lockPath := filepath.Join(cacheDir(), "ccc.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("failed to open lock file: %w", err)
	}
	defer lockFile.Close()

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Println("Another ccc listen instance is already running, exiting quietly")
		os.Exit(0)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)

	lockFile.Truncate(0)
	lockFile.Seek(0, 0)
	fmt.Fprintf(lockFile, "%d\n", os.Getpid())

	initListenLog()
	if listenLogFile != nil {
		defer listenLogFile.Close()
	}

	config, err := loadConfig()
	if err != nil {
		return fmt.Errorf("not configured. Run: ccc setup <bot_token>")
	}

	listenLog("Bot started (chat: %d, group: %d, sessions: %d)", config.ChatID, config.GroupID, len(config.Sessions))

	setBotCommands(config.BotToken)

	// Recover undelivered Telegram messages from ledger
	for sessName, info := range config.Sessions {
		if info == nil || info.TopicID == 0 || config.GroupID == 0 {
			continue
		}
		undelivered := findUndelivered(sessName, "telegram")
		for _, ur := range undelivered {
			if ur.Type == "assistant_text" || ur.Type == "notification" {
				sendMessage(config, config.GroupID, info.TopicID, ur.Text)
				updateDelivery(sessName, ur.ID, "telegram_delivered", true)
			}
		}
	}

	// Launch the fleet mirror poller (pull-based: no Claude hooks needed).
	go runMirror()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		listenLog("Shutting down (signal: %v)", sig)
		os.Exit(0)
	}()

	offset := 0
	client := &http.Client{Timeout: 35 * time.Second}

	for {
		reqURL := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=30", config.BotToken, offset)
		resp, err := telegramClientGet(client, config.BotToken, reqURL)
		if err != nil {
			listenLog("Network error: %v (retrying...)", err)
			time.Sleep(5 * time.Second)
			continue
		}

		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
		resp.Body.Close()

		var updates TelegramUpdate
		if err := json.Unmarshal(body, &updates); err != nil {
			listenLog("Parse error: %v", err)
			time.Sleep(time.Second)
			continue
		}
		if !updates.OK {
			listenLog("Telegram API error: %s", updates.Description)
			time.Sleep(5 * time.Second)
			continue
		}

		for _, update := range updates.Result {
			offset = update.UpdateID + 1

			// Handle callback queries (inline button taps for AskUserQuestion)
			if update.CallbackQuery != nil {
				handleCallback(config, update.CallbackQuery)
				continue
			}

			msg := update.Message

			// Only accept from the authorized user
			if msg.From.ID != config.ChatID {
				continue
			}

			chatID := msg.Chat.ID
			threadID := msg.MessageThreadID
			isGroup := msg.Chat.Type == "supergroup"

			// Voice messages → transcribe → deliver to session
			if msg.Voice != nil && isGroup && threadID > 0 {
				config, _ = loadConfig()
				sessName := getSessionByTopic(config, threadID)
				if sessName == "" {
					continue
				}
				sendMessage(config, chatID, threadID, "🎤 Transcribing...")
				audioPath := filepath.Join(os.TempDir(), fmt.Sprintf("voice_%d.ogg", time.Now().UnixNano()))
				if err := downloadTelegramFile(config, msg.Voice.FileID, audioPath); err != nil {
					sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Download failed: %v", err))
					continue
				}
				transcription, err := transcribeAudio(config, audioPath)
				os.Remove(audioPath)
				if err != nil || transcription == "" {
					sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Transcription failed: %v", err))
					continue
				}
				listenLog("[voice] @%s: %s", msg.From.Username, transcription)
				sendMessage(config, chatID, threadID, fmt.Sprintf("📝 %s", transcription))
				text := "[Audio transcription, may contain errors]: " + transcription
				deliverToSession(config, sessName, chatID, threadID, text)
				continue
			}

			// Photo messages → save → deliver path to session
			if len(msg.Photo) > 0 && isGroup && threadID > 0 {
				config, _ = loadConfig()
				sessName := getSessionByTopic(config, threadID)
				if sessName == "" {
					continue
				}
				photo := msg.Photo[len(msg.Photo)-1]
				imgPath := filepath.Join(os.TempDir(), fmt.Sprintf("telegram_%d.jpg", time.Now().UnixNano()))
				if err := downloadTelegramFile(config, photo.FileID, imgPath); err != nil {
					sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Download failed: %v", err))
					continue
				}
				caption := msg.Caption
				if caption == "" {
					caption = "Analyze this image:"
				}
				sendMessage(config, chatID, threadID, "📷 Image saved, sending to Claude...")
				deliverToSession(config, sessName, chatID, threadID, fmt.Sprintf("%s %s", caption, imgPath))
				continue
			}

			// Document messages → save into project dir → tell the session
			if msg.Document != nil && isGroup && threadID > 0 {
				config, _ = loadConfig()
				sessName := getSessionByTopic(config, threadID)
				if sessName == "" {
					continue
				}
				info := config.Sessions[sessName]
				destDir := info.Path
				if destDir == "" {
					destDir = resolveProjectPath(config, sessName)
				}
				destPath := filepath.Join(destDir, msg.Document.FileName)
				if err := downloadTelegramFile(config, msg.Document.FileID, destPath); err != nil {
					sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Download failed: %v", err))
					continue
				}
				caption := msg.Caption
				if caption == "" {
					caption = fmt.Sprintf("I sent you this file: %s", destPath)
				} else {
					caption = fmt.Sprintf("%s\n\nFile: %s", caption, destPath)
				}
				sendMessage(config, chatID, threadID, fmt.Sprintf("📎 File saved: %s", destPath))
				deliverToSession(config, sessName, chatID, threadID, caption)
				continue
			}

			text := strings.TrimSpace(msg.Text)
			if text == "" {
				continue
			}

			// Strip bot mention from commands (e.g. /ping@botname -> /ping)
			if strings.HasPrefix(text, "/") {
				if idx := strings.Index(text, "@"); idx != -1 {
					spaceIdx := strings.Index(text, " ")
					if spaceIdx == -1 || idx < spaceIdx {
						text = text[:idx] + text[strings.Index(text+" ", " "):]
					}
				}
				text = strings.TrimSpace(text)
			}

			listenLog("[%s] @%s: %s", msg.Chat.Type, msg.From.Username, text)

			// --- Global commands ---
			if strings.HasPrefix(text, "/c ") {
				output, err := executeCommand(strings.TrimPrefix(text, "/c "))
				if err != nil {
					output = fmt.Sprintf("⚠️ %s\n\nExit: %v", output, err)
				}
				sendMessage(config, chatID, threadID, output)
				continue
			}
			if text == "/update" {
				updateCCC(config, chatID, threadID, offset)
				continue
			}
			if text == "/restart" {
				sendMessage(config, chatID, threadID, "🔄 Restarting ccc service...")
				go func() {
					time.Sleep(500 * time.Millisecond)
					exe, err := os.Executable()
					if err != nil {
						return
					}
					exec.Command(exe, "listen").Start()
					os.Exit(0)
				}()
				continue
			}
			if text == "/stats" {
				sendMessage(config, chatID, threadID, getSystemStats())
				continue
			}
			if text == "/version" {
				sendMessage(config, chatID, threadID, fmt.Sprintf("ccc %s", version))
				continue
			}
			if text == "/list" {
				sendMessage(config, chatID, threadID, formatSessionList(config))
				continue
			}

			// /new <name> — create a new lazy session + topic
			// /new (in topic) — reset the conversation (fresh context next message)
			if strings.HasPrefix(text, "/new") && isGroup {
				config, _ = loadConfig()
				arg := strings.TrimSpace(strings.TrimPrefix(text, "/new"))
				if arg != "" {
					if existing, ok := config.Sessions[arg]; ok && existing != nil && existing.TopicID != 0 {
						sendMessage(config, chatID, threadID, fmt.Sprintf("⚠️ Session '%s' already exists.", arg))
						continue
					}
					topicID, err := createForumTopic(config, arg)
					if err != nil {
						sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Failed to create topic: %v", err))
						continue
					}
					workDir := resolveProjectPath(config, arg)
					if existing, ok := config.Sessions[arg]; ok && existing != nil && existing.Path != "" {
						workDir = existing.Path
					}
					config.Sessions[arg] = &SessionInfo{TopicID: topicID, Path: workDir}
					saveConfig(config)
					sendMessage(config, config.GroupID, topicID, fmt.Sprintf("🆕 Session '%s' ready (%s).\n\nSend a message here to start Claude working.", arg, workDir))
					continue
				}
				if threadID > 0 {
					sessName := getSessionByTopic(config, threadID)
					if sessName == "" {
						sendMessage(config, chatID, threadID, "❌ No session mapped to this topic. Use /new <name>.")
						continue
					}
					resetSession(config, sessName)
					sendMessage(config, chatID, threadID, fmt.Sprintf("🔄 Session '%s' reset — next message starts a fresh conversation.", sessName))
				} else {
					sendMessage(config, chatID, threadID, "Usage: /new <name> to create a new session")
				}
				continue
			}

			// /stop — stop the current session's agent (keeps conversation)
			if text == "/stop" && isGroup && threadID > 0 {
				config, _ = loadConfig()
				sessName := getSessionByTopic(config, threadID)
				if sessName == "" {
					sendMessage(config, chatID, threadID, "❌ No session mapped to this topic.")
					continue
				}
				agents, _ := listAgents(true)
				if short := liveShortID(config, sessName, agents); short != "" {
					stopAgent(short)
					sendMessage(config, chatID, threadID, fmt.Sprintf("⏹️ Stopped '%s' (conversation kept — send a message to resume).", sessName))
				} else {
					sendMessage(config, chatID, threadID, "Nothing running.")
				}
				continue
			}

			// /delete — stop agent, delete topic + config entry
			if text == "/delete" && isGroup && threadID > 0 {
				config, _ = loadConfig()
				sessName := getSessionByTopic(config, threadID)
				if sessName == "" {
					sendMessage(config, chatID, threadID, "❌ No session mapped to this topic.")
					continue
				}
				agents, _ := listAgents(true)
				if short := liveShortID(config, sessName, agents); short != "" {
					stopAgent(short)
				}
				topicID := config.Sessions[sessName].TopicID
				delete(config.Sessions, sessName)
				saveConfig(config)
				if err := deleteForumTopic(config, topicID); err != nil {
					sendMessage(config, chatID, threadID, fmt.Sprintf("⚠️ Session deleted but failed to delete thread: %v", err))
				}
				continue
			}

			// /cleanup — stop all agents, delete all topics, clear config
			if text == "/cleanup" {
				config, _ = loadConfig()
				if len(config.Sessions) == 0 {
					sendMessage(config, chatID, threadID, "No sessions to clean up.")
					continue
				}
				agents, _ := listAgents(true)
				var cleaned []string
				for sessName, info := range config.Sessions {
					if short := liveShortID(config, sessName, agents); short != "" {
						stopAgent(short)
					}
					if info.TopicID > 0 && config.GroupID > 0 {
						deleteForumTopic(config, info.TopicID)
					}
					cleaned = append(cleaned, sessName)
				}
				config.Sessions = make(map[string]*SessionInfo)
				saveConfig(config)
				sendMessage(config, chatID, threadID, fmt.Sprintf("🧹 Cleaned %d sessions: %s", len(cleaned), strings.Join(cleaned, ", ")))
				continue
			}

			// --- Topic message → deliver to the session's bg agent ---
			if isGroup && threadID > 0 {
				config, _ = loadConfig()
				sessName := getSessionByTopic(config, threadID)
				if sessName == "" {
					sendMessage(config, chatID, threadID, "⚠️ No session linked to this topic. Use /new <name>.")
					continue
				}
				ledgerID := fmt.Sprintf("tg:%d", update.UpdateID)
				appendMessage(&MessageRecord{
					ID: ledgerID, Session: sessName, Type: "user_prompt",
					Text: text, Origin: "telegram",
					TerminalDelivered: false, TelegramDelivered: true,
				})
				deliverToSession(config, sessName, chatID, threadID, text)
				updateDelivery(sessName, ledgerID, "terminal_delivered", true)
				continue
			}

			// --- Private chat → one-shot Claude query ---
			if !isGroup {
				sendMessage(config, chatID, threadID, "🤖 Running Claude...")
				prompt := text
				if msg.ReplyToMessage != nil && msg.ReplyToMessage.Text != "" {
					prompt = fmt.Sprintf("Original message:\n%s\n\nReply:\n%s", msg.ReplyToMessage.Text, text)
				}
				go func(p string, cid int64) {
					defer func() {
						if r := recover(); r != nil {
							sendMessage(config, cid, 0, fmt.Sprintf("💥 Panic: %v", r))
						}
					}()
					output, err := runClaude(p)
					if err != nil {
						output = fmt.Sprintf("⚠️ %s\n\nExit: %v", output, err)
					}
					sendMessage(config, cid, 0, output)
				}(prompt, chatID)
			}
		}
	}
}

// deliverToSession dispatches/resumes a session's bg agent with a user message,
// reporting failures to the topic. Runs the (blocking) resume in a goroutine so
// the listen loop stays responsive.
func deliverToSession(config *Config, sessName string, chatID, threadID int64, text string) {
	go func() {
		defer func() { recover() }()
		if err := sendToSession(config, sessName, text); err != nil {
			listenLog("deliver to %s failed: %v", sessName, err)
			sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Failed to send: %v", err))
		}
	}()
}

// resetSession stops the running agent and clears the stored ids so the next
// message starts a brand-new conversation for this session.
func resetSession(config *Config, sessName string) {
	agents, _ := listAgents(true)
	if short := liveShortID(config, sessName, agents); short != "" {
		stopAgent(short)
	}
	if info := config.Sessions[sessName]; info != nil {
		mirrorMu.Lock()
		delete(lastStatus, info.TopicID)
		mirrorMu.Unlock()
		info.SessionID = ""
		info.ShortID = ""
		saveConfig(config)
	}
}

// handleCallback processes an inline-button tap for an AskUserQuestion. It
// writes the chosen option to the answer file that the blocked PreToolUse hook
// is polling (see hooks.go); the hook then returns the answer to Claude with no
// resume or session churn.
func handleCallback(config *Config, cb *CallbackQuery) {
	if cb.From.ID != config.ChatID {
		return
	}
	answerCallbackQuery(config, cb.ID)

	// callback_data: "q:<sid8>:<qIdx>:<optIdx>"
	parts := strings.Split(cb.Data, ":")
	if len(parts) != 4 || parts[0] != "q" {
		return
	}
	sid8 := parts[1]
	qIdx, _ := strconv.Atoi(parts[2])
	optIdx, _ := strconv.Atoi(parts[3])

	// Record the answer for the waiting hook.
	os.WriteFile(qAnswerPath(sid8, qIdx), []byte(strconv.Itoa(optIdx)), 0600)

	// Reflect the choice in the message and drop the keyboard.
	if cb.Message != nil {
		chosen := fmt.Sprintf("option %d", optIdx+1)
		if m := readQMeta(sid8); m != nil && qIdx < len(m.Questions) && optIdx < len(m.Questions[qIdx].Options) {
			chosen = m.Questions[qIdx].Options[optIdx]
		}
		editMessageRemoveKeyboard(config, cb.Message.Chat.ID, cb.Message.MessageID,
			fmt.Sprintf("%s\n\n✓ %s", cb.Message.Text, chosen))
	}
	listenLog("[callback] answer sid=%s q=%d opt=%d", sid8, qIdx, optIdx)
}

// formatSessionList renders the current sessions and their live status.
func formatSessionList(config *Config) string {
	if len(config.Sessions) == 0 {
		return "No sessions. Create one with /new <name>."
	}
	agents, _ := listAgents(true)
	var sb strings.Builder
	sb.WriteString("*Sessions:*\n")
	for name, info := range config.Sessions {
		status := "idle"
		if a, ok := agentBySessionID(agents, info.SessionID); ok {
			status = classifyState(a.State, a.Status)
		}
		sb.WriteString(fmt.Sprintf("• %s — %s\n", name, status))
	}
	return sb.String()
}

func printHelp() {
	fmt.Printf(`ccc - Claude Code Companion v%s

Control Claude Code background-agent sessions remotely via Telegram.
Each session is a dedicated bg agent, visible in `+"`claude agents`"+`.

USAGE:
    ccc                     Attach to this directory's bg session (or run claude)
    ccc -c                  Continue previous local session
    ccc <message>           Send notification (if away mode is on)

COMMANDS:
    setup <token>           Complete setup (bot + skill + service)
    doctor                  Check dependencies and configuration
    config                  Show/set configuration values
    setgroup                Configure Telegram group for topics
    listen                  Start the Telegram bot listener manually
    send <file>             Send a file to the current session's topic
    start <name> <dir> <p>  Start a detached session with an initial prompt
    relay [port]            Start relay server for large files (default: 8080)

TELEGRAM COMMANDS:
    /new <name>             Create a new session + topic
    /new                    Reset the conversation in this topic
    /stop                   Stop this session's agent (keeps conversation)
    /delete                 Delete this session + topic
    /cleanup                Delete all sessions + topics
    /list                   List sessions and their status
    /c <cmd>                Execute a shell command
    /stats                  System stats
    /update                 Update ccc from GitHub
    /restart                Restart the ccc service

    Send a message in a topic to talk to that session's Claude agent.
    When Claude asks a question, tap the inline buttons to answer.

FLAGS:
    -h, --help              Show this help
    -v, --version           Show version

For more info: https://github.com/kidandcat/ccc
`, version)
}
