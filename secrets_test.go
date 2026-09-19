package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// vaultTestValue is a distinctive string used as a secret in tests. Failures
// must not print it (t.Errorf the location, not the haystack).
const vaultTestValue = "vault-test-value-xyzzy"

func assertNoSecret(t *testing.T, haystack, where string) {
	t.Helper()
	if strings.Contains(haystack, vaultTestValue) {
		t.Errorf("%s leaked the vault value", where)
	}
}

func TestSecretStoreAddListDelete(t *testing.T) {
	isolateConfigEnv(t)
	t.Setenv("HOME", t.TempDir())

	if err := putSecret("github-work-otp", vaultTestValue); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(secretsPath())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("secrets file mode = %v, want 0600", perm)
	}
	if want := filepath.Join(os.Getenv("HOME"), ".config", "ccc", "secrets"); secretsPath() != want {
		t.Errorf("path = %q, want %q", secretsPath(), want)
	}

	names, err := listSecretNames()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, " ") != "github-work-otp" {
		t.Errorf("names = %v", names)
	}
	for _, n := range names {
		assertNoSecret(t, n, "list names")
	}

	ok, err := deleteSecret("github-work-otp")
	if err != nil || !ok {
		t.Fatalf("delete: ok=%v err=%v", ok, err)
	}
	names, _ = listSecretNames()
	if len(names) != 0 {
		t.Errorf("after delete: %v", names)
	}
}

func TestValidateSecretName(t *testing.T) {
	for _, good := range []string{"github-work-otp", "A", "token_1", "foo.bar"} {
		if err := validateSecretName(good); err != nil {
			t.Errorf("%q should be allowed: %v", good, err)
		}
	}
	for _, bad := range []string{"", "1start", "has space", "a/b", "-dash", strings.Repeat("a", 65)} {
		if err := validateSecretName(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestSecretCaptureNeverReachesTheModel(t *testing.T) {
	in, runner, api := testInstance(t)

	in.handleMessage(ownerMessage("/secret add github-work-otp"))
	prompt := strings.Join(api.texts(""), "\n")
	if !strings.Contains(prompt, "github-work-otp") || !strings.Contains(strings.ToLower(prompt), "next message") {
		t.Errorf("add should prompt for the value by name:\n%s", prompt)
	}
	assertNoSecret(t, prompt, "/secret add prompt")
	if _, ok := runner.last(); ok {
		t.Error("/secret add enqueued a turn")
	}

	valueMsg := ownerMessage(vaultTestValue)
	valueMsg.MessageID = 77
	in.handleMessage(valueMsg)

	if _, ok := runner.last(); ok {
		t.Error("the captured value enqueued a turn")
	}
	var n int64
	in.db.Model(&Turn{}).Count(&n)
	if n != 0 {
		t.Errorf("turns table has %d rows after capture", n)
	}
	var inbox int64
	in.db.Model(&InboxMessage{}).Count(&inbox)
	if inbox != 0 {
		t.Errorf("inbox has %d rows after capture", inbox)
	}

	joined := strings.Join(api.texts(""), "\n")
	if !strings.Contains(joined, "saved") || !strings.Contains(joined, "github-work-otp") {
		t.Errorf("success reply missing:\n%s", joined)
	}
	assertNoSecret(t, joined, "telegram replies")

	if got := len(api.since("deleteMessage")); got != 1 {
		t.Errorf("deleteMessage calls = %d, want 1", got)
	}

	names, err := listSecretNames()
	if err != nil || strings.Join(names, " ") != "github-work-otp" {
		t.Errorf("stored names = %v err=%v", names, err)
	}
	val, ok, err := lookupSecret("github-work-otp")
	if err != nil || !ok || val != vaultTestValue {
		t.Error("vault did not store the captured value")
	}

	assertNoSecretInLogs(t)
}

func TestSecretCaptureCancelAndCommandAbort(t *testing.T) {
	in, runner, api := testInstance(t)

	in.handleMessage(ownerMessage("/secret add foo"))
	in.handleMessage(ownerMessage("/cancel"))
	if in.secret.get() != "" {
		t.Error("cancel left capture open")
	}
	if _, ok := runner.last(); ok {
		t.Error("/cancel enqueued a turn")
	}

	in.handleMessage(ownerMessage("/secret add foo"))
	in.handleMessage(ownerMessage("/secret list"))
	if in.secret.get() != "" {
		t.Error("a follow-up command should abort capture")
	}
	joined := strings.Join(api.texts(""), "\n")
	if !strings.Contains(joined, "No secrets") && !strings.Contains(joined, "Secrets") {
		t.Errorf("/secret list after abort did not run:\n%s", joined)
	}
}

func TestSecretShowIsForbidden(t *testing.T) {
	in, runner, api := testInstance(t)
	if err := putSecret("x", vaultTestValue); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"/secret show x", "/secret get x", "/secret cat x"} {
		in.handleMessage(ownerMessage(cmd))
	}
	if _, ok := runner.last(); ok {
		t.Error("show/get enqueued a turn")
	}
	joined := strings.Join(api.texts(""), "\n")
	if !strings.Contains(joined, "never shows") {
		t.Errorf("show should be refused:\n%s", joined)
	}
	assertNoSecret(t, joined, "/secret show")
}

func TestSecretDeleteCommand(t *testing.T) {
	in, _, api := testInstance(t)
	if err := putSecret("old-token", vaultTestValue); err != nil {
		t.Fatal(err)
	}
	in.handleMessage(ownerMessage("/secret delete old-token"))
	joined := strings.Join(api.texts(""), "\n")
	if !strings.Contains(joined, "deleted") || !strings.Contains(joined, "old-token") {
		t.Errorf("delete reply:\n%s", joined)
	}
	assertNoSecret(t, joined, "/secret delete")
	names, _ := listSecretNames()
	if len(names) != 0 {
		t.Errorf("still listed: %v", names)
	}
}

func TestMCPSecretsListDeleteAndNoGet(t *testing.T) {
	in, _, _ := testInstance(t)
	if err := putSecret("alpha", vaultTestValue); err != nil {
		t.Fatal(err)
	}
	if err := putSecret("beta", "other-value-not-the-const"); err != nil {
		t.Fatal(err)
	}
	s := &mcpServer{db: in.db, config: in.cfg, botID: 1}
	res, _, err := s.secretsList(t.Context(), nil, emptyIn{})
	if err != nil || res.IsError {
		t.Fatalf("list: %+v %v", res, err)
	}
	body := toolText(res)
	if !strings.Contains(body, "alpha") || !strings.Contains(body, "beta") {
		t.Errorf("list body = %q", body)
	}
	assertNoSecret(t, body, "secrets_list")
	if strings.Contains(strings.ToLower(body), "other-value") {
		t.Error("secrets_list returned a value")
	}

	res, _, err = s.secretsDelete(t.Context(), nil, secretNameIn{Name: "alpha"})
	if err != nil || res.IsError {
		t.Fatalf("delete: %+v %v", res, err)
	}
	assertNoSecret(t, toolText(res), "secrets_delete")
	names, _ := listSecretNames()
	if strings.Join(names, " ") != "beta" {
		t.Errorf("after mcp delete: %v", names)
	}
}

func TestRunInjectsEnvAndRedactsOutput(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := putSecret("deploy-token", vaultTestValue); err != nil {
		t.Fatal(err)
	}
	s := &mcpServer{db: in.db, config: in.cfg, botID: b.ID}
	res, _, err := s.runSecret(t.Context(), nil, runIn{
		Command: `printf '%s\n' "$DEPLOY_TOKEN"`,
		Env:     map[string]string{"DEPLOY_TOKEN": "deploy-token"},
	})
	if err != nil || res.IsError {
		t.Fatalf("run: %s %v", toolText(res), err)
	}
	body := toolText(res)
	if !strings.Contains(body, "exit 0") {
		t.Errorf("expected exit 0, got %q", body)
	}
	if !strings.Contains(body, secretRedactToken) {
		t.Error("redacted output should contain ***")
	}
	assertNoSecret(t, body, "run tool result")
}

func TestRunStdinInjectAndRedact(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := putSecret("pem", vaultTestValue); err != nil {
		t.Fatal(err)
	}
	s := &mcpServer{db: in.db, config: in.cfg, botID: b.ID}
	res, _, err := s.runSecret(t.Context(), nil, runIn{
		Command:     "cat",
		StdinSecret: "pem",
	})
	if err != nil || res.IsError {
		t.Fatalf("run: %s %v", toolText(res), err)
	}
	body := toolText(res)
	if !strings.Contains(body, secretRedactToken) {
		t.Error("stdin echo should be redacted")
	}
	assertNoSecret(t, body, "run stdin result")
}

func TestRunUnknownSecretAndNoEnv(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}
	s := &mcpServer{db: in.db, config: in.cfg, botID: b.ID}
	res, _, err := s.runSecret(t.Context(), nil, runIn{
		Command: "true",
		Env:     map[string]string{"X": "missing-name"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(toolText(res), "missing-name") {
		t.Errorf("unknown secret: %s", toolText(res))
	}
	res, _, err = s.runSecret(t.Context(), nil, runIn{Command: "true"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("run without env/stdin should fail")
	}
}

func TestRunRejectsClaudeEnvOverride(t *testing.T) {
	in, _, _ := testInstance(t)
	b, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := putSecret("x", vaultTestValue); err != nil {
		t.Fatal(err)
	}
	s := &mcpServer{db: in.db, config: in.cfg, botID: b.ID}
	res, _, err := s.runSecret(t.Context(), nil, runIn{
		Command: "true",
		Env:     map[string]string{"CLAUDE_CONFIG_DIR": "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("CLAUDE_* inject should be refused")
	}
	assertNoSecret(t, toolText(res), "CLAUDE inject error")
}

func TestBackgroundJobInjectsAndRedacts(t *testing.T) {
	sched, in, runner, _ := testScheduler(t)
	b, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := putSecret("bg-token", vaultTestValue); err != nil {
		t.Fatal(err)
	}
	job, err := queueBackgroundJob(in.db, b.ID, 0, "print", `printf '%s\n' "$BG_TOKEN"`,
		bgSecrets{Env: map[string]string{"BG_TOKEN": "bg-token"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(job.Command, vaultTestValue) || strings.Contains(job.EnvJSON, vaultTestValue) {
		t.Error("job row stored a vault value")
	}
	sched.tickBackground()
	got := waitJob(t, in, job.ID)
	if got.Status != jobDone {
		t.Fatalf("status=%q error=%q", got.Status, got.Error)
	}
	assertNoSecret(t, got.Output, "background_jobs.output")
	if !strings.Contains(got.Output, secretRedactToken) {
		t.Error("stored output should be redacted")
	}
	queued := waitEnqueued(t, runner, 1)
	if len(queued) != 1 {
		t.Fatalf("enqueued %d", len(queued))
	}
	assertNoSecret(t, queued[0].Text, "background wake")
}

func TestRedactSecretsLongestFirst(t *testing.T) {
	got := redactSecrets("abcXYZdefXYZ", []string{"XYZ", "XYZdefXYZ"})
	if strings.Contains(got, "XYZ") && !strings.Contains(got, secretRedactToken) {
		t.Errorf("redact = %q", got)
	}
	if redactSecrets("ab", []string{"ab"}) != "ab" {
		t.Error("values shorter than redactMinLen should be left alone")
	}
}

func assertNoSecretInLogs(t *testing.T) {
	t.Helper()
	for _, rel := range []string{
		filepath.Join("Library", "Caches", "ccc", "hook-debug.log"),
		filepath.Join("Library", "Caches", "ccc", "ccc.log"),
	} {
		p := filepath.Join(os.Getenv("HOME"), rel)
		body, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		assertNoSecret(t, string(body), p)
	}
}
