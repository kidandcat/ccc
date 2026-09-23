package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
)

// ptyflow.go drives `claude` through a pseudo-terminal so the owner can log an
// account in from Telegram, with no shell anywhere (DESIGN §8 "Account
// management from Telegram"). Every string and pattern Claude Code's TUI is
// matched against lives in this file, with a note on how it was verified —
// they are the only part of ccc that depends on the CLI's human output, so
// when a Claude Code release breaks the flow, this is the file to re-verify.
//
// ---------------------------------------------------------------------------
// How the strings below were verified (Claude Code 2.1.270, ARM macOS)
// ---------------------------------------------------------------------------
//
// A throwaway CLAUDE_CONFIG_DIR was created under /private/tmp, `claude` was
// started in a PTY with that dir, the raw bytes were captured until the prompt
// appeared, the process was killed and the directory deleted. No code was ever
// entered, no login completed, and ~/.claude / the Keychain were never touched.
//
// `claude auth login` writes, verbatim (\a is BEL, the OSC-8 hyperlink
// terminator; the URL appears twice, once as the link target and once as the
// visible text):
//
//	Opening browser to sign in…\r\n
//	If the browser didn't open, visit: \x1b]8;;<URL>\a\x1b[94m<URL>\x1b[39m\x1b]8;;\a\r\n
//	Paste code here if prompted >
//
// with <URL> = https://claude.com/cai/oauth/authorize?<query redacted>.
//
// A brand-new config dir shows a first-run theme picker before any of that;
// it was captured the same way:
//
//	Choose the text style that looks best with your terminal
//	❯ 2. Dark mode ✔
//
// The driver never assumes an option's position: it reads the numbered list off
// the screen and answers with the number next to the label it wants
// (selectNumberFor), which is what keeps it working when Claude Code reorders
// its menus.
//
// The bypass-permissions disclaimer is NOT driven here. It is a settings.json
// key ccc writes directly (acceptBypassDisclaimer in profiles.go, DESIGN
// §14.23); only the login needs a terminal.
//
// Finally, success is never inferred from the TUI: a login is confirmed with
// `claude auth status --json`, which reads real state on disk.

// Verified prompt fragments. Matched against the screen after normalizePTY, so
// they must be written the way they READ, not the way they are drawn.
const (
	// ptyLoginURLPrompt precedes the OAuth URL in `claude auth login`.
	ptyLoginURLPrompt = "If the browser didn't open, visit:"
	// ptyCodePrompt is the line that waits for the code pasted back.
	ptyCodePrompt = "Paste code here if prompted"
	// ptyThemePrompt is the first-run theme picker a fresh config dir shows.
	ptyThemePrompt = "Choose the text style"
	// ptyThemeAnswer is the option the driver picks: any is fine, ccc never
	// reads colours back, so the default dark theme is chosen for determinism.
	ptyThemeAnswer = "Dark mode"
	// ptyLoginSuccess is a best-effort progress hint only; the authority is
	// `claude auth status --json`.
	ptyLoginSuccess = "Login successful"
)

// ptyLoginTimeout is how long the whole login flow may take, including the
// human walking to their phone (DESIGN §8: 10 minutes).
const ptyLoginTimeout = 10 * time.Minute

// oauthURLRe extracts the login URL. It stops at whitespace, BEL and ESC so the
// OSC-8 wrapper around it never becomes part of the URL.
var oauthURLRe = regexp.MustCompile(`https://[^\s\x07\x1b"']+`)

// ANSI/OSC strippers. Claude Code positions text with CSI sequences instead of
// spaces (it writes "Welcome\x1b[9Gto"), so every sequence becomes a single
// space rather than being deleted — otherwise words run together and no phrase
// above would ever match.
var (
	ptyCSIRe   = regexp.MustCompile(`\x1b\[[0-9;?>]*[a-zA-Z]`)
	ptyOSCRe   = regexp.MustCompile(`\x1b\][^\x07\x1b]*(\x07|\x1b\\)`)
	ptyOtherRe = regexp.MustCompile(`\x1b[()][0-9A-Za-z]|\x1b[78=>]`)
)

// normalizePTY turns raw PTY bytes into the text a human would read.
func normalizePTY(s string) string {
	s = ptyOSCRe.ReplaceAllString(s, " ")
	s = ptyCSIRe.ReplaceAllString(s, " ")
	s = ptyOtherRe.ReplaceAllString(s, " ")
	return s
}

// flatten collapses the screen to one whitespace-normalised line, which is what
// the phrase constants are matched against (a phrase can be split across a
// line wrap or a cursor jump).
func flatten(s string) string { return strings.Join(strings.Fields(normalizePTY(s)), " ") }

// selectNumberRe matches one option of a Claude Code select list, e.g.
// "❯ 2. Dark mode ✔" or "  3. Light mode".
var selectNumberRe = regexp.MustCompile(`(?m)^\s*[^\d\n]{0,4}?(\d+)\s*\.\s+(.*)$`)

// selectNumberFor finds the number to type for the option whose label contains
// want. Reading the number off the screen is what makes the driver safe against
// Claude Code reordering its options between releases.
func selectNumberFor(screen, want string) (string, bool) {
	for _, m := range selectNumberRe.FindAllStringSubmatch(normalizePTY(screen), -1) {
		label := strings.Join(strings.Fields(m[2]), " ")
		if strings.Contains(label, want) {
			return m[1], true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// The driver
// ---------------------------------------------------------------------------

// ptySession is one `claude` process attached to a pseudo-terminal, with its
// output accumulated so the flow can wait for phrases in it.
type ptySession struct {
	cmd  *exec.Cmd
	tty  *os.File
	mu   sync.Mutex
	buf  strings.Builder
	done chan struct{}
}

// ptyStarter builds a session. The real one is startPTY; the tests swap in a
// fake script instead of the claude binary.
type ptyStarter func(dir string, env []string, name string, args ...string) (*ptySession, error)

// startPTY runs a command on a PTY of a fixed size. The size matters: Claude
// Code lays its TUI out to the terminal width, and an 80-column terminal wraps
// the OAuth URL across lines.
func startPTY(dir string, env []string, name string, args ...string) (*ptySession, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 60, Cols: 200})
	if err != nil {
		return nil, fmt.Errorf("start %s on a pty: %w", filepath.Base(name), err)
	}
	s := &ptySession{cmd: cmd, tty: tty, done: make(chan struct{})}
	go s.pump()
	return s, nil
}

func (s *ptySession) pump() {
	defer close(s.done)
	b := make([]byte, 4096)
	for {
		n, err := s.tty.Read(b)
		if n > 0 {
			s.mu.Lock()
			s.buf.Write(b[:n])
			s.mu.Unlock()
		}
		if err != nil {
			return // EOF (the child exited) or the tty was closed by Close()
		}
	}
}

// screen returns everything the child has written so far.
func (s *ptySession) screen() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// send writes to the child's terminal.
func (s *ptySession) send(text string) error {
	_, err := io.WriteString(s.tty, text)
	return err
}

// Close kills the child and releases the pty. Wait reaps it; Kill alone
// leaves a zombie for the life of listen.
func (s *ptySession) Close() {
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill() // safe-ignore: the child is being abandoned; a dead process is the goal
	}
	if s.tty != nil {
		_ = s.tty.Close() // safe-ignore: same
	}
	if s.done != nil {
		<-s.done
	}
	if s.cmd != nil {
		_ = s.cmd.Wait() // safe-ignore: Wait reaps; the error is the kill we just sent
	}
}

// waitFor blocks until the flattened screen contains one of the phrases, the
// child exits, or ctx is done. It returns the phrase that matched.
func (s *ptySession) waitFor(ctx context.Context, phrases []string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		screen := flatten(s.screen())
		for _, p := range phrases {
			if strings.Contains(screen, p) {
				return p, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-s.done:
			// One last look: the phrase may have arrived in the final write.
			screen = flatten(s.screen())
			for _, p := range phrases {
				if strings.Contains(screen, p) {
					return p, nil
				}
			}
			return "", fmt.Errorf("claude exited before printing any of %v", phrases)
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timed out waiting for %v", phrases)
		}
	}
}

// answerSelect answers a numbered select list by label.
func (s *ptySession) answerSelect(want string) error {
	n, ok := selectNumberFor(s.screen(), want)
	if !ok {
		return fmt.Errorf("could not find the option %q on screen", want)
	}
	if err := s.send(n); err != nil {
		return err
	}
	time.Sleep(150 * time.Millisecond)
	return s.send("\r")
}

// ---------------------------------------------------------------------------
// Browser suppression
// ---------------------------------------------------------------------------

// noBrowserDir builds a directory of no-op `open`/`xdg-open` shims and returns
// it, so a `claude auth login` ccc drives NEVER opens a browser on the machine
// ccc runs on. That matters twice over: the VM is headless (an attempted
// launch is noise at best), and on the Mac it would hijack the owner's browser
// with a callback URL aimed at a localhost port nothing is listening on. The
// URL is for Telegram and nowhere else.
func noBrowserDir() (string, error) {
	dir := filepath.Join(cacheDir(), "no-browser")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	for _, name := range []string{"open", "xdg-open", "x-www-browser", "www-browser", "sensible-browser"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// loginEnv is claudeEnv plus the browser suppression: the shim directory first
// on PATH (Claude Code shells out to `open` / `xdg-open`) and BROWSER pointed
// at the same no-op (it honours $BROWSER before either).
func loginEnv(p Profile) ([]string, error) {
	shim, err := noBrowserDir()
	if err != nil {
		return nil, err
	}
	env := claudeEnv(p)
	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			out = append(out, "PATH="+shim+string(os.PathListSeparator)+strings.TrimPrefix(kv, "PATH="))
			replaced = true
			continue
		}
		out = append(out, kv)
	}
	if !replaced {
		out = append(out, "PATH="+shim)
	}
	out = append(out, "BROWSER="+filepath.Join(shim, "open"))
	// A TERM is required or the TUI refuses to draw; "dumb" makes it fall back
	// to a plain renderer whose output is harder to parse, so ask for a real one.
	out = append(out, "TERM=xterm-256color")
	return out, nil
}

// ---------------------------------------------------------------------------
// Flows
// ---------------------------------------------------------------------------

// loginPrompter is how the flow talks to whoever is driving it: it is handed
// the URL to show, and must come back with the code the user pasted (or an
// error/timeout). The Telegram implementation posts the URL in the chat and
// waits for the next message there.
type loginPrompter interface {
	AskForCode(ctx context.Context, url string) (string, error)
	Progress(text string)
	// ShowDeviceAuth posts a URL + one-time code the owner types on the
	// website (RFC 8628 / Codex). It must not wait for a Telegram paste:
	// that code never comes back to the CLI.
	ShowDeviceAuth(url, userCode string)
}

// errLoginCancelled is returned when nobody answered with a code in time.
var errLoginCancelled = errors.New("login cancelled")

// runLoginFlow drives `claude auth login` end to end and returns the account it
// ended up logged in as. start is injectable so the tests can drive a fake
// script that prints the same strings.
func runLoginFlow(ctx context.Context, start ptyStarter, p Profile, prompter loginPrompter) (string, error) {
	env, err := loginEnv(p)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(claudeHome(p), 0o700); err != nil {
		return "", fmt.Errorf("create the profile's config dir: %w", err)
	}
	s, err := start(claudeHome(p), env, claudeBin(), "auth", "login")
	if err != nil {
		return "", err
	}
	defer s.Close()

	// A brand-new config dir may ask for a theme before anything else.
	phrase, err := s.waitFor(ctx, []string{ptyLoginURLPrompt, ptyThemePrompt}, 90*time.Second)
	if err != nil {
		return "", fmt.Errorf("claude auth login did not get going: %w", err)
	}
	if phrase == ptyThemePrompt {
		if err := s.answerSelect(ptyThemeAnswer); err != nil {
			return "", err
		}
		if _, err := s.waitFor(ctx, []string{ptyLoginURLPrompt}, 60*time.Second); err != nil {
			return "", fmt.Errorf("no login URL after the theme picker: %w", err)
		}
	}

	url := oauthURLRe.FindString(s.screen())
	if url == "" {
		return "", errors.New("claude printed no login URL")
	}
	// The code prompt follows the URL immediately. Waiting for it is not
	// required — the child buffers whatever is typed — but it catches a claude
	// that printed a URL and then died before it could read anything.
	if _, err := s.waitFor(ctx, []string{ptyCodePrompt}, 20*time.Second); err != nil {
		return "", fmt.Errorf("claude never asked for the code: %w", err)
	}
	prompter.Progress("waiting for the code")

	code, err := prompter.AskForCode(ctx, url)
	if err != nil {
		return "", err
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return "", errLoginCancelled
	}
	if err := s.send(code + "\r"); err != nil {
		return "", fmt.Errorf("write the code to claude: %w", err)
	}

	// The TUI's own "Login successful" is a hint, not proof; `auth status`
	// reads the credentials that were actually written.
	_, _ = s.waitFor(ctx, []string{ptyLoginSuccess}, 45*time.Second) // safe-ignore: the verification below is the authority
	deadline := time.Now().Add(60 * time.Second)
	for {
		loggedIn, account, statusErr := profileLoggedIn(p)
		if loggedIn {
			return account, nil
		}
		if time.Now().After(deadline) {
			if statusErr != nil {
				return "", fmt.Errorf("the code was not accepted: %w", statusErr)
			}
			return "", errors.New("the code was not accepted (claude auth status still reports logged out)")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
