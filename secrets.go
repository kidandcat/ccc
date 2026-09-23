package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

// secrets.go is the owner vault (DESIGN §16). Values live in a 0600 JSON file
// under the config dir — the same class of store as <config>/env. They are
// never returned to the model, never written to the topic/inbox, and never
// printed in logs. The happy path is blind inject: a run tool takes env-var
// name → secret name (or stdin_secret), ccc sets env/stdin on the child, and
// redacts the known values from captured output before the model sees it.
//
// Honest limit: a hostile `ps eww`, `set -x`, or a Bash `cat` of this file
// (bots run as the owner) can still leak. The happy path does not.

const (
	redactMinLen      = 4
	maxSecretBytes    = 64 * 1024
	secretRedactToken = "***"
)

// secretRunTimeout is how long `run` waits. Tests shorten it.
var secretRunTimeout = 2 * time.Minute

var secretsFileMu sync.Mutex

// secretsPath is <config_dir>/secrets (0600 JSON object, name → value).
func secretsPath() string { return filepath.Join(configDir(), "secrets") }

func secretsLockPath() string { return secretsPath() + ".lock" }

// validateSecretName is what /secret add and the MCP tools accept. Short,
// filesystem-and-JSON-friendly, no spaces. The value is unconstrained besides
// size (Telegram already caps a message).
func validateSecretName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("secret name is empty")
	}
	if len(name) > 64 {
		return fmt.Errorf("secret name is too long")
	}
	for i, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		case (r == '_' || r == '-' || r == '.') && i > 0:
		default:
			return fmt.Errorf("secret name %q is not allowed (letters, then letters/digits/._-)", name)
		}
	}
	return nil
}

// validateEnvVarName is POSIX-ish, and rejects the names claudeEnv owns so a
// bot cannot override its own account isolation through the vault.
func validateEnvVarName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("env var name is empty")
	}
	if strings.HasPrefix(name, "CLAUDE") || strings.HasPrefix(name, "ANTHROPIC") {
		return fmt.Errorf("env var %s cannot be injected (account isolation)", name)
	}
	for i, r := range name {
		ok := r == '_' || unicode.IsLetter(r) || (i > 0 && unicode.IsDigit(r))
		if !ok {
			return fmt.Errorf("env var name %q is not allowed", name)
		}
	}
	return nil
}

func lockSecretsFile() (*os.File, error) {
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(secretsLockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close() // safe-ignore: lock failed; the fd is useless
		return nil, err
	}
	return f, nil
}

func loadSecretsMap() (map[string]string, error) {
	body, err := os.ReadFile(secretsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return map[string]string{}, nil
	}
	var m map[string]string
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("secrets file is not valid JSON")
	}
	if m == nil {
		m = map[string]string{}
	}
	return m, nil
}

func writeSecretsMap(m map[string]string) error {
	if m == nil {
		m = map[string]string{}
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	path := secretsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "secrets.tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()        // safe-ignore: best-effort on the error path
		os.Remove(tmpName) // safe-ignore: same
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName) // safe-ignore: same
		return err
	}
	return os.Rename(tmpName, path)
}

// withSecretsFile serializes vault reads/writes across listen and mcp.
func withSecretsFile(mutate func(map[string]string) error) error {
	secretsFileMu.Lock()
	defer secretsFileMu.Unlock()
	lk, err := lockSecretsFile()
	if err != nil {
		return err
	}
	defer func() {
		syscall.Flock(int(lk.Fd()), syscall.LOCK_UN) // safe-ignore: process exit also drops it
		lk.Close()                                   // safe-ignore: same
	}()
	m, err := loadSecretsMap()
	if err != nil {
		return err
	}
	return mutate(m)
}

func listSecretNames() ([]string, error) {
	var names []string
	err := withSecretsFile(func(m map[string]string) error {
		names = make([]string, 0, len(m))
		for k := range m {
			names = append(names, k)
		}
		sort.Strings(names)
		return nil
	})
	return names, err
}

func putSecret(name, value string) error {
	if err := validateSecretName(name); err != nil {
		return err
	}
	if strings.ContainsAny(value, "\x00") {
		return fmt.Errorf("secret value contains a NUL")
	}
	if len(value) > maxSecretBytes {
		return fmt.Errorf("secret value is too large")
	}
	return withSecretsFile(func(m map[string]string) error {
		m[name] = value
		return writeSecretsMap(m)
	})
}

func deleteSecret(name string) (bool, error) {
	if err := validateSecretName(name); err != nil {
		return false, err
	}
	var found bool
	err := withSecretsFile(func(m map[string]string) error {
		if _, ok := m[name]; !ok {
			return nil
		}
		found = true
		delete(m, name)
		return writeSecretsMap(m)
	})
	return found, err
}

func lookupSecret(name string) (string, bool, error) {
	if err := validateSecretName(name); err != nil {
		return "", false, err
	}
	var value string
	var ok bool
	err := withSecretsFile(func(m map[string]string) error {
		value, ok = m[name]
		return nil
	})
	return value, ok, err
}

func vaultValues() ([]string, error) {
	var values []string
	err := withSecretsFile(func(m map[string]string) error {
		for _, v := range m {
			if v != "" {
				values = append(values, v)
			}
		}
		return nil
	})
	return values, err
}

// redactSecrets replaces known values with ***. Short values are skipped so a
// 1-character secret cannot wipe the output. Order is longest-first.
func redactSecrets(s string, values []string) string {
	if s == "" || len(values) == 0 {
		return s
	}
	uniq := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, v := range values {
		if len(v) < redactMinLen || seen[v] {
			continue
		}
		seen[v] = true
		uniq = append(uniq, v)
	}
	sort.Slice(uniq, func(i, j int) bool { return len(uniq[i]) > len(uniq[j]) })
	for _, v := range uniq {
		s = strings.ReplaceAll(s, v, secretRedactToken)
	}
	return s
}

func redactVaultOutput(s string, extra []string) (string, error) {
	all, err := vaultValues()
	if err != nil {
		// A missing or corrupt vault must not publish the raw output.
		// After a restart the in-memory redact list is empty, so the vault
		// is the only source of values to hide.
		return "", err
	}
	values := append(append([]string{}, extra...), all...)
	return redactSecrets(s, values), nil
}

func overlayEnv(base []string, extra map[string]string) []string {
	skip := map[string]bool{}
	for k := range extra {
		skip[k] = true
	}
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if skip[name] {
			continue
		}
		out = append(out, kv)
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+extra[k])
	}
	return out
}

// resolveSecretEnv looks up vault names (the map values) and returns env
// assignments plus the raw values for later redaction. The raw values must
// not be logged or returned to the model.
func resolveSecretEnv(envMap map[string]string) (extra map[string]string, redact []string, err error) {
	if len(envMap) == 0 {
		return nil, nil, nil
	}
	extra = make(map[string]string, len(envMap))
	for envName, secretName := range envMap {
		envName = strings.TrimSpace(envName)
		secretName = strings.TrimSpace(secretName)
		if err := validateEnvVarName(envName); err != nil {
			return nil, nil, err
		}
		if err := validateSecretName(secretName); err != nil {
			return nil, nil, err
		}
		val, ok, err := lookupSecret(secretName)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			return nil, nil, fmt.Errorf("no secret named %s", secretName)
		}
		extra[envName] = val
		redact = append(redact, val)
	}
	return extra, redact, nil
}

func resolveStdinSecret(name string) (value string, err error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	if err := validateSecretName(name); err != nil {
		return "", err
	}
	val, ok, err := lookupSecret(name)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("no secret named %s", name)
	}
	return val, nil
}

// runWithSecrets starts a foreground /bin/sh -c in cwd. The secret values go
// on the child's env and/or stdin, never argv. Output is redacted.
func runWithSecrets(ctx context.Context, cfg *Config, cwd, command string, envMap map[string]string, stdinSecret string) (exit int, output string, err error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return 0, "", fmt.Errorf("run needs a command")
	}
	if len(envMap) == 0 && strings.TrimSpace(stdinSecret) == "" {
		return 0, "", fmt.Errorf("run needs env (env var → secret name) or stdin_secret; use Bash when you do not need the vault")
	}
	extra, redact, err := resolveSecretEnv(envMap)
	if err != nil {
		return 0, "", err
	}
	stdin, err := resolveStdinSecret(stdinSecret)
	if err != nil {
		return 0, "", err
	}
	if stdin != "" {
		redact = append(redact, stdin)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, secretRunTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	if strings.TrimSpace(cwd) != "" {
		cmd.Dir = cwd
	}
	cmd.Env = overlayEnv(instanceEnv(cfg), extra)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	// CommandContext only signals the shell. A command that backgrounds
	// itself would keep the stdout pipe open and CombinedOutput would
	// wait until that grandchild exits. Kill the group, and don't wait
	// forever for the pipes to close.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 3 * time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil || cmd.Process.Pid <= 0 {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	out, runErr := cmd.CombinedOutput()
	redacted, rerr := redactVaultOutput(string(out), redact)
	if rerr != nil {
		redacted = ""
	}
	// A timed-out command comes back as ExitError (-1). That branch used
	// to swallow the timeout and report a normal exit.
	if ctx.Err() != nil {
		if rerr != nil {
			return -1, "", fmt.Errorf("timed out after %s (output withheld)", secretRunTimeout)
		}
		return -1, redacted, fmt.Errorf("timed out after %s", secretRunTimeout)
	}
	if rerr != nil {
		return -1, "", fmt.Errorf("output withheld: %w", rerr)
	}
	output = redacted
	exit = 0
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			exit = ee.ExitCode()
			runErr = nil
		}
	}
	return exit, output, runErr
}

func formatRunResult(exit int, output string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "exit %d\n", exit)
	if strings.TrimSpace(output) == "" {
		sb.WriteString("(no output)\n")
	} else {
		sb.WriteString(output)
		if !strings.HasSuffix(output, "\n") {
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// Telegram: /secret and the capture path
// ---------------------------------------------------------------------------

// secretPending is the instance's at-most-one capture slot. While name is
// set, the owner's next DM is the value: it is not a turn, not logged, not
// stored in the inbox.
type secretPending struct {
	mu   sync.Mutex
	name string
}

func (p *secretPending) set(name string) {
	p.mu.Lock()
	p.name = name
	p.mu.Unlock()
}

func (p *secretPending) get() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.name
}

func (p *secretPending) clear() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	name := p.name
	p.name = ""
	return name
}

func (in *instance) loginBusy() bool {
	in.login.mu.Lock()
	defer in.login.mu.Unlock()
	return in.login.waiting != nil
}

// takeSecretValue consumes the owner's next message while a /secret add is
// waiting. Returns true if the message must not be delivered to a bot.
func (in *instance) takeSecretValue(msg *TelegramMessage) bool {
	name := in.secret.get()
	if name == "" || msg == nil {
		return false
	}
	text := strings.TrimSpace(msg.Text)
	cmd, _ := splitCommand(text)
	if cmd == "/cancel" {
		in.secret.clear()
		in.reply(msg, "Cancelled. Nothing was saved.")
		return true
	}
	if strings.HasPrefix(text, "/") {
		// A new command: drop the capture so /secret list (etc.) still work.
		in.secret.clear()
		hookLog("secret: capture for %s cancelled by command", name)
		return false
	}
	if text == "" {
		in.reply(msg, "The value was empty. Send it again, or /cancel.")
		return true
	}
	in.secret.clear()
	if err := putSecret(name, text); err != nil {
		hookLog("secret: save %s: %v", name, err)
		in.reply(msg, "Could not save <b>"+htmlEscape(name)+"</b>: "+htmlEscape(err.Error()))
		return true
	}
	hookLog("secret: saved %s", name)
	cfg := in.config()
	if cfg != nil && cfg.BotToken != "" && msg.MessageID != 0 {
		if err := deleteMessage(cfg, msg.Chat.ID, int64(msg.MessageID)); err != nil {
			hookLog("secret: delete value message for %s: %v", name, err)
		}
	}
	in.reply(msg, "saved <b>"+htmlEscape(name)+"</b>")
	return true
}

func (in *instance) handleSecretCommand(msg *TelegramMessage, rest string) {
	sub, arg := splitFirstWord(strings.TrimSpace(rest))
	switch strings.ToLower(sub) {
	case "", "list", "ls":
		in.replySecretList(msg)
	case "add", "set":
		in.beginSecretAdd(msg, arg)
	case "delete", "rm", "remove":
		in.deleteSecretCmd(msg, arg)
	case "show", "get", "cat", "print":
		in.reply(msg, "ccc never shows secret values. Use /secret list for names, or a session's <code>run</code> tool to inject one.")
	default:
		in.reply(msg, "Usage: /secret add &lt;name&gt; | /secret list | /secret delete &lt;name&gt;")
	}
}

func (in *instance) replySecretList(msg *TelegramMessage) {
	names, err := listSecretNames()
	if err != nil {
		in.reply(msg, "Could not list secrets: "+htmlEscape(err.Error()))
		return
	}
	if len(names) == 0 {
		in.reply(msg, "No secrets. /secret add &lt;name&gt; then send the value as the next message.")
		return
	}
	var sb strings.Builder
	sb.WriteString("<b>Secrets</b> (names only)\n")
	for _, n := range names {
		fmt.Fprintf(&sb, "• <code>%s</code>\n", htmlEscape(n))
	}
	in.reply(msg, sb.String())
}

func (in *instance) beginSecretAdd(msg *TelegramMessage, name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		in.reply(msg, "Usage: /secret add &lt;name&gt; — then send the value as your next message.")
		return
	}
	if err := validateSecretName(name); err != nil {
		in.reply(msg, htmlEscape(err.Error()))
		return
	}
	if in.loginBusy() {
		in.reply(msg, "Finish the account login (or /cancel) first.")
		return
	}
	if pending := in.secret.get(); pending != "" {
		in.reply(msg, "Already waiting for a value for <b>"+htmlEscape(pending)+"</b>. Send it, or /cancel.")
		return
	}
	in.secret.set(name)
	in.reply(msg, "Send the value for <b>"+htmlEscape(name)+"</b> as your next message (or /cancel). It is not shown to any session.")
}

func (in *instance) deleteSecretCmd(msg *TelegramMessage, name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		in.reply(msg, "Usage: /secret delete &lt;name&gt;")
		return
	}
	ok, err := deleteSecret(name)
	if err != nil {
		in.reply(msg, "Could not delete: "+htmlEscape(err.Error()))
		return
	}
	if !ok {
		in.reply(msg, "No secret named <b>"+htmlEscape(name)+"</b>.")
		return
	}
	hookLog("secret: deleted %s", name)
	in.reply(msg, "deleted <b>"+htmlEscape(name)+"</b>")
}
