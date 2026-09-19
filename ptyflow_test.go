package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// capturedLoginScreen is the VERBATIM output of `claude auth login` 2.1.270 on
// a pty, with only the OAuth query string replaced. It is the regression test
// for normalizePTY and oauthURLRe: if a future Claude Code changes this, the
// login flow breaks, and this is where it shows.
const capturedLoginScreen = "Opening browser to sign in…\r\n" +
	"If the browser didn't open, visit: \x1b]8;;https://claude.com/cai/oauth/authorize?code=REDACTED\a" +
	"\x1b[94mhttps://claude.com/cai/oauth/authorize?code=REDACTED\x1b[39m\x1b]8;;\a\r\n" +
	"Paste code here if prompted > "

// capturedThemePicker is the VERBATIM first-run theme prompt, which draws its
// words with cursor-positioning escapes instead of spaces. It is why
// normalizePTY turns every escape into a space rather than deleting it.
const capturedThemePicker = "\x1b[2G\x1b[1mChoose\x1b[9Gthe\x1b[13Gtext\x1b[18Gstyle\x1b[24Gthat\x1b[29Glooks\x1b[35Gbest" +
	"\x1b[40Gwith\x1b[45Gyour\x1b[50Gterminal\x1b[22m\r\r\n" +
	"\x1b[4G\x1b[38;5;246m1.\x1b[7G\x1b[39mAuto\x1b[12G(match\x1b[19Gterminal)\r\r\n" +
	"\x1b[2G\x1b[38;5;153m❯\x1b[4G\x1b[38;5;246m2.\x1b[7G\x1b[38;5;114mDark\x1b[12Gmode\x1b[17G✔\x1b[39m\r\r\n" +
	"\x1b[4G\x1b[38;5;246m3.\x1b[7G\x1b[39mLight\x1b[13Gmode\r\r\n"

func TestNormalizePTYKeepsWordsApart(t *testing.T) {
	got := flatten(capturedThemePicker)
	if !strings.Contains(got, ptyThemePrompt) {
		t.Fatalf("the theme prompt is not readable after normalisation:\n%q", got)
	}
	if strings.Contains(got, "Choosethe") {
		t.Errorf("escapes were deleted instead of becoming spaces:\n%q", got)
	}
}

func TestLoginScreenYieldsTheURLAndPrompts(t *testing.T) {
	flat := flatten(capturedLoginScreen)
	if !strings.Contains(flat, ptyLoginURLPrompt) {
		t.Errorf("the URL prompt does not match:\n%q", flat)
	}
	if !strings.Contains(flat, ptyCodePrompt) {
		t.Errorf("the code prompt does not match:\n%q", flat)
	}
	url := oauthURLRe.FindString(capturedLoginScreen)
	want := "https://claude.com/cai/oauth/authorize?code=REDACTED"
	if url != want {
		t.Errorf("extracted URL = %q, want %q (the OSC-8 wrapper must not leak in)", url, want)
	}
}

func TestSelectNumberFor(t *testing.T) {
	if got, ok := selectNumberFor(capturedThemePicker, ptyThemeAnswer); !ok || got != "2" {
		t.Errorf("theme option = %q (%v), want 2", got, ok)
	}
	if got, ok := selectNumberFor(capturedThemePicker, "Light mode"); !ok || got != "3" {
		t.Errorf("light option = %q (%v), want 3", got, ok)
	}
	if _, ok := selectNumberFor(capturedThemePicker, "Nonexistent"); ok {
		t.Error("an absent label must not match")
	}

	// An option is found by its label wherever it sits, so a Claude Code
	// release that reorders a menu does not send the driver to the wrong entry.
	reordered := "  1. Light mode\n❯ 2. Auto (match terminal)\n  3. Dark mode\n"
	if got, ok := selectNumberFor(reordered, ptyThemeAnswer); !ok || got != "3" {
		t.Errorf("reordered theme option = %q (%v), want 3", got, ok)
	}
}

// ---------------------------------------------------------------------------
// The driver, against a fake script
// ---------------------------------------------------------------------------

// fakeClaudeScript writes a shell script that imitates `claude auth login`:
// the theme picker, then the real login strings, then it waits for a code and
// records what it was given.
func fakeClaudeScript(t *testing.T, codeFile string, withTheme bool) string {
	t.Helper()
	theme := ""
	if withTheme {
		theme = `
printf 'Choose the text style that looks best with your terminal\n'
printf '  1. Auto (match terminal)\n'
printf '\342\235\257 2. Dark mode\n'
printf '  3. Light mode\n'
read -r themechoice
printf 'theme %s\n' "$themechoice" >> "$OUT"
`
	}
	script := `#!/bin/sh
OUT="` + codeFile + `"
` + theme + `
printf 'Opening browser to sign in\342\200\246\n'
printf 'If the browser didn'"'"'t open, visit: \033]8;;https://claude.com/cai/oauth/authorize?x=1\a\033[94mhttps://claude.com/cai/oauth/authorize?x=1\033[39m\033]8;;\a\n'
printf 'Paste code here if prompted > '
read -r code
printf 'code %s\n' "$code" >> "$OUT"
printf '\nLogin successful\n'
sleep 0.2
`
	path := filepath.Join(t.TempDir(), "fake-claude.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestPTYDriverAgainstAFakeClaude exercises the whole driver — start on a pty,
// wait for a phrase, answer a numbered select, extract the URL, write the code
// back — without touching the real claude or any credentials.
func TestPTYDriverAgainstAFakeClaude(t *testing.T) {
	out := filepath.Join(t.TempDir(), "received.txt")
	script := fakeClaudeScript(t, out, true)

	s, err := startPTY(t.TempDir(), []string{"PATH=" + os.Getenv("PATH"), "TERM=xterm-256color"}, script)
	if err != nil {
		t.Fatalf("startPTY: %v", err)
	}
	defer s.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	phrase, err := s.waitFor(ctx, []string{ptyLoginURLPrompt, ptyThemePrompt}, 10*time.Second)
	if err != nil {
		t.Fatalf("waiting for the first prompt: %v\nscreen: %q", err, s.screen())
	}
	if phrase != ptyThemePrompt {
		t.Fatalf("first prompt = %q, want the theme picker", phrase)
	}
	if err := s.answerSelect(ptyThemeAnswer); err != nil {
		t.Fatalf("answerSelect: %v", err)
	}

	if _, err := s.waitFor(ctx, []string{ptyLoginURLPrompt}, 10*time.Second); err != nil {
		t.Fatalf("waiting for the URL: %v\nscreen: %q", err, s.screen())
	}
	url := oauthURLRe.FindString(s.screen())
	if !strings.HasPrefix(url, "https://claude.com/cai/oauth/authorize") {
		t.Fatalf("extracted URL = %q", url)
	}

	if _, err := s.waitFor(ctx, []string{ptyCodePrompt}, 10*time.Second); err != nil {
		t.Fatalf("waiting for the code prompt: %v", err)
	}
	if err := s.send("ABC123\r"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := s.waitFor(ctx, []string{ptyLoginSuccess}, 10*time.Second); err != nil {
		t.Fatalf("waiting for success: %v\nscreen: %q", err, s.screen())
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the fake claude recorded nothing: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "theme 2") {
		t.Errorf("the theme picker got %q, want the number of the Dark mode option", got)
	}
	if !strings.Contains(got, "code ABC123") {
		t.Errorf("the code did not reach the child: %q", got)
	}
}

// A child that dies without printing what we wait for must not hang the flow.
func TestWaitForGivesUpWhenTheChildExits(t *testing.T) {
	script := filepath.Join(t.TempDir(), "dies.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho nope\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := startPTY(t.TempDir(), []string{"PATH=" + os.Getenv("PATH")}, script)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	start := time.Now()
	if _, err := s.waitFor(t.Context(), []string{ptyLoginURLPrompt}, 10*time.Second); err == nil {
		t.Fatal("waitFor should have failed when the child exited")
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("waitFor took %v; it should notice the exit immediately", time.Since(start))
	}
}

// The PTY environment must make it impossible for a login ccc drives to open a
// browser on the host: the shims come first on PATH and $BROWSER points at one.
func TestLoginEnvSuppressesTheBrowser(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	env, err := loginEnv(Profile{Name: "test", ConfigDir: filepath.Join(t.TempDir(), "cfg")})
	if err != nil {
		t.Fatalf("loginEnv: %v", err)
	}

	var path, browser string
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "PATH="):
			path = strings.TrimPrefix(kv, "PATH=")
		case strings.HasPrefix(kv, "BROWSER="):
			browser = strings.TrimPrefix(kv, "BROWSER=")
		}
	}
	shim := filepath.Join(cacheDir(), "no-browser")
	if !strings.HasPrefix(path, shim+string(os.PathListSeparator)) && path != shim {
		t.Errorf("PATH does not start with the shim dir: %q", path)
	}
	if browser != filepath.Join(shim, "open") {
		t.Errorf("BROWSER = %q, want the no-op shim", browser)
	}

	// The shims exist, are executable and do nothing.
	for _, name := range []string{"open", "xdg-open"} {
		info, err := os.Stat(filepath.Join(shim, name))
		if err != nil {
			t.Fatalf("%s shim missing: %v", name, err)
		}
		if info.Mode().Perm()&0o100 == 0 {
			t.Errorf("%s shim is not executable (%v)", name, info.Mode())
		}
	}
	body, err := os.ReadFile(filepath.Join(shim, "open"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "exit 0") {
		t.Errorf("the open shim is not a no-op: %q", body)
	}
}

// The profile's own CLAUDE_CONFIG_DIR survives the browser suppression, and no
// inherited CLAUDE*/ANTHROPIC* variable sneaks in with it.
func TestLoginEnvKeepsTheProfileScoped(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("ANTHROPIC_BASE_URL", "https://leak.example")
	dir := filepath.Join(t.TempDir(), "cfg")

	env, err := loginEnv(Profile{Name: "work", ConfigDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, kv := range env {
		if kv == "CLAUDE_CONFIG_DIR="+dir {
			found = true
		}
		if strings.HasPrefix(kv, "CLAUDECODE=") || strings.HasPrefix(kv, "ANTHROPIC_") {
			t.Errorf("inherited %q leaked into the login environment", kv)
		}
	}
	if !found {
		t.Error("the profile's CLAUDE_CONFIG_DIR is missing")
	}
}
