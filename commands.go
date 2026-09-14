package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// CLI surface of ccc v3: bootstrap (`setup`, `setgroup`, `config`), diagnostics
// (`doctor`) and the service control the installer needs. Everything the bots
// do at runtime lives in listenv3.go / runner.go.

// listenLog writes timestamped log entries to ccc.log AND stdout, so the logs
// survive whichever way the process was started.
var listenLogFile *os.File

func listenLog(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	line := fmt.Sprintf("[%s] [pid:%d] %s\n", time.Now().Format("2006-01-02 15:04:05"), os.Getpid(), msg)
	fmt.Print(line)
	if listenLogFile != nil {
		listenLogFile.WriteString(line) // safe-ignore: a failed log write must not take the listener down
	}
}

func initListenLog() {
	logPath := filepath.Join(cacheDir(), "ccc.log")
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		listenLogFile = f
	}
}

// ---------------------------------------------------------------------------
// Service control
// ---------------------------------------------------------------------------

func stopListenerService() {
	home, err := os.UserHomeDir()
	if err == nil {
		if _, err := os.Stat("/Library"); err == nil {
			plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.ccc.plist")
			exec.Command("launchctl", "unload", plistPath).Run() // safe-ignore: "not loaded" is the normal case
		} else {
			exec.Command("systemctl", "--user", "stop", "ccc").Run() // safe-ignore: same
		}
	}
	// Also kill any manual listener via the lock file's pid.
	lockPath := filepath.Join(cacheDir(), "ccc.lock")
	if data, err := os.ReadFile(lockPath); err == nil {
		if pid := strings.TrimSpace(string(data)); pid != "" {
			exec.Command("kill", pid).Run() // safe-ignore: the pid may already be gone
		}
	}
	time.Sleep(500 * time.Millisecond)
}

func startListenerService() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	if _, err := os.Stat("/Library"); err == nil {
		plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.ccc.plist")
		exec.Command("launchctl", "load", plistPath).Run() // safe-ignore: best effort; `ccc doctor` reports the real state
		return
	}
	exec.Command("systemctl", "--user", "start", "ccc").Run() // safe-ignore: same
}

// ---------------------------------------------------------------------------
// Bootstrap
// ---------------------------------------------------------------------------

// setup is the interactive bootstrap. On a headless box use the non-interactive
// path instead: `ccc config set bot_token|chat_id|group_id …` plus `/setgroup`
// from Telegram (README "Bootstrap on a VM").
func setup(botToken string) error {
	fmt.Println("🚀 ccc setup")
	fmt.Println("============")
	fmt.Println()

	config, err := loadConfig()
	if err != nil || config == nil {
		config = &Config{}
	}
	config.BotToken = botToken

	// A running listener would eat the updates this loop is waiting for (and
	// Telegram answers the second getUpdates with 409 Conflict).
	fmt.Println("Stopping the listener...")
	stopListenerService()

	fmt.Println("Step 1/3: send any message to your bot in Telegram...")
	offset := 0
	client := &http.Client{Timeout: 35 * time.Second}
	for config.ChatID == 0 {
		updates, next, err := pollUpdates(client, botToken, offset, 30)
		if err != nil {
			return err
		}
		offset = next
		for _, u := range updates {
			if u.Message.From.ID != 0 {
				config.ChatID = u.Message.From.ID
				if err := saveConfig(config); err != nil {
					return fmt.Errorf("save config: %w", err)
				}
				fmt.Printf("✅ Owner: @%s (%d)\n\n", u.Message.From.Username, config.ChatID)
			}
		}
	}

	fmt.Println("Step 2/3: create a Telegram group with Topics enabled, add the bot as")
	fmt.Println("          an admin and send a message there (30 s, or skip with ctrl-c)...")
	deadline := time.Now().Add(30 * time.Second)
	for config.GroupID == 0 && time.Now().Before(deadline) {
		updates, next, err := pollUpdates(client, botToken, offset, 5)
		if err != nil {
			continue
		}
		offset = next
		for _, u := range updates {
			if u.Message.Chat.Type == "supergroup" && u.Message.From.ID == config.ChatID {
				config.GroupID = u.Message.Chat.ID
				if err := saveConfig(config); err != nil {
					return fmt.Errorf("save config: %w", err)
				}
				fmt.Printf("✅ Group: %d\n\n", config.GroupID)
			}
		}
	}
	if config.GroupID == 0 {
		fmt.Println("⏭️  Skipped — run `ccc setgroup`, or send /setgroup in the group later.")
		fmt.Println()
	}

	fmt.Println("Step 3/3: installing the background service...")
	if err := installService(); err != nil {
		fmt.Printf("⚠️  Service installation failed: %v\n", err)
		fmt.Println("   You can start it manually with: ccc listen")
	}

	fmt.Println()
	fmt.Println("✅ Setup complete. Send a message in the group's General topic to create")
	fmt.Println("   your first bot, or /account to add a Claude account.")
	startListenerService()
	return nil
}

// setGroup records the forum group from the next message the owner sends there.
func setGroup(config *Config) error {
	fmt.Println("Send a message in the group where you want your bots to live...")
	fmt.Println("(Topics must be enabled and the bot must be an admin.)")

	offset := 0
	client := &http.Client{Timeout: 35 * time.Second}
	for {
		updates, next, err := pollUpdates(client, config.BotToken, offset, 30)
		if err != nil {
			return err
		}
		offset = next
		for _, u := range updates {
			chat := u.Message.Chat
			if chat.Type == "supergroup" && u.Message.From.ID == config.ChatID {
				config.GroupID = chat.ID
				if err := saveConfig(config); err != nil {
					return err
				}
				fmt.Printf("✅ Group set: %d\n", chat.ID)
				return nil
			}
		}
	}
}

// pollUpdates is one getUpdates round trip; it returns the updates and the next
// offset. Shared by the two bootstrap loops above.
func pollUpdates(client *http.Client, token string, offset, timeout int) ([]telegramUpdateEntry, int, error) {
	reqURL := fmt.Sprintf("%s?offset=%d&timeout=%d", telegramURL(token, "getUpdates"), offset, timeout)
	resp, err := telegramClientGet(client, token, reqURL)
	if err != nil {
		return nil, offset, fmt.Errorf("telegram: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize)) // safe-ignore: a short read surfaces as a parse error below
	resp.Body.Close()                                                 // safe-ignore: nothing to do if closing a read body fails

	var updates TelegramUpdate
	if err := json.Unmarshal(body, &updates); err != nil {
		return nil, offset, fmt.Errorf("parse telegram response: %w", err)
	}
	if !updates.OK {
		return nil, offset, fmt.Errorf("telegram API error: %s (check the bot token)", updates.Description)
	}
	for _, u := range updates.Result {
		if u.UpdateID+1 > offset {
			offset = u.UpdateID + 1
		}
	}
	return updates.Result, offset, nil
}

// ---------------------------------------------------------------------------
// doctor
// ---------------------------------------------------------------------------

// doctor prints the health report. With fix set (`ccc doctor --fix`) it also
// repairs what it safely can: today that is the bypass-permissions disclaimer,
// which is one settings.json key per profile (acceptBypassDisclaimer).
func doctor(fix bool) {
	fmt.Println("🩺 ccc doctor")
	fmt.Println("=============")
	fmt.Println()

	allGood := true

	fmt.Print("claude............ ")
	if claudePath != "" {
		fmt.Printf("✅ %s\n", claudePath)
	} else {
		fmt.Println("❌ not found")
		fmt.Println("   Install: https://claude.com/claude-code")
		allGood = false
	}

	fmt.Print("grok.............. ")
	if p, err := resolveEngineBin(engineGrok); err == nil {
		fmt.Printf("✅ %s (optional; /engine grok)\n", p)
	} else {
		fmt.Println("— not found (optional; needed for grok bots)")
		fmt.Println("   Install Grok Build to ~/.grok/bin/grok, then: grok login")
		fmt.Println("   On a VM: grok login --device-auth")
	}

	fmt.Print("agy............... ")
	if p, err := resolveEngineBin(engineAntigravity); err == nil {
		fmt.Printf("✅ %s (optional; /engine antigravity)\n", p)
	} else {
		fmt.Println("— not found (optional; needed for antigravity bots)")
		fmt.Println("   Install Antigravity CLI to ~/.local/bin/agy, then authenticate with an interactive agy session")
	}

	if !doctorProfiles(fix) {
		allGood = false
	}

	fmt.Print("config............ ")
	config, err := loadConfig()
	if err != nil {
		fmt.Println("❌ not found")
		fmt.Println("   Run: ccc setup <bot_token>   (or: ccc config set bot_token <token>)")
		allGood = false
	} else {
		fmt.Printf("✅ %s\n", getConfigPath())
		for _, check := range []struct {
			label string
			ok    bool
			value string
			hint  string
		}{
			{"bot_token", config.BotToken != "", "configured", "ccc config set bot_token <token>"},
			{"chat_id", config.ChatID != 0, fmt.Sprint(config.ChatID), "ccc config set chat_id <your telegram user id>"},
			{"group_id", config.GroupID != 0, fmt.Sprint(config.GroupID), "ccc config set group_id <id>, or send /setgroup in the group"},
		} {
			fmt.Printf("  %-14s ", check.label)
			if check.ok {
				fmt.Printf("✅ %s\n", check.value)
				continue
			}
			fmt.Println("❌ missing")
			fmt.Printf("   %s\n", check.hint)
			allGood = false
		}
		fmt.Printf("  %-14s %s\n", "data_dir", dataDir(config))
		fmt.Printf("  %-14s %s\n", "model", firstNonEmpty(instanceModel(config), "(claude default)"))
	}

	fmt.Print("service........... ")
	home, homeErr := os.UserHomeDir()
	switch {
	case homeErr != nil:
		fmt.Println("⚠️  cannot resolve the home directory")
	case isMacOS():
		plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.ccc.plist")
		if _, err := os.Stat(plistPath); err != nil {
			fmt.Println("❌ not installed — run: ccc install")
			allGood = false
		} else if exec.Command("launchctl", "list", "com.ccc").Run() == nil {
			fmt.Println("✅ running (launchd)")
		} else {
			fmt.Println("⚠️  installed but not running — launchctl load ~/Library/LaunchAgents/com.ccc.plist")
		}
	default:
		out, err := exec.Command("systemctl", "--user", "is-active", "ccc").Output()
		if err == nil && strings.TrimSpace(string(out)) == "active" {
			fmt.Println("✅ running (systemd --user)")
		} else if _, err := os.Stat(filepath.Join(home, ".config", "systemd", "user", "ccc.service")); err == nil {
			fmt.Println("⚠️  installed but not running — systemctl --user start ccc")
		} else {
			fmt.Println("❌ not installed — run: ccc install")
			allGood = false
		}
	}

	doctorCheckWhisper()

	fmt.Println()
	if allGood {
		fmt.Println("✅ All checks passed!")
		return
	}
	if fix {
		fmt.Println("❌ Some issues are left. Fix them and run `ccc doctor` again.")
		return
	}
	fmt.Println("❌ Some issues found. Fix them and run `ccc doctor` again (`ccc doctor --fix` repairs what it can).")
}

// isMacOS distinguishes the launchd host from the systemd one. /Library only
// exists on macOS, which is the same probe the installer uses.
func isMacOS() bool {
	_, err := os.Stat("/Library")
	return err == nil
}

// ---------------------------------------------------------------------------
// Help
// ---------------------------------------------------------------------------

func printHelp() {
	fmt.Printf(`ccc - a team of Claude, Grok or Antigravity bots in one Telegram forum group (v%s)

USAGE:
    ccc listen              Run the instance (normally done by the service)

COMMANDS:
    setup <bot_token>       Interactive bootstrap (owner, group, service)
    config set <key> <val>  Non-interactive bootstrap; keys: bot_token, chat_id,
                            group_id, model, default_engine, data_dir, env_passthrough
    config get <key>        Show one value
    config                  Show the whole configuration
    setgroup                Record the forum group from your next message in it
    install                 Install the background service (launchd / systemd --user)
    env sync                Snapshot env_passthrough secrets into <config>/env
                            (run from a login shell: bash -lc 'ccc env sync')
    doctor [--fix]          Check dependencies and configuration; --fix also
                            records the bypass disclaimer for every account
    maintain                Run the daily growth-control job once, now
    profile <cmd>           Manage Claude accounts (list/add/remove/default/
                            login/accept-disclaimer)
    mcp --bot <id>          MCP server for one turn (spawned by Claude Code)
    send <file>             Send a file into the topic of the bot owning this directory
    relay [port]            Relay server for files over 50 MB (default port: 8080)

TELEGRAM (in the forum group):
    Text in General         Create a new bot from your message
    Text in a bot's topic   Talk to that bot
    /role /new /stop /cwd /engine /memory /forget /watches /schedules   per bot
    /bots /status /usage                                        anywhere
    /memory stats|restore <id>                                  memory upkeep
    /account /access /model /setgroup                           owner only

FLAGS:
    -h, --help              Show this help
    -v, --version           Show the version

For more: https://github.com/kidandcat/ccc
`, version)
}
