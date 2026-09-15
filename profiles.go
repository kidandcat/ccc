package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Multi-profile support: one ccc can drive several accounts at once, mixing
// engines in the same instance (Claude + Grok + Antigravity).
//
// Engine is a property of the account, set when it is added — not a per-bot
// toggle. A Claude profile is a CLAUDE_CONFIG_DIR (credentials, .claude.json,
// projects/ and settings.json). A Grok profile is an isolated GROK_HOME
// (auth.json). An Antigravity profile is an isolated HOME so ~/.gemini stays
// off the real user home and off Claude's config dirs.
//
// Claude profiles of an instance still share projects/ (DESIGN §4) so failover
// can resume the same conversation UUID. Failover stays inside one engine.
//
// Verified against Claude Code 2.1.270. Grok isolation is the documented
// GROK_HOME. Antigravity has no official profile selector; isolation is HOME +
// GEMINI_HOME + GEMINI_FORCE_FILE_STORAGE (file token, not the OS keyring).

// defaultProfileName is the name of the synthesized profile used when the user
// has not configured any. It keeps single-account setups working unchanged.
const defaultProfileName = "default"

// Profile is one account sandbox: a Claude CLAUDE_CONFIG_DIR, a Grok
// GROK_HOME, or an isolated Antigravity HOME. Engine is set at add time.
type Profile struct {
	// Name is the config-map key; it is not stored inside the object.
	Name string `json:"-"`
	// Engine is claude, grok, antigravity or codex. Empty means Claude so existing
	// config.json entries stay backward compatible.
	Engine string `json:"engine,omitempty"`
	// ConfigDir is the isolated home for this account. Claude: CLAUDE_CONFIG_DIR
	// (empty = claude's own default layout). Grok: GROK_HOME. Antigravity: the
	// HOME the agy child runs with. An EMPTY config_dir means "whatever that
	// CLI uses by default" — ccc then sets no isolation variable.
	ConfigDir string `json:"config_dir"`
	// Label is a human hint (usually the account email). Cosmetic only.
	Label string `json:"label,omitempty"`
	// Implicit marks the profile synthesized for a config without any
	// `profiles` block. For it ccc passes no CLAUDE_CONFIG_DIR at all (unless
	// the var was already set in ccc's own environment at startup), so a
	// single-account install behaves exactly as it did before profiles existed.
	Implicit bool `json:"-"`
}

// profileEngine is the engine this account was registered for. Empty or
// unknown values are Claude so a hand-edited config cannot break dispatch.
func profileEngine(p Profile) string {
	e, err := parseEngine(p.Engine)
	if err != nil {
		return engineClaude
	}
	return e
}

// inheritedConfigDir is $CLAUDE_CONFIG_DIR as it was when ccc started, captured
// before anything scrubs the environment. "" means the user did not set one.
var inheritedConfigDir string

func initProfiles() {
	inheritedConfigDir = strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
}

// implicitProfile is the profile used when no `profiles` block is configured:
// $CLAUDE_CONFIG_DIR if ccc inherited one, else ~/.claude.
func implicitProfile() Profile {
	dir := inheritedConfigDir
	implicit := true
	if dir == "" {
		home, _ := os.UserHomeDir() // safe-ignore: an empty home yields a relative .claude path, which is the least-bad fallback here
		dir = filepath.Join(home, ".claude")
	} else {
		// ccc was started with an explicit dir — pass it through to children.
		implicit = false
	}
	return Profile{Name: defaultProfileName, Engine: engineClaude, ConfigDir: dir, Implicit: implicit}
}

// implicitAccount is the fallback when a bot's engine has no registered
// accounts: Claude's implicit profile, or a machine-default Grok/Antigravity
// home. That keeps `/engine grok` working before any grok account is added,
// and lets existing tests spawn grok/agy against the CLI on PATH.
func implicitAccount(engine string) Profile {
	engine, err := parseEngine(engine)
	if err != nil || engine == engineClaude {
		return implicitProfile()
	}
	return Profile{Name: engine, Engine: engine, Implicit: true}
}

// listProfiles returns every configured profile sorted by name, or the single
// implicit profile when none are configured.
func listProfiles(config *Config) []Profile {
	if config == nil || len(config.Profiles) == 0 {
		return []Profile{implicitProfile()}
	}
	names := make([]string, 0, len(config.Profiles))
	for name := range config.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Profile, 0, len(names))
	for _, name := range names {
		p := config.Profiles[name]
		if p == nil {
			continue
		}
		engine := profileEngine(*p)
		if strings.TrimSpace(p.ConfigDir) == "" {
			if engine == engineClaude {
				// Explicitly registered, but pinned to claude's own default layout.
				imp := implicitProfile()
				out = append(out, Profile{Name: name, Engine: engineClaude, ConfigDir: imp.ConfigDir, Label: p.Label, Implicit: imp.Implicit})
				continue
			}
			out = append(out, Profile{Name: name, Engine: engine, Label: p.Label, Implicit: true})
			continue
		}
		out = append(out, Profile{Name: name, Engine: engine, ConfigDir: expandPath(p.ConfigDir), Label: p.Label})
	}
	if len(out) == 0 {
		return []Profile{implicitProfile()}
	}
	return out
}

// profileByKey resolves a profile by its config-map key. Buttons, pickAccount
// and other internal callers have a real key; they must not go through
// profileByName, which treats a bare email as ambiguous when several engines
// share it.
func profileByKey(config *Config, name string) (Profile, bool) {
	if name == "" {
		return defaultProfile(config), true
	}
	for _, p := range listProfiles(config) {
		if p.Name == name {
			return p, true
		}
	}
	return Profile{}, false
}

// profileByName resolves a profile by the way the owner addressed it. A bare
// identity (usually an email) matches when exactly one profile uses it. When
// several engines share that identity the lookup fails — the owner must name
// the engine (`email/codex` or `email codex`). The config-map key and the
// legacy name a profile was keyed by before its email was known still match,
// case-insensitively. An empty name means "the default".
func profileByName(config *Config, name string) (Profile, bool) {
	matches := profilesForName(config, name)
	if len(matches) == 1 {
		return matches[0], true
	}
	return Profile{}, false
}

// profilesForName is the shared lookup behind profileByName and the
// "specify the engine" reply. Zero matches means unknown; more than one means
// the identity is shared across engines.
func profilesForName(config *Config, name string) []Profile {
	if name == "" {
		return []Profile{defaultProfile(config)}
	}
	identity, engine := parseAccountRef(name)
	if identity == "" {
		return nil
	}
	if matches := profilesMatching(config, identity, engine); len(matches) > 0 {
		return matches
	}
	if engine != "" {
		return nil
	}
	// A legacy key ("default", "work") whose label is already an email: the
	// identity parse does not see the key, but the owner can still type it.
	key := normalizeEmail(name)
	var out []Profile
	for _, p := range listProfiles(config) {
		if p.Name == name || normalizeEmail(p.Name) == key {
			out = append(out, p)
		}
	}
	return out
}

// profilesMatching finds profiles with this identity, optionally restricted
// to one engine. Identity is the human address (email or short name), not the
// config-map key.
func profilesMatching(config *Config, identity, engine string) []Profile {
	identity = normalizeEmail(identity)
	if identity == "" {
		return nil
	}
	if engine != "" {
		if e, err := parseEngine(engine); err == nil {
			engine = e
		}
	}
	var out []Profile
	for _, p := range listProfiles(config) {
		if engine != "" && profileEngine(p) != engine {
			continue
		}
		if profileIdentity(p) == identity || normalizeEmail(p.Name) == identity {
			out = append(out, p)
		}
	}
	return out
}

// profileByIdentityEngine is the existence check for /account add: the same
// email on a different engine is a different account.
func profileByIdentityEngine(config *Config, identity, engine string) (Profile, bool) {
	if e, err := parseEngine(engine); err == nil {
		engine = e
	}
	matches := profilesMatching(config, identity, engine)
	if len(matches) == 0 {
		return Profile{}, false
	}
	return matches[0], true
}

// profileIdentity is the human address a profile is known by: its label
// (the email the owner typed, or the one auth status reported) or the
// identity half of a composite key.
func profileIdentity(p Profile) string {
	if label := strings.TrimSpace(p.Label); label != "" {
		return normalizeEmail(label)
	}
	if id, _, ok := splitAccountKey(p.Name); ok {
		return id
	}
	return normalizeEmail(p.Name)
}

// ---------------------------------------------------------------------------
// Accounts are addressed by email
// ---------------------------------------------------------------------------

// accountEmailRe is the shape /account accepts. It is deliberately loose (one
// @, a dotted domain, no spaces or slashes): the authority on whether an
// address is real is `claude auth status`, not a regexp. Slashes are reserved
// for composite keys (`you@x.com/codex`).
var accountEmailRe = regexp.MustCompile(`^[^\s@/]+@[^\s@./]+(?:\.[^\s@./]+)+$`)

// isAccountEmail reports whether s is plausibly an email address.
func isAccountEmail(s string) bool { return accountEmailRe.MatchString(strings.TrimSpace(s)) }

// accountIdentityRe is the shape non-Claude identities accept: a short name or
// email, no spaces or path separators. Claude still requires a real email.
var accountIdentityRe = regexp.MustCompile(`^[A-Za-z0-9._@+-][A-Za-z0-9._@+-]{0,63}$`)

// isAccountIdentity reports whether s is a usable account key for grok/agy.
func isAccountIdentity(s string) bool { return accountIdentityRe.MatchString(strings.TrimSpace(s)) }

// normalizeEmail is the canonical form of an account key: addresses are
// case-insensitive in practice and the config map is not.
func normalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// normalizeAccountKey is the config-map key for an identity + engine.
// Claude keeps the plain email so existing configs stay valid. Every other
// engine (and a Claude account whose email is already taken by another
// engine) uses `identity/engine` (e.g. `jairo@x.com/codex`).
func normalizeAccountKey(identity, engine string) string {
	identity = normalizeEmail(identity)
	e, err := parseEngine(engine)
	if err != nil {
		e = engineClaude
	}
	if e == engineClaude {
		return identity
	}
	return identity + "/" + e
}

// accountMapKey picks a unique Profiles map key. It is normalizeAccountKey
// unless that string is already occupied by a different engine, in which
// case Claude also gets the composite form (`email/claude`).
func accountMapKey(config *Config, identity, engine string) string {
	preferred := normalizeAccountKey(identity, engine)
	if p, ok := profileByKey(config, preferred); ok && profileEngine(p) != profileEngine(Profile{Engine: engine}) {
		e, err := parseEngine(engine)
		if err != nil {
			e = engineClaude
		}
		return normalizeEmail(identity) + "/" + e
	}
	return preferred
}

// splitAccountKey reads a composite `identity/engine` key. A bare email or
// short name is not composite.
func splitAccountKey(s string) (identity, engine string, ok bool) {
	s = strings.TrimSpace(s)
	i := strings.LastIndex(s, "/")
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	e, known := knownEngineName(s[i+1:])
	if !known {
		return "", "", false
	}
	return normalizeEmail(s[:i]), e, true
}

// parseAccountRef reads how the owner addressed an account:
//
//	you@example.com
//	you@example.com/codex
//	you@example.com codex
//	codex you@example.com
func parseAccountRef(s string) (identity, engine string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	if id, eng, ok := splitAccountKey(s); ok {
		return id, eng
	}
	first, rest := splitFirstWord(s)
	second, extra := splitFirstWord(rest)
	if strings.TrimSpace(extra) != "" {
		return normalizeEmail(s), ""
	}
	if second == "" {
		return normalizeEmail(first), ""
	}
	if e, ok := knownEngineName(second); ok {
		return normalizeEmail(first), e
	}
	if e, ok := knownEngineName(first); ok {
		return normalizeEmail(second), e
	}
	return normalizeEmail(s), ""
}

// accountAddress is the unambiguous form to type back: the email when that
// is unique to Claude, otherwise `identity/engine`.
func accountAddress(p Profile) string {
	return normalizeAccountKey(profileIdentity(p), profileEngine(p))
}

// addressedAccount is the user-facing lookup: unique match, not found, or
// "specify the engine". hint is HTML for Telegram (empty when ok or unknown).
func addressedAccount(config *Config, name string) (Profile, string, bool) {
	matches := profilesForName(config, name)
	switch len(matches) {
	case 1:
		return matches[0], "", true
	case 0:
		return Profile{}, "", false
	default:
		return Profile{}, ambiguousAccountHTML(name, matches), false
	}
}

func ambiguousAccountHTML(typed string, matches []Profile) string {
	return "Several accounts use <b>" + htmlEscape(accountRefIdentity(typed, matches)) +
		"</b>. Specify the engine: " + joinAccountRefsHTML(matches) + "."
}

func ambiguousAccountText(typed string, matches []Profile) string {
	refs := accountRefList(matches)
	return fmt.Sprintf("several accounts use %s; specify the engine (%s)",
		accountRefIdentity(typed, matches), strings.Join(refs, " or "))
}

func accountRefIdentity(typed string, matches []Profile) string {
	identity, _ := parseAccountRef(typed)
	if identity != "" {
		return identity
	}
	if len(matches) > 0 {
		return profileIdentity(matches[0])
	}
	return strings.TrimSpace(typed)
}

func accountRefList(matches []Profile) []string {
	refs := make([]string, 0, len(matches))
	for _, p := range matches {
		refs = append(refs, accountAddress(p))
	}
	sort.Strings(refs)
	return refs
}

func joinAccountRefsHTML(matches []Profile) string {
	refs := accountRefList(matches)
	escaped := make([]string, 0, len(refs))
	for _, r := range refs {
		escaped = append(escaped, "<code>"+htmlEscape(r)+"</code>")
	}
	return strings.Join(escaped, " or ")
}

// knownEngineName reports whether s is an engine name or alias (not empty).
func knownEngineName(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "", false
	}
	e, err := parseEngine(s)
	return e, err == nil
}

// profileDirName turns an email into a filesystem-safe directory name
// ("jairo@agentero.com" -> "jairo_at_agentero.com"). It exists only so a
// profile has somewhere to live: the owner never sees it, and it is never an
// identifier — the profile is keyed by the email itself.
func profileDirName(email string) string {
	s := strings.ReplaceAll(normalizeEmail(email), "@", "_at_")
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			sb.WriteRune(r)
		default:
			sb.WriteByte('_')
		}
	}
	// Leading dots would hide the directory (or spell ".."), so they go too.
	out := strings.Trim(sb.String(), "._-")
	if out == "" {
		out = "account"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// accountDisplay is how a profile is named in Telegram: its email (or the
// short identity the owner typed). The engine is shown separately via
// engineLabel; composite keys like `you@x.com/codex` are never the title.
func accountDisplay(p Profile) string {
	if id, _, ok := splitAccountKey(p.Name); ok {
		if isAccountEmail(p.Label) {
			return normalizeEmail(p.Label)
		}
		if isAccountEmail(id) {
			return id
		}
		if label := strings.TrimSpace(p.Label); label != "" {
			return label
		}
		return id
	}
	if isAccountEmail(p.Name) {
		return p.Name
	}
	if isAccountEmail(p.Label) {
		return normalizeEmail(p.Label)
	}
	if label := strings.TrimSpace(p.Label); label != "" {
		return label
	}
	return p.Name
}

// accountTargetBudget is Telegram's 64-byte callback_data cap minus the longest
// prefix the account buttons use ("account:default:").
const accountTargetBudget = 64 - len("account:default:")

// accountTarget is the profile reference an inline button carries. A profile is
// keyed by an email now, and a long address would overflow callback_data, so an
// oversized key travels as a short digest instead.
func accountTarget(name string) string {
	if len(name) <= accountTargetBudget {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return "#" + hex.EncodeToString(sum[:6])
}

// resolveAccountTarget maps what a button carried back to a profile.
func resolveAccountTarget(config *Config, target string) (Profile, bool) {
	if strings.HasPrefix(target, "#") {
		for _, p := range listProfiles(config) {
			if accountTarget(p.Name) == target {
				return p, true
			}
		}
		return Profile{}, false
	}
	return profileByKey(config, target)
}

// profileDirFor picks the config dir for a new account. Two different addresses
// can sanitize to the same directory, so a taken one gets a numeric suffix
// rather than two accounts sharing credentials.
func profileDirFor(config *Config, email string) string {
	root := filepath.Join(dataDir(config), "profiles")
	base := profileDirName(email)
	taken := map[string]bool{}
	for _, p := range listProfiles(config) {
		if p.ConfigDir != "" {
			taken[p.ConfigDir] = true
		}
	}
	dir := filepath.Join(root, base)
	for i := 2; taken[dir]; i++ {
		dir = filepath.Join(root, fmt.Sprintf("%s-%d", base, i))
	}
	return dir
}

// accountDirFor picks the isolated home for a new account. Claude keeps the
// existing <data_dir>/profiles/<email> layout. Grok and Antigravity live under
// <data_dir>/accounts/<engine>/<identity> so their credentials never share a
// directory with Claude profiles or with each other.
func accountDirFor(config *Config, engine, identity string) string {
	engine, err := parseEngine(engine)
	if err != nil || engine == engineClaude {
		return profileDirFor(config, identity)
	}
	root := filepath.Join(dataDir(config), "accounts", engine)
	base := profileDirName(identity)
	taken := map[string]bool{}
	for _, p := range listProfiles(config) {
		if p.ConfigDir != "" {
			taken[p.ConfigDir] = true
		}
	}
	dir := filepath.Join(root, base)
	for i := 2; taken[dir]; i++ {
		dir = filepath.Join(root, fmt.Sprintf("%s-%d", base, i))
	}
	return dir
}

// engineHome is the on-disk root a turn or login uses for this account.
// Claude: CLAUDE_CONFIG_DIR. Grok: GROK_HOME (default ~/.grok). Antigravity:
// isolated HOME (default the real user home).
func engineHome(p Profile) string {
	switch profileEngine(p) {
	case engineGrok:
		if p.ConfigDir != "" {
			return p.ConfigDir
		}
		home, _ := os.UserHomeDir() // safe-ignore: empty home yields a relative .grok path, same fallback as implicitProfile
		return filepath.Join(home, ".grok")
	case engineCodex:
		if p.ConfigDir != "" {
			return p.ConfigDir
		}
		home, _ := os.UserHomeDir() // safe-ignore: empty home yields a relative .codex path
		return filepath.Join(home, ".codex")
	case engineAntigravity:
		if p.ConfigDir != "" {
			return p.ConfigDir
		}
		home, _ := os.UserHomeDir() // safe-ignore: empty home yields the process cwd, the least-bad machine default
		return home
	default:
		return claudeHome(p)
	}
}

// listProfilesForEngine is the account pool a turn of this engine may use.
func listProfilesForEngine(config *Config, engine string) []Profile {
	engine, err := parseEngine(engine)
	if err != nil {
		engine = engineClaude
	}
	var out []Profile
	for _, p := range listProfiles(config) {
		if profileEngine(p) == engine {
			out = append(out, p)
		}
	}
	return out
}

// configuredEngines lists distinct engines that have a registered account.
func configuredEngines(config *Config) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range listProfiles(config) {
		e := profileEngine(p)
		if seen[e] {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

// defaultProfile is the profile new sessions fall back to: config.DefaultProfile
// when it resolves, else the first profile by name.
func defaultProfile(config *Config) Profile {
	all := listProfiles(config)
	if config != nil && config.DefaultProfile != "" {
		for _, p := range all {
			if p.Name == config.DefaultProfile {
				return p
			}
		}
	}
	return all[0]
}

// claudeHome is the on-disk root of a profile: the directory that holds
// .claude.json, settings.json and projects/.
func claudeHome(p Profile) string {
	if p.ConfigDir != "" {
		return p.ConfigDir
	}
	return implicitProfile().ConfigDir
}

func profileProjectsDir(p Profile) string { return filepath.Join(claudeHome(p), "projects") }
func profileSettings(p Profile) string    { return filepath.Join(claudeHome(p), "settings.json") }

// profileClaudeJSON is the odd one out. Everything else lives INSIDE the config
// dir, but .claude.json only moves in when CLAUDE_CONFIG_DIR is actually set:
//
//	CLAUDE_CONFIG_DIR unset → ~/.claude.json   (beside ~/.claude, not in it)
//	CLAUDE_CONFIG_DIR=X     → X/.claude.json
//
// Verified on 2.1.259: running any claude command with CLAUDE_CONFIG_DIR
// pointed at ~/.claude creates a SECOND, empty ~/.claude/.claude.json rather
// than reusing ~/.claude.json. Getting this wrong makes every profile's usage
// cache read as "unknown".
func profileClaudeJSON(p Profile) string {
	home, _ := os.UserHomeDir() // safe-ignore: an empty home yields a relative path, the same least-bad fallback as implicitProfile
	legacy := filepath.Join(home, ".claude.json")
	if p.Implicit {
		return legacy
	}
	inDir := filepath.Join(claudeHome(p), ".claude.json")
	// A profile registered against the default ~/.claude only grows its own
	// .claude.json once claude next runs with CLAUDE_CONFIG_DIR set. Until then
	// the real config is still the legacy one, and reading the in-dir path
	// would report every usage number as unknown.
	if _, err := os.Stat(inDir); err != nil && claudeHome(p) == filepath.Join(home, ".claude") {
		if _, err := os.Stat(legacy); err == nil {
			return legacy
		}
	}
	return inDir
}

// envWhitelist is the exact set of variables a child `claude` may inherit.
//
// This is a whitelist, never a filter, because environment leakage is real:
// when ccc is itself started from inside a Claude Code session (or the desktop
// app) the parent exports CLAUDECODE=1, ANTHROPIC_BASE_URL,
// CLAUDE_CODE_OAUTH_SCOPES, CLAUDE_CODE_MESSAGING_*, CLAUDE_CODE_SDK_* and
// friends. A child `claude` inherits them and then authenticates/behaves
// differently — we observed `claude -p` failing with an org-policy error purely
// because of inherited env. No parent CLAUDE*/ANTHROPIC* variable is ever
// passed through; the only one ccc sets is CLAUDE_CONFIG_DIR.
var envWhitelist = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL",
	"LANG", "TMPDIR", "TZ", "TERM", "SSH_AUTH_SOCK",
}

// envWhitelistPrefixes are variable name prefixes kept wholesale.
var envWhitelistPrefixes = []string{"LC_", "XDG_"}

// claudeEnv builds the environment for every child `claude` process: the
// whitelist above plus this profile's CLAUDE_CONFIG_DIR. Every exec.Command
// that runs claude MUST use it — see envWhitelist for why.
func claudeEnv(p Profile) []string {
	keep := map[string]bool{}
	for _, k := range envWhitelist {
		keep[k] = true
	}
	var env []string
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		name := kv[:eq]
		if keep[name] {
			env = append(env, kv)
			continue
		}
		for _, prefix := range envWhitelistPrefixes {
			if strings.HasPrefix(name, prefix) {
				env = append(env, kv)
				break
			}
		}
	}
	// The implicit profile with no inherited dir means "whatever claude does by
	// default" — setting the var would pin a path claude might resolve
	// differently (and would defeat the point of leaving it alone).
	if !p.Implicit && p.ConfigDir != "" {
		env = append(env, "CLAUDE_CONFIG_DIR="+p.ConfigDir)
	}
	return env
}

// ---------------------------------------------------------------------------
// Profile selection for new sessions
// ---------------------------------------------------------------------------

// unknownUtilization is the assumed utilization when a profile's cache has no
// usable number: pessimistic enough not to be picked over a known-idle account,
// optimistic enough not to be starved by a known-busy one.
const unknownUtilization = 50

// defaultLimitCooldown is how long a profile is skipped after a usage/rate
// limit signal when its cache carries no resets_at to aim at.
const defaultLimitCooldown = 30 * time.Minute

// usageCache mirrors the parts of <config_dir>/.claude.json ccc reads.
// Claude Code used to persist cachedUsageUtilization there; 2.1.x often
// does not, so /account also fetches /api/oauth/usage (usagefetch.go).
type usageCache struct {
	CachedUsageUtilization struct {
		FetchedAtMs int64             `json:"fetchedAtMs"`
		Utilization oauthUsagePayload `json:"utilization"`
	} `json:"cachedUsageUtilization"`
}

// oauthUsagePayload is both the .claude.json utilization object and the
// body of GET /api/oauth/usage. five_hour/seven_day may be null; the
// structured `limits` array is the fallback Claude Code itself uses.
type oauthUsagePayload struct {
	FiveHour *usageWindow `json:"five_hour"`
	SevenDay *usageWindow `json:"seven_day"`
	Limits   []usageLimit `json:"limits"`
}

type usageWindow struct {
	Utilization *float64 `json:"utilization"` // 0-100, nil when unknown (API sends 2.0, not 2)
	ResetsAt    string   `json:"resets_at"`   // RFC3339, "" when unknown
}

type usageLimit struct {
	Kind     string  `json:"kind"` // session, weekly_all, weekly_scoped
	Percent  float64 `json:"percent"`
	ResetsAt string  `json:"resets_at"`
}

// profileUsage is the digested usage snapshot for one profile.
type profileUsage struct {
	FiveHour        int // 0-100, unknownUtilization when unavailable
	SevenDay        int // 0-100, unknownUtilization when unavailable
	FiveHourKnown   bool
	SevenDayKnown   bool
	FiveHourResetAt time.Time // zero when unknown
}

// readProfileUsage is the no-network view: a fresh in-memory snapshot if
// the doctor or /account just fetched one, else Claude Code's on-disk cache.
// Missing data yields unknownUtilization rather than an error: chooseProfile
// must never block on a cold cache.
func readProfileUsage(p Profile) profileUsage {
	if u, ok := usageMemGet(p); ok {
		return u
	}
	return readProfileUsageFromFile(p)
}

func readProfileUsageFromFile(p Profile) profileUsage {
	u := unknownProfileUsage()
	data, err := os.ReadFile(profileClaudeJSON(p))
	if err != nil {
		return u
	}
	var c usageCache
	if json.Unmarshal(data, &c) != nil {
		return u
	}
	return profileUsageFromPayload(c.CachedUsageUtilization.Utilization)
}

func unknownProfileUsage() profileUsage {
	return profileUsage{FiveHour: unknownUtilization, SevenDay: unknownUtilization}
}

func profileUsageFromPayload(p oauthUsagePayload) profileUsage {
	u := unknownProfileUsage()
	applyWindow(&u, p.FiveHour, true)
	applyWindow(&u, p.SevenDay, false)
	for _, lim := range p.Limits {
		switch lim.Kind {
		case "session":
			if !u.FiveHourKnown {
				u.FiveHour = percentFromFloat(lim.Percent)
				u.FiveHourKnown = true
				u.FiveHourResetAt = parseResetAt(lim.ResetsAt)
			}
		case "weekly_all":
			if !u.SevenDayKnown {
				u.SevenDay = percentFromFloat(lim.Percent)
				u.SevenDayKnown = true
			}
		}
	}
	return u
}

func applyWindow(u *profileUsage, w *usageWindow, fiveHour bool) {
	if w == nil || w.Utilization == nil {
		return
	}
	pct := percentFromFloat(*w.Utilization)
	if fiveHour {
		u.FiveHour = pct
		u.FiveHourKnown = true
		u.FiveHourResetAt = parseResetAt(w.ResetsAt)
		return
	}
	u.SevenDay = pct
	u.SevenDayKnown = true
}

func parseResetAt(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	return time.Time{}
}

func percentFromFloat(v float64) int {
	return clampPercent(int(math.Round(v)))
}

func clampPercent(v int) int {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// profileStat is the full input the selection policy needs for one profile.
// Keeping it a plain value makes chooseProfile a pure, unit-testable function.
type profileStat struct {
	Name          string
	Engine        string
	FiveHour      int
	SevenDay      int
	WorkingAgents int
	CooledUntil   time.Time // zero = available
}

// chooseProfile picks the profile a new session should run under: the lowest
// five-hour utilization wins, ties break on fewer working agents, then on name
// (so the choice is deterministic). Profiles still inside a rate-limit cooldown
// are excluded — unless every profile is cooling down, in which case the one
// whose cooldown ends soonest is used rather than refusing to dispatch.
func chooseProfile(stats []profileStat, now time.Time) string {
	if len(stats) == 0 {
		return ""
	}
	var open []profileStat
	for _, s := range stats {
		if s.CooledUntil.IsZero() || !s.CooledUntil.After(now) {
			open = append(open, s)
		}
	}
	if len(open) == 0 {
		best := stats[0]
		for _, s := range stats[1:] {
			if s.CooledUntil.Before(best.CooledUntil) ||
				(s.CooledUntil.Equal(best.CooledUntil) && s.Name < best.Name) {
				best = s
			}
		}
		return best.Name
	}
	best := open[0]
	for _, s := range open[1:] {
		if betterProfile(s, best) {
			best = s
		}
	}
	return best.Name
}

func betterProfile(a, b profileStat) bool {
	if a.FiveHour != b.FiveHour {
		return a.FiveHour < b.FiveHour
	}
	if a.WorkingAgents != b.WorkingAgents {
		return a.WorkingAgents < b.WorkingAgents
	}
	return a.Name < b.Name
}

// ---------------------------------------------------------------------------
// Rate-limit cooldowns
// ---------------------------------------------------------------------------

var (
	cooldownMu sync.Mutex
	cooldowns  = map[string]time.Time{} // profile name -> available again at
)

// noteProfileLimit puts a profile on cooldown after a limit signal: until its
// cached five-hour reset time when we have one, else defaultLimitCooldown.
func noteProfileLimit(p Profile, now time.Time) {
	until := now.Add(defaultLimitCooldown)
	if r := readProfileUsage(p).FiveHourResetAt; r.After(now) {
		until = r
	}
	cooldownMu.Lock()
	if cur, ok := cooldowns[p.Name]; !ok || until.After(cur) {
		cooldowns[p.Name] = until
	}
	cooldownMu.Unlock()
	hookLog("profile %s on usage cooldown until %s", p.Name, until.Format(time.RFC3339))
}

// profileCooledUntil returns when a profile becomes available again (zero when
// it is available now).
func profileCooledUntil(name string, now time.Time) time.Time {
	cooldownMu.Lock()
	defer cooldownMu.Unlock()
	until, ok := cooldowns[name]
	if !ok {
		return time.Time{}
	}
	if !until.After(now) {
		delete(cooldowns, name)
		return time.Time{}
	}
	return until
}

// collectProfileStats gathers the selection inputs for every profile. working
// is the number of turns currently running on each profile (DESIGN §4's
// "fewer working bots" tie-break); a nil map just means that input is unknown
// this round, which only affects ties.
func collectProfileStats(config *Config, working map[string]int, now time.Time) []profileStat {
	var stats []profileStat
	for _, p := range listProfiles(config) {
		u := readProfileUsage(p)
		stats = append(stats, profileStat{
			Name:          p.Name,
			Engine:        profileEngine(p),
			FiveHour:      u.FiveHour,
			SevenDay:      u.SevenDay,
			WorkingAgents: working[p.Name],
			CooledUntil:   profileCooledUntil(p.Name, now),
		})
	}
	return stats
}

// ---------------------------------------------------------------------------
// Disclaimer / login state
// ---------------------------------------------------------------------------

// bypassAccepted reports whether a profile has accepted the bypass-permissions
// disclaimer, which `claude --bg --dangerously-skip-permissions` requires once
// per config dir (2.1.259 refuses the launch otherwise, see bypassDisclaimerMsg).
//
// Acceptance is recorded as `skipDangerousModePermissionPrompt: true` in the
// profile's settings.json — which is exactly what acceptBypassDisclaimer
// writes, and what the interactive prompt writes; older installs recorded
// `bypassPermissionsModeAccepted` in .claude.json and Claude Code migrates that
// forward on startup, so both are accepted as proof. Returns ok=false when
// neither file can be read — "unknown", not "not accepted".
func bypassAccepted(p Profile) (accepted bool, ok bool) {
	readable := false
	if data, err := os.ReadFile(profileSettings(p)); err == nil {
		readable = true
		var s struct {
			Skip *bool `json:"skipDangerousModePermissionPrompt"`
		}
		if json.Unmarshal(data, &s) == nil && s.Skip != nil && *s.Skip {
			return true, true
		}
	}
	if data, err := os.ReadFile(profileClaudeJSON(p)); err == nil {
		readable = true
		var c struct {
			Accepted *bool `json:"bypassPermissionsModeAccepted"`
		}
		if json.Unmarshal(data, &c) == nil && c.Accepted != nil && *c.Accepted {
			return true, true
		}
	}
	return false, readable
}

// bypassSettingKey is the settings.json key Claude Code's interactive
// disclaimer writes when it is accepted. It is the whole of the acceptance:
// nothing else on disk changes (verified on 2.1.270 — writing this key by hand
// into a profile's settings.json is enough for
// `claude -p --permission-mode bypassPermissions` to run under it).
const bypassSettingKey = "skipDangerousModePermissionPrompt"

// acceptBypassDisclaimer records the bypass-permissions disclaimer for a
// profile by merging `"skipDangerousModePermissionPrompt": true` into its
// settings.json, and confirms the result with bypassAccepted().
//
// ccc used to drive Claude Code's interactive warning through a pseudo-terminal
// (DESIGN §14.23). Answering a TUI is fragile — a first-run theme picker, a
// renderer change or a reordered option is enough to leave a profile logged in
// but unusable — while the acceptance itself is one boolean in a file ccc
// already reads. Writing it directly is idempotent, needs no claude process and
// cannot half-happen.
//
// The profile's other settings are preserved (only key order is not, since the
// file is re-marshalled), the file is 0600, and it is replaced atomically so a
// concurrently-starting claude never reads a torn one. A profile with an empty
// config_dir resolves to ~/.claude/settings.json through claudeHome.
func acceptBypassDisclaimer(p Profile) error {
	if accepted, _ := bypassAccepted(p); accepted {
		return nil
	}
	path := profileSettings(p)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create the profile's config dir: %w", err)
	}
	settings := map[string]any{}
	switch data, err := os.ReadFile(path); {
	case err == nil:
		// An empty (or whitespace-only) file is not an error; it is a config
		// dir claude has touched but never written settings into.
		if len(strings.TrimSpace(string(data))) > 0 {
			if err := json.Unmarshal(data, &settings); err != nil {
				return fmt.Errorf("parse %s (fix or remove it, then retry): %w", path, err)
			}
		}
	case !os.IsNotExist(err):
		return fmt.Errorf("read %s: %w", path, err)
	}
	settings[bypassSettingKey] = true
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	if err := writeFileAtomic(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if accepted, _ := bypassAccepted(p); !accepted {
		return fmt.Errorf("%s was written but %s still does not record the acceptance", path, bypassSettingKey)
	}
	return nil
}

// writeFileAtomic writes data to a temp file in the same directory and renames
// it over path, so a reader never observes a partial file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()        // safe-ignore: best-effort cleanup on an error path
		os.Remove(tmpName) // safe-ignore: same
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()        // safe-ignore: same
		os.Remove(tmpName) // safe-ignore: same
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName) // safe-ignore: same
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName) // safe-ignore: same
		return err
	}
	return nil
}

// bypassDisclaimerMsg is the exact refusal Claude Code 2.1.259 prints when a
// config dir has not accepted the disclaimer yet (extracted from the binary).
const bypassDisclaimerMsg = "--bg with bypassPermissions requires accepting the disclaimer first. " +
	"Run `claude --dangerously-skip-permissions` once interactively."

// bypassDisclaimerHint tells the user how to accept the disclaimer for a
// specific profile. It names ccc's own command rather than the interactive
// `claude --dangerously-skip-permissions`: the acceptance is a settings.json
// key ccc writes itself (acceptBypassDisclaimer), with no TUI in the way.
func bypassDisclaimerHint(p Profile) string {
	return fmt.Sprintf("Run `ccc profile accept-disclaimer %s`, or `ccc doctor --fix`.", accountDisplay(p))
}

// isDisclaimerRefusal reports whether a dispatch error is the disclaimer gate,
// so callers can surface the actionable instruction instead of a raw failure.
func isDisclaimerRefusal(err error) bool {
	return err != nil && strings.Contains(err.Error(), "requires accepting the disclaimer")
}

// staleTokenMsg is the error Claude Code reports when a profile's OAuth token
// has gone stale. It reads like an org policy decision, but we have seen it
// resolve with nothing but a fresh `claude auth login` on the same account —
// so ccc reports it as "re-login needed", not as "your admin blocked you".
const staleTokenMsg = "organization has disabled Claude subscription access"

// isStaleTokenError reports whether a failure is the stale-token symptom above.
func isStaleTokenError(s string) bool {
	return strings.Contains(s, staleTokenMsg)
}

// grokAuthJSON is $GROK_HOME/auth.json — the file `grok login` writes.
func grokAuthJSON(p Profile) string { return filepath.Join(engineHome(p), "auth.json") }

func codexAuthJSON(p Profile) string { return filepath.Join(engineHome(p), "auth.json") }

// agyOAuthToken is the file-store token agy writes when
// GEMINI_FORCE_FILE_STORAGE=true (headless / no keyring).
func agyOAuthToken(p Profile) string {
	return filepath.Join(engineHome(p), ".gemini", "antigravity-cli", "antigravity-oauth-token")
}

// profileLoggedIn asks whether a profile is authenticated. Claude uses
// `claude auth status --json`. Grok looks at $GROK_HOME/auth.json. Antigravity
// looks at the isolated ~/.gemini token file. The answer is about THIS
// account's credentials, not a parent process leak.
func profileLoggedIn(p Profile) (loggedIn bool, account string, err error) {
	switch profileEngine(p) {
	case engineGrok:
		return grokLoggedIn(p)
	case engineAntigravity:
		return agyLoggedIn(p)
	case engineCodex:
		return codexLoggedIn(p)
	}
	// `auth status --json` exits 1 for a logged-out config dir while still
	// printing its JSON, so the payload is authoritative and the exit code is
	// only a fallback for "the command did not run at all".
	out, runErr := runClaudeOutput(p, 15*time.Second, "auth", "status", "--json")
	var st struct {
		LoggedIn bool   `json:"loggedIn"`
		Email    string `json:"email"`
		Account  string `json:"account"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		if runErr != nil {
			return false, "", runErr
		}
		return false, "", fmt.Errorf("cannot parse auth status: %w", err)
	}
	acct := st.Email
	if acct == "" {
		acct = st.Account
	}
	return st.LoggedIn, acct, nil
}

// grokLoggedIn reports whether $GROK_HOME/auth.json exists and looks like a
// session. The file is the authority: `grok login` writes it, and ccc never
// invents credentials.
func grokLoggedIn(p Profile) (bool, string, error) {
	data, err := os.ReadFile(grokAuthJSON(p))
	if err != nil {
		if os.IsNotExist(err) {
			return false, "", nil
		}
		return false, "", err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return false, "", nil
	}
	return true, accountHintFromJSON(data), nil
}

// codexLoggedIn reports whether $CODEX_HOME/auth.json exists. `codex login`
// writes it; ccc never invents credentials.
func codexLoggedIn(p Profile) (bool, string, error) {
	data, err := os.ReadFile(codexAuthJSON(p))
	if err != nil {
		if os.IsNotExist(err) {
			return false, "", nil
		}
		return false, "", err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return false, "", nil
	}
	return true, accountHintFromJSON(data), nil
}

// agyLoggedIn reports whether the isolated HOME has a file-store OAuth token.
// agy also keeps google_accounts.json under ~/.gemini; either is enough to
// treat the account as logged in.
func agyLoggedIn(p Profile) (bool, string, error) {
	home := engineHome(p)
	for _, rel := range []string{
		filepath.Join(".gemini", "antigravity-cli", "antigravity-oauth-token"),
		filepath.Join(".gemini", "oauth_creds.json"),
		filepath.Join(".gemini", "google_accounts.json"),
	} {
		data, err := os.ReadFile(filepath.Join(home, rel))
		if err != nil {
			continue
		}
		if len(strings.TrimSpace(string(data))) == 0 {
			continue
		}
		return true, accountHintFromJSON(data), nil
	}
	return false, "", nil
}

// accountHintFromJSON picks an email-looking field out of an auth blob so the
// status card can show more than the identity the owner typed. Missing or
// unlike-email values are fine: the account key is still the identity.
func accountHintFromJSON(data []byte) string {
	var blob map[string]any
	if json.Unmarshal(data, &blob) != nil {
		return ""
	}
	for _, key := range []string{"email", "account", "user", "username", "login"} {
		if s, ok := blob[key].(string); ok {
			s = strings.TrimSpace(s)
			if s != "" {
				return s
			}
		}
	}
	return ""
}
