package main

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// `ccc profile …` — manage the accounts ccc dispatches under (engine is set at
// add), plus the doctor section and the Telegram-side rendering of the same table.

func profileCommand(args []string) error {
	if len(args) == 0 {
		printProfileUsage()
		return nil
	}
	switch args[0] {
	case "list", "ls":
		fmt.Print(renderProfileTable(loadConfigOrNil(), true))
		return nil
	case "add":
		if len(args) < 3 {
			return fmt.Errorf("usage: ccc profile add <name> <config_dir> [--engine claude|grok|agy] [--label <label>]")
		}
		label := ""
		engine := engineClaude
		rest := args[3:]
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--label":
				if i+1 < len(rest) {
					label = rest[i+1]
					i++
				}
			case "--engine":
				if i+1 < len(rest) {
					e, err := parseEngine(rest[i+1])
					if err != nil {
						return err
					}
					engine = e
					i++
				}
			}
		}
		return profileAdd(args[1], args[2], label, engine)
	case "remove", "rm":
		if len(args) < 2 {
			return fmt.Errorf("usage: ccc profile remove <name>")
		}
		return profileRemove(args[1])
	case "default":
		if len(args) < 2 {
			return fmt.Errorf("usage: ccc profile default <name>")
		}
		return profileSetDefault(args[1])
	case "login":
		if len(args) < 2 {
			return fmt.Errorf("usage: ccc profile login <name>")
		}
		return profileLogin(args[1])
	case "accept-disclaimer":
		if len(args) < 2 {
			return fmt.Errorf("usage: ccc profile accept-disclaimer <name>")
		}
		return profileAcceptDisclaimer(args[1])
	default:
		printProfileUsage()
		return fmt.Errorf("unknown profile subcommand: %s", args[0])
	}
}

func printProfileUsage() {
	fmt.Println(`ccc profile - manage the accounts ccc runs agents under.

Engine is set when the account is added. Claude profiles are one
CLAUDE_CONFIG_DIR; Grok profiles are an isolated GROK_HOME; Antigravity
profiles are an isolated HOME. Failover stays inside one engine.

Telegram: /account add <identity> <engine>

    ccc profile list                              Show profiles, usage and login state
    ccc profile add <id> <dir> [--engine E] [--label X]
    ccc profile remove <id>                       Unregister an unused profile
    ccc profile default <id>                      Set the default account (new bots inherit its engine)
    ccc profile login <id>                        Run that engine's login (interactive)
    ccc profile accept-disclaimer <id>            Record the Claude bypass-permissions disclaimer`)
}

// loadConfigOrNil returns the config, or nil when there is none — every profile
// helper treats nil as "no profiles configured", i.e. the implicit default.
func loadConfigOrNil() *Config {
	config, err := loadConfig()
	if err != nil {
		return nil
	}
	return config
}

// profileRow is one rendered line of the profile table.
type profileRow struct {
	Profile      Profile
	Usage        profileUsage
	RunningTurns int
	LoggedIn     bool
	LoginKnown   bool
	Account      string
	CooledUntil  time.Time
}

// collectProfileRows gathers everything the table shows. probeLogin is slow (it
// shells out per profile), so the Telegram path can skip it.
func collectProfileRows(config *Config, probeLogin bool) []profileRow {
	now := time.Now()
	working := runningTurnsByProfile(config)
	var rows []profileRow
	for _, p := range listProfiles(config) {
		row := profileRow{
			Profile:      p,
			Usage:        readProfileUsage(p),
			RunningTurns: working[p.Name],
			CooledUntil:  profileCooledUntil(p.Name, now),
		}
		if probeLogin {
			in, acct, err := profileLoggedIn(p)
			row.LoggedIn, row.Account, row.LoginKnown = in, acct, err == nil
		}
		rows = append(rows, row)
	}
	return rows
}

func pct(v int, known bool) string {
	if !known {
		return "?"
	}
	return fmt.Sprintf("%d%%", v)
}

// renderProfileTable renders the profile list as plain aligned text, used by
// both `ccc profile list` and the /profiles Telegram command.
func renderProfileTable(config *Config, probeLogin bool) string {
	rows := collectProfileRows(config, probeLogin)
	def := defaultProfile(config).Name
	var sb strings.Builder
	header := []string{"ACCOUNT", "ENGINE", "LABEL", "CONFIG DIR", "5h", "7d", "TURNS", "LOGIN"}
	table := [][]string{header}
	for _, r := range rows {
		name := r.Profile.Name
		if name == def {
			name += " *"
		}
		login := "-"
		if probeLogin {
			switch {
			case !r.LoginKnown:
				login = "unknown"
			case r.LoggedIn && r.Account != "":
				login = r.Account
			case r.LoggedIn:
				login = "yes"
			default:
				login = "NO"
			}
		}
		working := fmt.Sprintf("%d", r.RunningTurns)
		table = append(table, []string{
			name,
			profileEngine(r.Profile),
			orDash(r.Profile.Label),
			r.Profile.ConfigDir,
			pct(r.Usage.FiveHour, r.Usage.FiveHourKnown),
			pct(r.Usage.SevenDay, r.Usage.SevenDayKnown),
			working,
			login,
		})
	}
	widths := make([]int, len(header))
	for _, row := range table {
		for i, cell := range row {
			if len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	for _, row := range table {
		for i, cell := range row {
			if i == len(row)-1 {
				sb.WriteString(cell)
			} else {
				sb.WriteString(fmt.Sprintf("%-*s  ", widths[i], cell))
			}
		}
		sb.WriteString("\n")
	}
	for _, r := range rows {
		if !r.CooledUntil.IsZero() {
			sb.WriteString(fmt.Sprintf("\n⏳ %s is on usage cooldown until %s\n", r.Profile.Name, r.CooledUntil.Format("15:04")))
		}
	}
	sb.WriteString("\n* = default profile. TURNS = turns running on it right now. Utilization comes from Claude Code's own cache and may be stale.\n")
	return sb.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func profileAdd(name, dir, label, engine string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("profile name cannot be empty")
	}
	dir = expandPath(dir)
	if !strings.HasPrefix(dir, "/") {
		abs, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("cannot resolve a relative config dir: %w", err)
		}
		dir = abs + "/" + dir
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("cannot create config dir %s: %w", dir, err)
	}
	config, err := loadConfig()
	if err != nil {
		return fmt.Errorf("no ccc config yet — run `ccc setup <bot_token>` first: %w", err)
	}
	if config.Profiles == nil {
		config.Profiles = map[string]*Profile{}
	}
	// The first explicit profile replaces the implicit one, so record the
	// previously-implicit dir too — otherwise existing sessions would silently
	// move to the new account.
	if len(config.Profiles) == 0 && name != defaultProfileName {
		// Record it with an EMPTY config_dir, not with ~/.claude spelled out:
		// pinning CLAUDE_CONFIG_DIR at the default location makes claude start
		// a fresh <dir>/.claude.json and lose the account's existing state.
		config.Profiles[defaultProfileName] = &Profile{Label: "claude's default config dir"}
		if config.DefaultProfile == "" {
			config.DefaultProfile = defaultProfileName
		}
	}
	engine, err = parseEngine(engine)
	if err != nil {
		return err
	}
	config.Profiles[name] = &Profile{Engine: engine, ConfigDir: dir, Label: label}
	if config.DefaultProfile == "" {
		config.DefaultProfile = name
	}
	if err := saveConfig(config); err != nil {
		return err
	}
	fmt.Printf("✅ profile %q (%s) → %s\n", name, engine, dir)
	if engine == engineClaude {
		if accepted, known := bypassAccepted(Profile{Name: name, Engine: engine, ConfigDir: dir}); !accepted {
			if known {
				fmt.Printf("⚠️  bypass-permissions disclaimer not accepted for this profile.\n   %s\n", bypassDisclaimerHint(Profile{Name: name, Engine: engine, ConfigDir: dir}))
			}
		}
	}
	fmt.Printf("Next: ccc profile login %s\n", name)
	return nil
}

func profileRemove(name string) error {
	config, err := loadConfig()
	if err != nil {
		return err
	}
	// An email, or the legacy name the profile is still keyed by.
	if p, ok := profileByName(config, name); ok {
		name = p.Name
	}
	if config.Profiles == nil || config.Profiles[name] == nil {
		return fmt.Errorf("no such profile: %s", name)
	}
	if busy, err := profileHasLiveTurns(config, name); err != nil {
		return err
	} else if len(busy) > 0 {
		sort.Strings(busy)
		return fmt.Errorf("profile %q is running a turn for %d bot(s): %s\n(wait for them, or /stop them in Telegram first)",
			name, len(busy), strings.Join(busy, ", "))
	}
	delete(config.Profiles, name)
	if config.DefaultProfile == name {
		config.DefaultProfile = ""
		for n := range config.Profiles {
			if config.DefaultProfile == "" || n < config.DefaultProfile {
				config.DefaultProfile = n
			}
		}
	}
	if err := saveConfig(config); err != nil {
		return err
	}
	fmt.Printf("✅ removed profile %q (its config dir was left on disk)\n", name)
	return nil
}

func profileSetDefault(name string) error {
	config, err := loadConfig()
	if err != nil {
		return err
	}
	p, ok := profileByName(config, name)
	if !ok {
		return fmt.Errorf("no such profile: %s", name)
	}
	config.DefaultProfile = p.Name
	if err := saveConfig(config); err != nil {
		return err
	}
	fmt.Printf("✅ default profile: %s\n", p.Name)
	return nil
}

// profileLogin hands the terminal to that engine's login under the account's
// isolated environment. It is interactive by design: ccc never handles
// credentials itself.
func profileLogin(name string) error {
	p, ok := profileByName(loadConfigOrNil(), name)
	if !ok {
		return fmt.Errorf("no such profile: %s", name)
	}
	home := engineHome(p)
	if err := os.MkdirAll(home, 0700); err != nil {
		return err
	}
	bin, args, err := loginCLIArgs(p)
	if err != nil {
		return err
	}
	fmt.Printf("Logging in profile %q (%s)…\n", p.Name, profileEngine(p))
	cmd := exec.Command(bin, args...)
	cmd.Env = engineEnv(nil, profileEngine(p), p)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func loginCLIArgs(p Profile) (string, []string, error) {
	switch profileEngine(p) {
	case engineGrok:
		bin, err := resolveEngineBin(engineGrok)
		return bin, []string{"login", "--device-auth"}, err
	case engineAntigravity:
		bin, err := resolveEngineBin(engineAntigravity)
		return bin, []string{"auth", "login"}, err
	default:
		return claudeBin(), []string{"auth", "login"}, nil
	}
}

// profileAcceptDisclaimer records the bypass-permissions disclaimer for one
// profile. It is idempotent, so running it on an already-accepted profile is a
// no-op that still reports the state.
func profileAcceptDisclaimer(name string) error {
	p, ok := profileByName(loadConfigOrNil(), name)
	if !ok {
		return fmt.Errorf("no such profile: %s", name)
	}
	if accepted, _ := bypassAccepted(p); accepted {
		fmt.Printf("✅ %s had already accepted the bypass-permissions disclaimer (%s)\n", p.Name, profileSettings(p))
		return nil
	}
	if err := acceptBypassDisclaimer(p); err != nil {
		return err
	}
	fmt.Printf("✅ bypass-permissions disclaimer accepted for %s (%s)\n", p.Name, profileSettings(p))
	return nil
}

// doctorProfiles prints the per-profile section of `ccc doctor`. Returns false
// when any profile has a problem that would stop ccc dispatching under it. With
// fix set, a missing disclaimer acceptance is written instead of only reported
// (`ccc doctor --fix`); nothing else is ever changed.
func doctorProfiles(fix bool) bool {
	config := loadConfigOrNil()
	profiles := listProfiles(config)
	def := defaultProfile(config).Name
	ok := true
	fmt.Printf("profiles.......... %d configured\n", len(profiles))
	for _, p := range profiles {
		marker := ""
		if p.Name == def {
			marker = " (default)"
		}
		fmt.Printf("  %s%s (%s)\n", p.Name, marker, profileEngine(p))

		fmt.Printf("    config dir.... ")
		home := engineHome(p)
		if st, err := os.Stat(home); err == nil && st.IsDir() {
			fmt.Printf("✅ %s\n", home)
		} else {
			fmt.Printf("❌ %s (missing)\n", home)
			ok = false
		}

		fmt.Printf("    login......... ")
		loggedIn, acct, err := profileLoggedIn(p)
		switch {
		case err != nil && isStaleTokenError(err.Error()):
			fmt.Println("❌ stale token — needs a fresh login")
			fmt.Printf("       Run: ccc profile login %s\n", p.Name)
			ok = false
		case err != nil:
			fmt.Printf("⚠️  unknown (%v)\n", err)
		case loggedIn && acct != "":
			fmt.Printf("✅ %s\n", acct)
		case loggedIn:
			fmt.Println("✅ logged in")
		default:
			fmt.Println("❌ not logged in")
			fmt.Printf("       Run: ccc profile login %s\n", p.Name)
			ok = false
		}

		if profileEngine(p) != engineClaude {
			continue
		}
		// The bypass-permissions disclaimer is accepted ONCE PER CONFIG DIR,
		// as one settings.json key ccc writes itself (acceptBypassDisclaimer).
		// Without it `--permission-mode bypassPermissions` is refused, so an
		// unaccepted profile is a real finding — and one --fix can close.
		fmt.Printf("    disclaimer.... ")
		accepted, known := bypassAccepted(p)
		switch {
		case accepted:
			fmt.Println("✅ bypass-permissions accepted")
		case fix:
			if err := acceptBypassDisclaimer(p); err != nil {
				fmt.Printf("❌ could not accept it: %v\n", err)
				ok = false
				break
			}
			fmt.Printf("🔧 accepted (written to %s)\n", profileSettings(p))
		case !known:
			fmt.Println("⚠️  unknown (no settings.json / .claude.json yet)")
			fmt.Printf("       %s\n", bypassDisclaimerHint(p))
		default:
			fmt.Println("❌ not accepted")
			fmt.Printf("       %s\n", bypassDisclaimerHint(p))
			ok = false
		}

		u := readProfileUsage(p)
		fmt.Printf("    usage......... 5h %s · 7d %s (Claude Code's cache, may be stale)\n",
			pct(u.FiveHour, u.FiveHourKnown), pct(u.SevenDay, u.SevenDayKnown))
	}
	return ok
}
