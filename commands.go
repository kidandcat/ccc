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
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/huh"
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

var authInProgress sync.Mutex
var authWaitingCode bool
var otpAttempts = make(map[string]int) // session -> failed attempts

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

	// Step 1: Permission mode
	fmt.Println("Step 1/6: Permission mode")
	var permMode string
	err := huh.NewSelect[string]().
		Title("How should remote sessions handle permissions?").
		Description("This controls what happens when Claude Code needs\npermission to run tools in Telegram-controlled sessions.").
		Options(
			huh.NewOption[string](
				"Auto-approve\n"+
					"  All permissions granted automatically. Claude works without\n"+
					"  interruptions. Best for trusted environments where you control\n"+
					"  physical access to your machine.",
				"auto"),
			huh.NewOption[string](
				"OTP (secure)\n"+
					"  Each permission requires a 6-digit TOTP code from your\n"+
					"  authenticator app (Google Authenticator, Authy, etc.).\n"+
					"  Local terminal sessions keep their normal interactive UI.",
				"otp"),
		).
		Value(&permMode).
		Run()
	if err != nil {
		return fmt.Errorf("selection cancelled: %w", err)
	}
	fmt.Println()

	// Step 2: Get chat ID
	fmt.Println("Step 2/6: Connecting to Telegram...")
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
	// Step 3: Install Claude hook and skill
	fmt.Println("Step 4/6: Installing Claude hook and skill...")
	if err := installHook(); err != nil {
		fmt.Printf("⚠️  Hook installation failed: %v\n", err)
		fmt.Println("   You can install it later with: ccc install")
	}
	if err := installSkill(); err != nil {
		fmt.Printf("⚠️  Skill installation failed: %v\n", err)
	} else {
		fmt.Println()
	}

	// Step 4: Install service
	fmt.Println("Step 5/6: Installing background service...")
	if err := installService(); err != nil {
		fmt.Printf("⚠️  Service installation failed: %v\n", err)
		fmt.Println("   You can start manually with: ccc listen")
	} else {
		fmt.Println()
	}

	// Step 6: Apply permission mode
	fmt.Println("Step 6/6: Configuring permission mode...")
	if permMode == "otp" {
		msg, err := setupOTP(config)
		if err != nil {
			fmt.Printf("⚠️  OTP setup failed: %v\n", err)
		} else {
			fmt.Println()
			fmt.Println(msg)
			fmt.Println()
			fmt.Println("   Save this secret! You'll need it to approve remote permission requests.")
		}
	} else {
		config.OTPSecret = ""
		if err := saveConfig(config); err != nil {
			fmt.Printf("⚠️  Failed to save config: %v\n", err)
		}
		fmt.Println("✅ Auto-approve mode — all remote permissions granted automatically")
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

	// Check tmux
	fmt.Print("tmux.............. ")
	if tmuxPath != "" {
		fmt.Printf("✅ %s\n", tmuxPath)
	} else {
		fmt.Println("❌ not found")
		fmt.Println("   Install: brew install tmux (macOS) or apt install tmux (Linux)")
		allGood = false
	}

	// Check claude
	fmt.Print("claude............ ")
	if claudePath != "" {
		fmt.Printf("✅ %s\n", claudePath)
	} else {
		fmt.Println("❌ not found")
		fmt.Println("   Install: npm install -g @anthropic-ai/claude-code")
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

	// Check Claude hooks (Stop, Notification, PreToolUse)
	fmt.Print("claude hooks...... ")
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	if data, err := os.ReadFile(settingsPath); err == nil {
		var settings map[string]interface{}
		if json.Unmarshal(data, &settings) == nil {
			if hooks, ok := settings["hooks"].(map[string]interface{}); ok {
				var installed []string
				if stop, has := hooks["Stop"].([]interface{}); has && len(stop) > 0 {
					installed = append(installed, "Stop")
				}
				if pre, has := hooks["PreToolUse"].([]interface{}); has && len(pre) > 0 {
					installed = append(installed, "PreToolUse")
				}
				if len(installed) == 2 {
					fmt.Printf("✅ installed (%s)\n", strings.Join(installed, ", "))
				} else if len(installed) > 0 {
					fmt.Printf("⚠️  partial (%s) - run: ccc install\n", strings.Join(installed, ", "))
				} else {
					fmt.Println("❌ not installed (run: ccc install)")
				}
			} else {
				fmt.Println("❌ not installed (run: ccc install)")
			}
		} else {
			fmt.Println("⚠️  settings.json parse error")
		}
	} else {
		fmt.Println("⚠️  ~/.claude/settings.json not found")
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

	// Check OTP (permission approval)
	fmt.Print("OTP (permissions). ")
	if config != nil && isOTPEnabled(config) {
		fmt.Println("✅ enabled")
	} else {
		fmt.Println("⚠️  disabled (run: ccc setup <token> to enable)")
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

// Main listen loop
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

	// Try to acquire exclusive lock (non-blocking)
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Println("Another ccc listen instance is already running, exiting quietly")
		os.Exit(0) // Exit with 0 so launchd doesn't restart
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)

	// Write our PID to the lock file
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
				sendMessage(config, config.GroupID, info.TopicID, fmt.Sprintf("*%s:*\n%s", sessName, ur.Text))
				updateDelivery(sessName, ur.ID, "telegram_delivered", true)
			}
		}
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	offset := 0
	client := &http.Client{Timeout: 35 * time.Second}

	go func() {
		sig := <-sigChan
		listenLog("Shutting down (signal: %v)", sig)
		os.Exit(0)
	}()

	// Typing indicator goroutine: sends "typing" action for sessions with thinking flag
	go func() {
		for {
			time.Sleep(4 * time.Second)
			cfg, err := loadConfig()
			if err != nil || cfg == nil {
				continue
			}
			for sessName, info := range cfg.Sessions {
				if info == nil || info.TopicID == 0 || cfg.GroupID == 0 {
					continue
				}
				if flagInfo, err := os.Stat(thinkingFlag(sessName)); err == nil {
					// Auto-expire after 10 minutes to handle missed stop hooks
					if time.Since(flagInfo.ModTime()) > 10*time.Minute {
						clearThinking(sessName)
						continue
					}
					sendTypingAction(cfg, cfg.GroupID, info.TopicID)
				}
			}
		}
	}()

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

			// Handle callback queries (button presses)
			if update.CallbackQuery != nil {
				cb := update.CallbackQuery
				// Only accept from authorized user
				if cb.From.ID != config.ChatID {
					continue
				}

					answerCallbackQuery(config, cb.ID)

				// Parse callback data: session:questionIndex:totalQuestions:optionIndex
				parts := strings.Split(cb.Data, ":")
				if len(parts) >= 3 {
					sessionName := parts[0]
					questionIndex, _ := strconv.Atoi(parts[1])
					var totalQuestions, optionIndex int
					if len(parts) == 4 {
						totalQuestions, _ = strconv.Atoi(parts[2])
						optionIndex, _ = strconv.Atoi(parts[3])
					} else {
						// Legacy format: session:questionIndex:optionIndex
						optionIndex, _ = strconv.Atoi(parts[2])
					}

					// Edit message to show selection and remove buttons
					if cb.Message != nil {
						originalText := cb.Message.Text
						newText := fmt.Sprintf("%s\n\n✓ Selected option %d", originalText, optionIndex+1)
						editMessageRemoveKeyboard(config, cb.Message.Chat.ID, cb.Message.MessageID, newText)
					}

					tmuxName := tmuxSafeName(sessionName)
					windowID := getWindowID(config, sessionName)
					if tmuxWindowExistsByID(windowID, tmuxName) {
						target := tmuxTargetByID(windowID, tmuxName)
						// Send arrow down keys to select option, then Enter
						for i := 0; i < optionIndex; i++ {
							exec.Command(tmuxPath, "send-keys", "-t", target, "Down").Run()
							time.Sleep(50 * time.Millisecond)
						}
						exec.Command(tmuxPath, "send-keys", "-t", target, "Enter").Run()
						listenLog("[callback] Selected option %d for %s (question %d/%d)", optionIndex, sessionName, questionIndex+1, totalQuestions)

						// After the last question, send Enter to confirm "Submit answers"
						if totalQuestions > 0 && questionIndex == totalQuestions-1 {
							time.Sleep(300 * time.Millisecond)
							exec.Command(tmuxPath, "send-keys", "-t", target, "Enter").Run()
							listenLog("[callback] Auto-submitted answers for %s", sessionName)
						}
					}
				}

				continue
			}

			msg := update.Message

			// Only accept from authorized user
			if msg.From.ID != config.ChatID {
				continue
			}

			chatID := msg.Chat.ID
			threadID := msg.MessageThreadID
			isGroup := msg.Chat.Type == "supergroup"

			// Handle voice messages
			if msg.Voice != nil && isGroup && threadID > 0 {
				config, _ = loadConfig()
				sessionName := getSessionByTopic(config, threadID)
				if sessionName != "" {
					tmuxName := tmuxSafeName(sessionName)
					windowID := getWindowID(config, sessionName)
					if tmuxWindowExistsByID(windowID, tmuxName) {
						sendMessage(config, chatID, threadID, "🎤 Transcribing...")
						// Download and transcribe
						audioPath := filepath.Join(os.TempDir(), fmt.Sprintf("voice_%d.ogg", time.Now().UnixNano()))
						if err := downloadTelegramFile(config, msg.Voice.FileID, audioPath); err != nil {
							sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Download failed: %v", err))
						} else {
							transcription, err := transcribeAudio(config, audioPath)
							os.Remove(audioPath)
							if err != nil {
								sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Transcription failed: %v", err))
							} else if transcription != "" {
								listenLog("[voice] @%s: %s", msg.From.Username, transcription)
								sendMessage(config, chatID, threadID, fmt.Sprintf("📝 %s", transcription))
								voiceText := "[Audio transcription, may contain errors]: " + transcription
								voiceLedgerID := fmt.Sprintf("tg:%d:voice", msg.MessageID)
								appendMessage(&MessageRecord{
									ID: voiceLedgerID, Session: sessionName, Type: "user_prompt",
									Text: voiceText, Origin: "telegram",
									TerminalDelivered: false, TelegramDelivered: true,
								})
								if err := sendToTmuxFromTelegram(tmuxTargetByID(windowID, tmuxName), tmuxName, voiceText); err == nil {
									updateDelivery(sessionName, voiceLedgerID, "terminal_delivered", true)
								}
							}
						}
					}
				}
				continue
			}

			// Handle photo messages
			if len(msg.Photo) > 0 && isGroup && threadID > 0 {
				config, _ = loadConfig()
				sessionName := getSessionByTopic(config, threadID)
				if sessionName != "" {
					tmuxName := tmuxSafeName(sessionName)
					windowID := getWindowID(config, sessionName)
					if tmuxWindowExistsByID(windowID, tmuxName) {
						// Get largest photo (last in array)
						photo := msg.Photo[len(msg.Photo)-1]
						imgPath := filepath.Join(os.TempDir(), fmt.Sprintf("telegram_%d.jpg", time.Now().UnixNano()))
						if err := downloadTelegramFile(config, photo.FileID, imgPath); err != nil {
							sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Download failed: %v", err))
						} else {
							caption := msg.Caption
							if caption == "" {
								caption = "Analyze this image:"
							}
							prompt := fmt.Sprintf("%s %s", caption, imgPath)
							sendMessage(config, chatID, threadID, fmt.Sprintf("📷 Image saved, sending to Claude..."))
							photoLedgerID := fmt.Sprintf("tg:%d:photo", msg.MessageID)
							appendMessage(&MessageRecord{
								ID: photoLedgerID, Session: sessionName, Type: "user_prompt",
								Text: caption, Origin: "telegram",
								TerminalDelivered: false, TelegramDelivered: true,
							})
							if err := sendToTmuxFromTelegramWithDelay(tmuxTargetByID(windowID, tmuxName), tmuxName, prompt, 2*time.Second); err == nil {
								updateDelivery(sessionName, photoLedgerID, "terminal_delivered", true)
							}
						}
					}
				}
				continue
			}

			// Handle document messages
			if msg.Document != nil && isGroup && threadID > 0 {
				config, _ = loadConfig()
				sessionName := getSessionByTopic(config, threadID)
				if sessionName != "" {
					tmuxName := tmuxSafeName(sessionName)
					windowID := getWindowID(config, sessionName)
					if tmuxWindowExistsByID(windowID, tmuxName) {
						sessionInfo := config.Sessions[sessionName]
						destDir := sessionInfo.Path
						if destDir == "" {
							destDir = resolveProjectPath(config, sessionName)
						}
						destPath := filepath.Join(destDir, msg.Document.FileName)
						if err := downloadTelegramFile(config, msg.Document.FileID, destPath); err != nil {
							sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Download failed: %v", err))
						} else {
							caption := msg.Caption
							if caption == "" {
								caption = fmt.Sprintf("I sent you this file: %s", destPath)
							} else {
								caption = fmt.Sprintf("%s\n\nFile: %s", caption, destPath)
							}
							sendMessage(config, chatID, threadID, fmt.Sprintf("📎 File saved: %s", destPath))
							docLedgerID := fmt.Sprintf("tg:%d:doc", msg.MessageID)
							appendMessage(&MessageRecord{
								ID: docLedgerID, Session: sessionName, Type: "user_prompt",
								Text: caption, Origin: "telegram",
								TerminalDelivered: false, TelegramDelivered: true,
							})
							if err := sendToTmuxFromTelegram(tmuxTargetByID(windowID, tmuxName), tmuxName, caption); err == nil {
								updateDelivery(sessionName, docLedgerID, "terminal_delivered", true)
							}
						}
					}
				}
				continue
			}

			text := strings.TrimSpace(msg.Text)
			if text == "" {
				continue
			}

			// Strip bot mention from commands (e.g., /ping@botname -> /ping)
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

			// Handle OTP code responses (for permission approval)
			if isOTPEnabled(config) && !strings.HasPrefix(text, "/") {
				pendingSession := findPendingOTPSession()
				if pendingSession != "" {
					code := strings.TrimSpace(text)
					if validateOTP(config.OTPSecret, code) {
						writeOTPResponse(pendingSession, true)
						delete(otpAttempts, pendingSession)
						sendMessage(config, chatID, threadID, "✅ Permission approved (valid for 5 min)")
					} else {
						otpAttempts[pendingSession]++
						remaining := 5 - otpAttempts[pendingSession]
						if remaining <= 0 {
							writeOTPResponse(pendingSession, false)
							delete(otpAttempts, pendingSession)
							sendMessage(config, chatID, threadID, "❌ Too many failed attempts - permission denied")
						} else {
							sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Invalid code — %d attempts remaining", remaining))
						}
					}
					continue
				}
			}

			// Handle commands
			if strings.HasPrefix(text, "/c ") {
				cmdStr := strings.TrimPrefix(text, "/c ")
				output, err := executeCommand(cmdStr)
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
				// Re-exec ourselves to restart cleanly
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
				stats := getSystemStats()
				sendMessage(config, chatID, threadID, stats)
				continue
			}

			if text == "/version" {
				sendMessage(config, chatID, threadID, fmt.Sprintf("ccc %s", version))
				continue
			}

			if text == "/auth" {
				go handleAuth(config, chatID, threadID)
				continue
			}

			// If auth is waiting for code, send it
			if authWaitingCode && !strings.HasPrefix(text, "/") {
				go handleAuthCode(config, chatID, threadID, text)
				continue
			}

			// /continue command - restart session preserving conversation history
			if text == "/continue" && isGroup && threadID > 0 {
				config, _ = loadConfig()
				sessName := getSessionByTopic(config, threadID)
				if sessName == "" {
					sendMessage(config, chatID, threadID, "❌ No session mapped to this topic. Use /new <name> to create one.")
					continue
				}
				tmuxName := tmuxSafeName(sessName)
				windowID := getWindowID(config, sessName)
				if tmuxWindowExistsByID(windowID, tmuxName) {
					killTmuxWindow(windowID, tmuxName)
					time.Sleep(300 * time.Millisecond)
				}
				// Use the stored path from config, fallback to resolveProjectPath
				sessionInfo := config.Sessions[sessName]
				workDir := sessionInfo.Path
				if workDir == "" {
					workDir = resolveProjectPath(config, sessName)
				}
				if _, err := os.Stat(workDir); os.IsNotExist(err) {
					os.MkdirAll(workDir, 0755)
				}
				newWindowID, err := createTmuxWindow(tmuxName, workDir, true)
				if err != nil {
					sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Failed to start: %v", err))
				} else {
					config.Sessions[sessName].WindowID = newWindowID
					saveConfig(config)
					time.Sleep(500 * time.Millisecond)
					if tmuxWindowExistsByID(newWindowID, tmuxName) {
						sendMessage(config, chatID, threadID, fmt.Sprintf("🔄 Session '%s' restarted with conversation history", sessName))
					} else {
						sendMessage(config, chatID, threadID, "⚠️ Session died immediately")
					}
				}
				continue
			}

			// /delete command - delete session and thread
			if text == "/delete" && isGroup && threadID > 0 {
				config, _ = loadConfig()
				sessName := getSessionByTopic(config, threadID)
				if sessName == "" {
					sendMessage(config, chatID, threadID, "❌ No session mapped to this topic.")
					continue
				}
				// Kill tmux window
				tmuxName := tmuxSafeName(sessName)
				windowID := getWindowID(config, sessName)
				if tmuxWindowExistsByID(windowID, tmuxName) {
					killTmuxWindow(windowID, tmuxName)
				}
				// Remove from config
				topicID := config.Sessions[sessName].TopicID
				delete(config.Sessions, sessName)
				saveConfig(config)
				// Delete telegram thread
				if err := deleteForumTopic(config, topicID); err != nil {
					sendMessage(config, chatID, threadID, fmt.Sprintf("⚠️ Session deleted but failed to delete thread: %v", err))
				}
				// No message needed - thread is gone
				continue
			}

			// /cleanup command - delete tmux sessions and Telegram topics (NOT folders)
			if text == "/cleanup" {
				config, _ = loadConfig()
				if len(config.Sessions) == 0 {
					sendMessage(config, chatID, threadID, "No sessions to clean up.")
					continue
				}

				var cleaned []string
				var errors []string

				for sessName, info := range config.Sessions {
					// Kill tmux window
					tmuxName := tmuxSafeName(sessName)
					windowID := getWindowID(config, sessName)
					if tmuxWindowExistsByID(windowID, tmuxName) {
						killTmuxWindow(windowID, tmuxName)
					}

					// NOTE: No longer deleting project folders - only tmux sessions and threads
					_ = info // Keep info reference for TopicID below

					// Delete telegram thread
					if info.TopicID > 0 && config.GroupID > 0 {
						if err := deleteForumTopic(config, info.TopicID); err != nil {
							errors = append(errors, fmt.Sprintf("%s: %v", sessName, err))
						}
					}

					cleaned = append(cleaned, sessName)
				}

				// Clear all sessions from config
				config.Sessions = make(map[string]*SessionInfo)
				saveConfig(config)

				msg := fmt.Sprintf("🧹 Cleaned %d sessions: %s", len(cleaned), strings.Join(cleaned, ", "))
				if len(errors) > 0 {
					msg += fmt.Sprintf("\n\n⚠️ Errors:\n%s", strings.Join(errors, "\n"))
				}
				sendMessage(config, chatID, threadID, msg)
				continue
			}

			// /new command - create/restart session
			if strings.HasPrefix(text, "/new") && isGroup {
				config, _ = loadConfig()
				arg := strings.TrimSpace(strings.TrimPrefix(text, "/new"))

				// /new <name> - create brand new session + topic
				if arg != "" {
					existing, exists := config.Sessions[arg]
					if exists && existing != nil && existing.TopicID != 0 {
						sendMessage(config, chatID, threadID, fmt.Sprintf("⚠️ Session '%s' already exists. Use /new without args in that topic to restart.", arg))
						continue
					}
					topicID, err := createForumTopic(config, arg)
					if err != nil {
						sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Failed to create topic: %v", err))
						continue
					}
					// Use pre-configured path if session was preset, otherwise resolve from name
					workDir := resolveProjectPath(config, arg)
					if exists && existing != nil && existing.Path != "" {
						workDir = existing.Path
					}
					config.Sessions[arg] = &SessionInfo{
						TopicID: topicID,
						Path:    workDir,
					}
					saveConfig(config)
					if _, err := os.Stat(workDir); os.IsNotExist(err) {
						os.MkdirAll(workDir, 0755)
					}
					tmuxName := tmuxSafeName(arg)
					newWindowID, err := createTmuxWindow(tmuxName, workDir, false)
					if err != nil {
						sendMessage(config, config.GroupID, topicID, fmt.Sprintf("❌ Failed to start tmux: %v", err))
					} else {
						config.Sessions[arg].WindowID = newWindowID
						saveConfig(config)
						time.Sleep(500 * time.Millisecond)
						if tmuxWindowExistsByID(newWindowID, tmuxName) {
							sendMessage(config, config.GroupID, topicID, fmt.Sprintf("🚀 Session '%s' started!\n\nSend messages here to interact with Claude.", arg))
						} else {
							sendMessage(config, config.GroupID, topicID, fmt.Sprintf("⚠️ Session '%s' created but died immediately. Check if ~/bin/ccc works.", arg))
						}
					}
					continue
				}

				// Without args - restart session in current topic
				if threadID > 0 {
					sessionName := getSessionByTopic(config, threadID)
					if sessionName == "" {
						sendMessage(config, chatID, threadID, "❌ No session mapped to this topic. Use /new <name> to create one.")
						continue
					}
					tmuxName := tmuxSafeName(sessionName)
					windowID := getWindowID(config, sessionName)
					if tmuxWindowExistsByID(windowID, tmuxName) {
						killTmuxWindow(windowID, tmuxName)
						time.Sleep(300 * time.Millisecond)
					}
					sessionInfo := config.Sessions[sessionName]
					workDir := sessionInfo.Path
					if workDir == "" {
						workDir = resolveProjectPath(config, sessionName)
					}
					if _, err := os.Stat(workDir); os.IsNotExist(err) {
						os.MkdirAll(workDir, 0755)
					}
					newWindowID, err := createTmuxWindow(tmuxName, workDir, false)
					if err != nil {
						sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Failed to start: %v", err))
					} else {
						config.Sessions[sessionName].WindowID = newWindowID
						saveConfig(config)
						time.Sleep(500 * time.Millisecond)
						if tmuxWindowExistsByID(newWindowID, tmuxName) {
							sendMessage(config, chatID, threadID, fmt.Sprintf("🚀 Session '%s' restarted", sessionName))
						} else {
							sendMessage(config, chatID, threadID, "⚠️ Session died immediately")
						}
					}
				} else {
					sendMessage(config, chatID, threadID, "Usage: /new <name> to create a new session")
				}
				continue
			}

			// Check if message is in a topic (interactive session)
			if isGroup && threadID > 0 {
				// Reload config to get latest sessions
				config, _ = loadConfig()
				sessName := getSessionByTopic(config, threadID)
				if sessName != "" {
					// Send to tmux session
					tmuxName := tmuxSafeName(sessName)
					windowID := getWindowID(config, sessName)
					if !tmuxWindowExistsByID(windowID, tmuxName) {
						// Auto-start session if not running
						sessionInfo := config.Sessions[sessName]
						workDir := sessionInfo.Path
						if _, err := os.Stat(workDir); os.IsNotExist(err) {
							os.MkdirAll(workDir, 0755)
						}
						newWindowID, err := createTmuxWindow(tmuxName, workDir, false)
						if err != nil {
							sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Failed to start session: %v", err))
							continue
						}
						windowID = newWindowID
						config.Sessions[sessName].WindowID = newWindowID
						saveConfig(config)
						sendMessage(config, chatID, threadID, fmt.Sprintf("🚀 Session '%s' auto-started", sessName))
						time.Sleep(3 * time.Second) // Wait for Claude to fully start

						// Recover undelivered messages from ledger
						undelivered := findUndelivered(sessName, "terminal")
						if len(undelivered) > 0 {
							listenLog("recovering %d undelivered messages for %s", len(undelivered), sessName)
							recTarget := tmuxTargetByID(windowID, tmuxName)
							for _, ur := range undelivered {
								if ur.Type == "user_prompt" && ur.Origin == "telegram" {
									if err := sendToTmuxFromTelegram(recTarget, tmuxName, ur.Text); err == nil {
										updateDelivery(sessName, ur.ID, "terminal_delivered", true)
									}
									time.Sleep(500 * time.Millisecond)
								}
							}
						}
					}
					target := tmuxTargetByID(windowID, tmuxName)
					listenLog("sendToTmux: target=%s window=%s", target, tmuxName)

					// Record in DB before sending
					ledgerID := fmt.Sprintf("tg:%d", update.UpdateID)
					appendMessage(&MessageRecord{
						ID:                ledgerID,
						Session:           sessName,
						Type:              "user_prompt",
						Text:              text,
						Origin:            "telegram",
						TerminalDelivered: false,
						TelegramDelivered: true,
					})

					if err := sendToTmuxFromTelegram(target, tmuxName, text); err != nil {
						listenLog("sendToTmux FAILED: target=%s err=%v", target, err)
						sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Failed to send: %v", err))
					} else {
						// Delayed confirmation: check after 10s if Claude received it
						go func(sid, lid, tgt, wn, txt string, cid, tid int64) {
							time.Sleep(10 * time.Second)
							if hasUnconfirmedPrompt(sid, lid) {
								listenLog("delivery unconfirmed after 10s, resending: session=%s id=%s", sid, lid)
								sendToTmuxFromTelegram(tgt, wn, txt)
								sendMessage(config, cid, tid, "⚠️ 消息未确认，已重发")
							}
						}(sessName, ledgerID, target, tmuxName, text, chatID, threadID)
					}
				} else {
					sendMessage(config, chatID, threadID, "⚠️ No session linked to this topic. Use /new <name> to create one.")
				}
				continue
			}

			// Private chat: run one-shot Claude
			if !isGroup {
				sendMessage(config, chatID, threadID, "🤖 Running Claude...")

				prompt := text
				if msg.ReplyToMessage != nil && msg.ReplyToMessage.Text != "" {
					origText := msg.ReplyToMessage.Text
					origWords := strings.Fields(origText)
					if len(origWords) > 0 {
						home, _ := os.UserHomeDir()
						potentialDir := filepath.Join(home, origWords[0])
						if info, err := os.Stat(potentialDir); err == nil && info.IsDir() {
							prompt = origWords[0] + " " + text
						}
					}
					prompt = fmt.Sprintf("Original message:\n%s\n\nReply:\n%s", origText, prompt)
				}

				go func(p string, cid int64) {
					defer func() {
						if r := recover(); r != nil {
							sendMessage(config, cid, 0, fmt.Sprintf("💥 Panic: %v", r))
						}
					}()
					output, err := runClaude(p)
					if err != nil {
						if strings.Contains(err.Error(), "context deadline exceeded") {
							output = fmt.Sprintf("⏱️ Timeout (10min)\n\n%s", output)
						} else {
							output = fmt.Sprintf("⚠️ %s\n\nExit: %v", output, err)
						}
					}
					sendMessage(config, cid, 0, output)
				}(prompt, chatID)
			}
		}
	}
}

func printHelp() {
	fmt.Printf(`ccc - Claude Code Companion v%s

Your companion for Claude Code - control sessions remotely via Telegram and tmux.

USAGE:
    ccc                     Start/attach tmux session in current directory
    ccc -c                  Continue previous session
    ccc <message>           Send notification (if away mode is on)

COMMANDS:
    setup <token>           Complete setup (bot, hook, service - all in one!)
    doctor                  Check all dependencies and configuration
    config                  Show/set configuration values
    config projects-dir <path>  Set base directory for projects
    config oauth-token <token>  Set OAuth token
    setgroup                Configure Telegram group for topics (if skipped during setup)
    listen                  Start the Telegram bot listener manually
    install                 Install Claude hook manually
    send <file>             Send file to current session's Telegram topic
    relay [port]            Start relay server for large files (default: 8080)
    run                     Run Claude directly (used by tmux sessions)

TELEGRAM COMMANDS:
    /new <name>             Create new session with topic (in projects_dir)
    /new ~/path/name        Create session with custom path
    /new                    Restart session in current topic
    /continue               Restart session keeping conversation history
    /c <cmd>                Execute shell command
    /update                 Update ccc binary from GitHub
    /restart                Restart ccc service

OTP (permission approval):
    When OTP is enabled (via 'ccc setup'), Claude's permission requests
    are forwarded to Telegram. Reply with your OTP code to approve.

FLAGS:
    -h, --help              Show this help
    -v, --version           Show version

For more info: https://github.com/kidandcat/ccc
`, version)
}

const authTmuxSession = "claude-auth"

func handleAuth(config *Config, chatID, threadID int64) {
	if !authInProgress.TryLock() {
		sendMessage(config, chatID, threadID, "⚠️ Auth already in progress")
		return
	}

	sendMessage(config, chatID, threadID, "🔐 Starting Claude auth...")

	killTmuxSession(authTmuxSession)
	time.Sleep(500 * time.Millisecond)

	home, _ := os.UserHomeDir()
	if err := exec.Command(tmuxPath, "new-session", "-d", "-s", authTmuxSession, "-c", home).Run(); err != nil {
		sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Failed to create tmux session: %v", err))
		authInProgress.Unlock()
		return
	}

	time.Sleep(500 * time.Millisecond)
	exec.Command(tmuxPath, "send-keys", "-t", authTmuxSession, claudePath+" --dangerously-skip-permissions", "C-m").Run()

	var oauthURL string
	for i := 0; i < 30; i++ {
		time.Sleep(500 * time.Millisecond)
		out, err := exec.Command(tmuxPath, "capture-pane", "-t", authTmuxSession, "-p", "-S", "-30").Output()
		if err != nil {
			continue
		}
		pane := string(out)

		if strings.Contains(pane, "Dark mode") || strings.Contains(pane, "❯") || strings.Contains(pane, "Welcome back") {
			sendMessage(config, chatID, threadID, "✅ Claude is already authenticated!")
			killTmuxSession(authTmuxSession)
			authInProgress.Unlock()
			return
		}

		if strings.Contains(pane, "claude.ai/oauth/authorize") {
			lines := strings.Split(pane, "\n")
			capturing := false
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "https://claude.ai/oauth/") {
					oauthURL = line
					capturing = true
				} else if capturing && line != "" && !strings.Contains(line, "Paste code") && !strings.Contains(line, "Browser") {
					oauthURL += line
				} else if capturing {
					capturing = false
				}
			}
			break
		}
	}

	if oauthURL == "" {
		sendMessage(config, chatID, threadID, "❌ Could not find OAuth URL. Try again.")
		killTmuxSession(authTmuxSession)
		authInProgress.Unlock()
		return
	}

	authWaitingCode = true
	sendMessage(config, chatID, threadID, fmt.Sprintf("🔗 Open this URL and authorize:\n\n%s\n\nThen paste the code here.", oauthURL))
}

func handleAuthCode(config *Config, chatID, threadID int64, code string) {
	authWaitingCode = false
	code = strings.TrimSpace(code)

	sendMessage(config, chatID, threadID, "🔄 Sending code to Claude...")

	exec.Command(tmuxPath, "send-keys", "-t", authTmuxSession, "-l", code).Run()
	time.Sleep(200 * time.Millisecond)
	exec.Command(tmuxPath, "send-keys", "-t", authTmuxSession, "C-m").Run()

	for i := 0; i < 10; i++ {
		time.Sleep(2 * time.Second)
		out, _ := exec.Command(tmuxPath, "capture-pane", "-t", authTmuxSession, "-p").Output()
		pane := string(out)

		if strings.Contains(pane, "Yes, I accept") {
			exec.Command(tmuxPath, "send-keys", "-t", authTmuxSession, "Down").Run()
			time.Sleep(200 * time.Millisecond)
			exec.Command(tmuxPath, "send-keys", "-t", authTmuxSession, "C-m").Run()
			continue
		}

		if strings.Contains(pane, "Press Enter") || strings.Contains(pane, "Enter to confirm") {
			exec.Command(tmuxPath, "send-keys", "-t", authTmuxSession, "C-m").Run()
			continue
		}

		if strings.Contains(pane, "❯") {
			sendMessage(config, chatID, threadID, "✅ Auth successful! Claude is ready.")
			killTmuxSession(authTmuxSession)
			authInProgress.Unlock()
			return
		}
	}

	out, _ := exec.Command(tmuxPath, "capture-pane", "-t", authTmuxSession, "-p").Output()
	pane := string(out)
	if strings.Contains(pane, "Login successful") || strings.Contains(pane, "❯") {
		sendMessage(config, chatID, threadID, "✅ Auth successful!")
	} else {
		sendMessage(config, chatID, threadID, "⚠️ Auth may have failed. Check VPS manually.")
	}

	killTmuxSession(authTmuxSession)
	authInProgress.Unlock()
}
