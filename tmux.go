package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const defaultTmuxSession = "ccc"

var (
	tmuxPath   string
	cccPath    string
	claudePath string
)

func initPaths() {
	// Find tmux binary
	if path, err := exec.LookPath("tmux"); err == nil {
		tmuxPath = path
	} else {
		// Fallback paths for common installations
		for _, p := range []string{"/opt/homebrew/bin/tmux", "/usr/local/bin/tmux", "/usr/bin/tmux"} {
			if _, err := os.Stat(p); err == nil {
				tmuxPath = p
				break
			}
		}
	}

	// Find ccc binary - prefer ~/bin/ccc (canonical install path),
	// then PATH, then current executable as last resort
	home, _ := os.UserHomeDir()
	binCcc := home + "/bin/ccc"
	if _, err := os.Stat(binCcc); err == nil {
		cccPath = binCcc
	} else if path, err := exec.LookPath("ccc"); err == nil {
		cccPath = path
	} else if exe, err := os.Executable(); err == nil {
		cccPath = exe
	}

	// Find claude binary - first try PATH, then fallback paths
	if path, err := exec.LookPath("claude"); err == nil {
		claudePath = path
	} else {
		home, _ := os.UserHomeDir()
		claudePaths := []string{
			home + "/.local/bin/claude",
			"/usr/local/bin/claude",
		}
		for _, p := range claudePaths {
			if _, err := os.Stat(p); err == nil {
				claudePath = p
				break
			}
		}
	}
}

// getTargetSession returns an existing tmux session name, or creates one if none exist
func getTargetSession() (string, error) {
	// Try to find any existing session
	cmd := exec.Command(tmuxPath, "list-sessions", "-F", "#{session_name}")
	out, err := cmd.Output()
	if err == nil {
		scanner := bufio.NewScanner(bytes.NewReader(out))
		for scanner.Scan() {
			name := scanner.Text()
			if name != "" {
				return name, nil
			}
		}
	}
	// No sessions exist, create one
	c := exec.Command(tmuxPath, "new-session", "-d", "-s", defaultTmuxSession)
	if err := c.Run(); err != nil {
		return "", err
	}
	exec.Command(tmuxPath, "set-option", "-t", defaultTmuxSession, "mouse", "on").Run()
	return defaultTmuxSession, nil
}

// tmuxTargetByID returns the window ID if available, otherwise falls back to name lookup
func tmuxTargetByID(windowID string, windowName string) string {
	if windowID != "" {
		return windowID
	}
	return tmuxTargetByName(windowName)
}

// tmuxTargetByName finds a window target by name (fallback)
func tmuxTargetByName(windowName string) string {
	cmd := exec.Command(tmuxPath, "list-windows", "-a", "-F", "#{window_id}\t#{window_name}")
	out, err := cmd.Output()
	if err == nil {
		scanner := bufio.NewScanner(bytes.NewReader(out))
		for scanner.Scan() {
			parts := strings.SplitN(scanner.Text(), "\t", 2)
			if len(parts) == 2 && parts[1] == windowName {
				return parts[0] // return window ID
			}
		}
	}
	return defaultTmuxSession + ":" + windowName
}

func tmuxWindowExistsByID(windowID string, windowName string) bool {
	if windowID != "" {
		// Check by ID directly
		cmd := exec.Command(tmuxPath, "list-windows", "-a", "-F", "#{window_id}")
		out, err := cmd.Output()
		if err != nil {
			return false
		}
		scanner := bufio.NewScanner(bytes.NewReader(out))
		for scanner.Scan() {
			if scanner.Text() == windowID {
				return true
			}
		}
		return false
	}
	// Fallback: search by name
	cmd := exec.Command(tmuxPath, "list-windows", "-a", "-F", "#{window_name}")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		if scanner.Text() == windowName {
			return true
		}
	}
	return false
}

func createTmuxWindow(windowName string, workDir string, continueSession bool) (string, error) {
	// Build the command to run inside the window
	cccCmd := cccPath + " run"
	if continueSession {
		cccCmd += " -c"
	}

	// Get an existing session or create one
	sess, err := getTargetSession()
	if err != nil {
		return "", err
	}

	// Create new window, -P -F prints the window ID
	args := []string{"new-window", "-P", "-F", "#{window_id}", "-t", sess + ":", "-n", windowName, "-c", workDir}
	cmd := exec.Command(tmuxPath, args...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	windowID := strings.TrimSpace(string(out))

	// Send the command to the window via send-keys using window ID
	time.Sleep(200 * time.Millisecond)
	tmuxSendKeys(windowID, cccCmd, "C-m")

	return windowID, nil
}

// runClaudeRaw runs claude directly (used inside tmux sessions)
func runClaudeRaw(continueSession bool) error {
	if claudePath == "" {
		return fmt.Errorf("claude binary not found")
	}

	// Clean stale Telegram flag from previous sessions.
	// Use window_name to identify the session
	if winName, err := exec.Command(tmuxPath, "display-message", "-p", "#{window_name}").Output(); err == nil {
		name := strings.TrimSpace(string(winName))
		if name != "" {
			os.Remove(telegramActiveFlag(name))
		}
	}

	var args []string
	// Auto-approve mode: skip permissions when no OTP is configured
	if config, err := loadConfig(); err == nil && config.OTPSecret == "" {
		args = append(args, "--dangerously-skip-permissions")
	}
	if continueSession {
		args = append(args, "-c")
	}

	cmd := exec.Command(claudePath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Ensure OAuth token is available from config if not already in environment
	if os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") == "" {
		if config, err := loadConfig(); err == nil && config.OAuthToken != "" {
			cmd.Env = append(os.Environ(), "CLAUDE_CODE_OAUTH_TOKEN="+config.OAuthToken)
		}
	}

	return cmd.Run()
}

// waitForClaude polls the tmux pane until Claude Code's input prompt appears
func waitForClaude(target string, timeout time.Duration) error {
	// Poll faster for short timeouts (message sending), slower for startup
	interval := 100 * time.Millisecond
	if timeout > 10*time.Second {
		interval = 500 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cmd := exec.Command(tmuxPath, "capture-pane", "-t", target, "-p")
		out, err := cmd.Output()
		if err == nil {
			content := string(out)
			// Claude Code shows "❯" when ready for input
			if strings.Contains(content, "❯") {
				return nil
			}
		}
		time.Sleep(interval)
	}
	return fmt.Errorf("timeout waiting for Claude to start")
}

// sendToTmuxFromTelegram sets the Telegram active flag before sending,
// so the permission hook knows this input came from Telegram and requires OTP.
// windowNameFromTarget extracts the window name from a "session:window" target
func windowNameFromTarget(target string) string {
	if idx := strings.LastIndex(target, ":"); idx >= 0 {
		return target[idx+1:]
	}
	return target
}

func sendToTmuxFromTelegram(target string, windowName string, text string) error {
	os.WriteFile(telegramActiveFlag(windowName), []byte("1"), 0600)
	return sendToTmux(target, text)
}

func sendToTmuxFromTelegramWithDelay(target string, windowName string, text string, delay time.Duration) error {
	os.WriteFile(telegramActiveFlag(windowName), []byte("1"), 0600)
	return sendToTmuxWithDelay(target, text, delay)
}

// tmuxSendKeys sends control keys to a tmux target with timeout protection.
// Use for all send-keys calls to prevent indefinite hangs (tmux/tmux#1185).
func tmuxSendKeys(target string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmdArgs := append([]string{"send-keys", "-t", target}, args...)
	return exec.CommandContext(ctx, tmuxPath, cmdArgs...).Run()
}

// tmuxPasteText sends text to a tmux target using set-buffer + paste-buffer -p.
// Bracketed paste mode avoids the send-keys -l hang when the PTY buffer is full.
func tmuxPasteText(target string, text string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, tmuxPath, "set-buffer", "-b", "ccc-input", text).Run(); err != nil {
		return fmt.Errorf("set-buffer: %w", err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	if err := exec.CommandContext(ctx2, tmuxPath, "paste-buffer", "-b", "ccc-input", "-p", "-t", target).Run(); err != nil {
		return fmt.Errorf("paste-buffer: %w", err)
	}
	return nil
}

func sendToTmux(target string, text string) error {
	return sendToTmuxWithDelay(target, text, 0)
}

func sendToTmuxWithDelay(target string, text string, delay time.Duration) error {
	if err := tmuxPasteText(target, text); err != nil {
		return err
	}

	// Dismiss autocomplete popup that bracketed paste may trigger
	time.Sleep(100 * time.Millisecond)
	tmuxSendKeys(target, "Escape")

	// Submit with double Enter (Claude Code needs double Enter)
	time.Sleep(50 * time.Millisecond)
	tmuxSendKeys(target, "Enter")
	time.Sleep(50 * time.Millisecond)
	tmuxSendKeys(target, "Enter")

	return nil
}

func killTmuxWindow(windowID string, windowName string) error {
	target := tmuxTargetByID(windowID, windowName)
	cmd := exec.Command(tmuxPath, "kill-window", "-t", target)
	return cmd.Run()
}

func listTmuxWindows() ([]string, error) {
	cmd := exec.Command(tmuxPath, "list-windows", "-a", "-F", "#{window_name}")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var windows []string
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		name := scanner.Text()
		windows = append(windows, name)
	}
	return windows, nil
}

// killTmuxSession kills an entire tmux session (used for temporary sessions like auth)
func killTmuxSession(name string) error {
	cmd := exec.Command(tmuxPath, "kill-session", "-t", name)
	return cmd.Run()
}
