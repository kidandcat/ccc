package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// enginelogin.go drives `grok login` and `agy auth login` on a PTY the same
// way ptyflow.go drives `claude auth login`. The binaries were not on the
// agent VM; phrases below are the documented / typical device-auth prompts
// (URL + optional user code). Success is never inferred from the TUI: a
// login is confirmed by the credential file on disk (profileLoggedIn).

// grokDeviceCodeRe matches the XXXX-XXXX user code device-auth often prints.
var grokDeviceCodeRe = regexp.MustCompile(`\b[A-Z0-9]{4}-[A-Z0-9]{4}\b`)

func loginCommandLabel(p Profile) string {
	switch profileEngine(p) {
	case engineGrok:
		return "<code>grok login --device-auth</code>"
	case engineAntigravity:
		return "<code>agy auth login</code>"
	default:
		return "<code>claude auth login</code>"
	}
}

// runAccountLogin dispatches the PTY login for this account's engine.
func runAccountLogin(ctx context.Context, start ptyStarter, p Profile, prompter loginPrompter) (string, error) {
	switch profileEngine(p) {
	case engineGrok, engineAntigravity:
		return runIsolatedLoginFlow(ctx, start, p, prompter)
	default:
		return runLoginFlow(ctx, start, p, prompter)
	}
}

// engineLoginEnv is engineEnv plus browser suppression, so a login ccc drives
// never opens a browser on the machine it runs on.
func engineLoginEnv(p Profile) ([]string, error) {
	shim, err := noBrowserDir()
	if err != nil {
		return nil, err
	}
	env := engineEnv(nil, profileEngine(p), p)
	out := make([]string, 0, len(env)+2)
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
	out = append(out, "TERM=xterm-256color")
	return out, nil
}

// runIsolatedLoginFlow starts grok/agy login under the account's isolated
// home, posts any URL (and device code) to Telegram, optionally feeds a
// pasted code into the PTY, and waits until profileLoggedIn is true.
func runIsolatedLoginFlow(ctx context.Context, start ptyStarter, p Profile, prompter loginPrompter) (string, error) {
	env, err := engineLoginEnv(p)
	if err != nil {
		return "", err
	}
	home := engineHome(p)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", fmt.Errorf("create the account's config dir: %w", err)
	}
	if profileEngine(p) == engineAntigravity {
		if err := os.MkdirAll(filepath.Join(home, ".gemini", "antigravity-cli"), 0o700); err != nil {
			return "", fmt.Errorf("create the isolated gemini dir: %w", err)
		}
	}

	bin, args, err := loginBinArgs(p)
	if err != nil {
		return "", err
	}
	s, err := start(home, env, bin, args...)
	if err != nil {
		return "", err
	}
	defer s.Close()

	prompter.Progress("waiting for a login URL")
	_, _ = s.waitFor(ctx, []string{"https://", "visit", "device", "code", "login", "auth"}, 90*time.Second) // safe-ignore: we still scrape the screen for a URL even if none of the phrases matched

	url := oauthURLRe.FindString(s.screen())
	userCode := grokDeviceCodeRe.FindString(normalizePTY(s.screen()))
	if url != "" {
		shown := url
		if userCode != "" {
			shown = url + "\n\nDevice code: " + userCode
		}
		// AskForCode blocks on the owner's next message. Device-auth often
		// finishes in the browser with no code to paste, so we also poll the
		// credential file and treat a Telegram message as optional PTY input.
		codeCh := make(chan string, 1)
		go func() {
			code, err := prompter.AskForCode(ctx, shown)
			if err != nil || strings.TrimSpace(code) == "" {
				return
			}
			select {
			case codeCh <- strings.TrimSpace(code):
			case <-ctx.Done():
			}
		}()
		return waitLoggedIn(ctx, s, p, codeCh)
	}

	// No URL: still poll — the CLI may already have credentials, or it may
	// print the URL later.
	return waitLoggedIn(ctx, s, p, nil)
}

func loginBinArgs(p Profile) (string, []string, error) {
	switch profileEngine(p) {
	case engineGrok:
		bin, err := resolveEngineBin(engineGrok)
		return bin, []string{"login", "--device-auth"}, err
	case engineAntigravity:
		bin, err := resolveEngineBin(engineAntigravity)
		return bin, []string{"auth", "login"}, err
	default:
		return "", nil, errors.New("not an isolated-engine login")
	}
}

func waitLoggedIn(ctx context.Context, s *ptySession, p Profile, codeCh <-chan string) (string, error) {
	deadline := time.Now().Add(9 * time.Minute)
	for {
		loggedIn, account, statusErr := profileLoggedIn(p)
		if loggedIn {
			if account == "" {
				account = accountDisplay(p)
			}
			return account, nil
		}
		if time.Now().After(deadline) {
			if statusErr != nil {
				return "", fmt.Errorf("login did not complete: %w", statusErr)
			}
			return "", errors.New("login did not complete (no credentials written)")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case code, ok := <-codeCh:
			if ok && code != "" && s != nil {
				_ = s.send(code + "\r") // safe-ignore: the next poll is what confirms login; a write error just delays it
			}
		case <-time.After(2 * time.Second):
		}
	}
}
