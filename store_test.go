package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func testDB(t *testing.T) (*instance, *Bot) {
	t.Helper()
	in, _, _ := testInstance(t)
	b, err := in.createBot("tester", "")
	if err != nil {
		t.Fatalf("createBot: %v", err)
	}
	return in, b
}

func TestMemoryScopeKey(t *testing.T) {
	cases := []struct {
		scope, path string
		wantScope   string
		wantKey     string
		wantErr     bool
	}{
		{scope: scopeUser, wantScope: scopeUser, wantKey: ""},
		{scope: scopeBot, wantScope: scopeBot, wantKey: "7"},
		{scope: scopeProject, path: "/srv/app", wantScope: scopeProject, wantKey: "/srv/app"},
		{scope: scopeProject, wantErr: true},
		{scope: "global", wantErr: true},
	}
	for _, c := range cases {
		scope, key, err := memoryScopeKey(c.scope, 7, c.path)
		if c.wantErr {
			if err == nil {
				t.Errorf("scope %q/%q: expected an error", c.scope, c.path)
			}
			continue
		}
		if err != nil {
			t.Errorf("scope %q: unexpected error %v", c.scope, err)
			continue
		}
		if scope != c.wantScope || key != c.wantKey {
			t.Errorf("scope %q -> (%q,%q), want (%q,%q)", c.scope, scope, key, c.wantScope, c.wantKey)
		}
	}
}

func TestUpsertMemoryReplacesTheSameKey(t *testing.T) {
	in, b := testDB(t)
	if err := upsertMemory(in.db, scopeUser, "", "deploy", "fly.io", b.ID); err != nil {
		t.Fatal(err)
	}
	if err := upsertMemory(in.db, scopeUser, "", "deploy", "OVH VPS", b.ID); err != nil {
		t.Fatal(err)
	}
	var mems []Memory
	if err := in.db.Where("key = ?", "deploy").Find(&mems).Error; err != nil {
		t.Fatal(err)
	}
	if len(mems) != 1 {
		t.Fatalf("expected one row after the upsert, got %d", len(mems))
	}
	if mems[0].Text != "OVH VPS" {
		t.Errorf("text = %q, want the updated value", mems[0].Text)
	}
}

func TestSearchMemoriesRespectsVisibility(t *testing.T) {
	in, mine := testDB(t)
	theirs, err := in.createBot("theirs", "")
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(upsertMemory(in.db, scopeUser, "", "owner-tz", "the owner lives in Madrid", mine.ID))
	must(upsertMemory(in.db, scopeProject, "/srv/app", "deploy-app", "deployed with systemd", mine.ID))
	must(upsertMemory(in.db, scopeBot, fmt.Sprint(mine.ID), "my-note", "remember to check Madrid weather", mine.ID))
	must(upsertMemory(in.db, scopeBot, fmt.Sprint(theirs.ID), "their-note", "Madrid is not my business", theirs.ID))

	got, err := searchMemories(in.db, mine.ID, "Madrid", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, m := range got {
		keys[m.Key] = true
	}
	if !keys["owner-tz"] || !keys["my-note"] {
		t.Errorf("visible memories missing from the results: %v", keys)
	}
	if keys["their-note"] {
		t.Error("another bot's private memory showed up in recall")
	}
}

func TestSearchMemoriesFindsProjectScope(t *testing.T) {
	in, b := testDB(t)
	if err := upsertMemory(in.db, scopeProject, "/srv/app", "deploy-app", "litestream replicates to OVH", b.ID); err != nil {
		t.Fatal(err)
	}
	got, err := searchMemories(in.db, b.ID, "litestream", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != "deploy-app" {
		t.Errorf("project memory not found: %+v", got)
	}
}

func TestSearchMemoriesEmptyQueryListsRecent(t *testing.T) {
	in, b := testDB(t)
	for i := 0; i < 3; i++ {
		if err := upsertMemory(in.db, scopeUser, "", fmt.Sprintf("k%d", i), "value", b.ID); err != nil {
			t.Fatal(err)
		}
	}
	got, err := searchMemories(in.db, b.ID, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("expected all 3 memories, got %d", len(got))
	}
}

// FTS5 MATCH has its own operator syntax; user text must never reach it raw.
func TestSearchMemoriesSurvivesFTSOperators(t *testing.T) {
	in, b := testDB(t)
	if err := upsertMemory(in.db, scopeUser, "", "quirk", "the owner likes parentheses", b.ID); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`parentheses OR (`, `"unterminated`, `NEAR/`, `*`, `^`} {
		if _, err := searchMemories(in.db, b.ID, q, "", 5); err != nil {
			t.Errorf("query %q returned an error: %v", q, err)
		}
	}
}

func TestFTSQueryQuotesEveryTerm(t *testing.T) {
	if got := ftsQuery(`deploy OR (drop table)`); !strings.Contains(got, `"deploy"`) || strings.Contains(got, "(") {
		t.Errorf("ftsQuery(%q) = %q, operators must not survive", `deploy OR (drop table)`, got)
	}
	if got := ftsQuery("!!!"); got != `""` {
		t.Errorf("ftsQuery with no usable words = %q, want an empty phrase", got)
	}
}

func TestFTSIndexIsAvailable(t *testing.T) {
	in, _ := testDB(t)
	var n int
	if err := in.db.Raw(`SELECT count(*) FROM sqlite_master WHERE name = 'memories_fts'`).Scan(&n).Error; err != nil {
		t.Fatalf("cannot inspect the schema: %v", err)
	}
	if n == 0 {
		t.Skip("this SQLite build has no FTS5; recall falls back to LIKE")
	}
	if !ftsAvailable {
		t.Error("the FTS table exists but ftsAvailable is false")
	}
}

func TestUniqueBotNameSuffixes(t *testing.T) {
	in, _, _ := testInstance(t)
	a, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "worker" || b.Name != "worker-2" {
		t.Errorf("names = %q, %q; want worker and worker-2", a.Name, b.Name)
	}
}

func TestSanitizeBotName(t *testing.T) {
	if got := sanitizeBotName("../../etc/passwd"); strings.Contains(got, "/") {
		t.Errorf("sanitizeBotName kept a path separator: %q", got)
	}
	if got := sanitizeBotName("   "); got == "" {
		t.Error("an empty name must get a generated fallback")
	}
	if got := sanitizeBotName(strings.Repeat("x", 200)); len(got) > 64 {
		t.Errorf("name is %d chars, want it clamped", len(got))
	}
}

func TestDataDirDefaultsUnderLocalShare(t *testing.T) {
	got := dataDir(&Config{})
	if !strings.HasSuffix(got, filepath.Join(".local", "share", "ccc")) {
		t.Errorf("default data dir = %q", got)
	}
	if got := dataDir(&Config{DataDir: "/tmp/whatever"}); got != "/tmp/whatever" {
		t.Errorf("configured data dir = %q", got)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	in, _ := testDB(t)
	if got := getSetting(in.db, "model", "sonnet"); got != "sonnet" {
		t.Errorf("missing setting = %q, want the default", got)
	}
	if err := setSetting(in.db, "model", "opus"); err != nil {
		t.Fatal(err)
	}
	if err := setSetting(in.db, "model", "fable"); err != nil {
		t.Fatal(err)
	}
	if got := getSetting(in.db, "model", "sonnet"); got != "fable" {
		t.Errorf("setting = %q, want the last written value", got)
	}
}

func TestSendFileRefusesCredentialPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	iso := filepath.Join(t.TempDir(), "isolated-codex")
	cfg := &Config{
		DataDir: dir,
		Profiles: map[string]*Profile{
			"codex-work": {Engine: engineCodex, ConfigDir: iso},
		},
	}
	cases := []string{
		filepath.Join(dir, "profiles", "work", "settings.json"),
		filepath.Join(home, ".claude", "anything.txt"),
		filepath.Join(dir, ".credentials.json"),
		filepath.Join(home, ".codex", "auth.json"),
		filepath.Join(home, ".codex", "sessions", "rollout.jsonl"),
		filepath.Join(home, ".grok", "auth.json"),
		filepath.Join(home, ".grok", "mcp_credentials.json"),
		filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token"),
		filepath.Join(iso, "auth.json"),
		filepath.Join(dir, "bots", "alpha", "workspace", "auth.json"),
	}
	for _, p := range cases {
		if _, forbidden := sendFileForbidden(cfg, p); !forbidden {
			t.Errorf("send_file should refuse %q", p)
		}
	}
	ok := filepath.Join(dir, "bots", "alpha", "workspace", "report.pdf")
	if reason, forbidden := sendFileForbidden(cfg, ok); forbidden {
		t.Errorf("send_file refused a workspace file (%s)", reason)
	}
}

func TestSanitizeFileNameStripsTraversal(t *testing.T) {
	for _, in := range []string{"../../etc/passwd", "/etc/passwd", "..", ""} {
		got := sanitizeFileName(in)
		if strings.ContainsAny(got, string(os.PathSeparator)) || got == ".." || got == "" {
			t.Errorf("sanitizeFileName(%q) = %q", in, got)
		}
	}
}

// toolText extracts the text of a single-block tool result.
func toolText(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}
