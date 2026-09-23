package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func installService() error {
	home, _ := os.UserHomeDir()

	// The service will not have the owner's login shell, so snapshot the
	// env_passthrough secrets into <config_dir>/env first (see envfile.go). It
	// is printed by name, so the owner sees immediately whether `ccc install`
	// was run from a login shell.
	if config := loadConfigOrNil(); config != nil && len(passthroughNames(config)) > 0 {
		res, err := syncEnvFile(config)
		if err != nil {
			return fmt.Errorf("write the env file: %w", err)
		}
		fmt.Print(res.String())
	}

	// Detect OS and install appropriate service
	if _, err := os.Stat("/Library"); err == nil {
		// macOS - use launchd
		return installLaunchdService(home)
	}
	// Linux - use systemd
	return installSystemdService(home)
}

func installLaunchdService(home string) error {
	plistDir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(plistDir, 0755); err != nil {
		return fmt.Errorf("failed to create LaunchAgents dir: %w", err)
	}

	plistPath := filepath.Join(plistDir, "com.ccc.plist")
	logPath := filepath.Join(cacheDir(), "ccc.log")

	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.ccc</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>listen</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`, cccPath, logPath, logPath)

	if err := os.WriteFile(plistPath, []byte(plist), 0644); err != nil {
		return fmt.Errorf("failed to write plist: %w", err)
	}

	// Unload if exists, then load
	exec.Command("launchctl", "unload", plistPath).Run()
	if err := exec.Command("launchctl", "load", plistPath).Run(); err != nil {
		return fmt.Errorf("failed to load service: %w", err)
	}

	fmt.Println("✅ Service installed and started (launchd)")
	return nil
}

// installSystemdService writes a systemd USER unit. User rather than system:
// the profiles, the data dir and the credentials all live in the owner's home,
// and `claude` resolves them from $HOME. Enable lingering
// (`loginctl enable-linger <user>`) so it survives logout on a VM.
func installSystemdService(home string) error {
	serviceDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(serviceDir, 0755); err != nil {
		return fmt.Errorf("failed to create systemd dir: %w", err)
	}

	servicePath := filepath.Join(serviceDir, "ccc.service")
	service := renderSystemdUnit(cccPath, loadConfigOrNil())
	if err := os.WriteFile(servicePath, []byte(service), 0644); err != nil {
		return fmt.Errorf("failed to write service file: %w", err)
	}

	exec.Command("systemctl", "--user", "daemon-reload").Run() // safe-ignore: a failure surfaces on the start below
	exec.Command("systemctl", "--user", "enable", "ccc").Run() // safe-ignore: same
	if err := exec.Command("systemctl", "--user", "start", "ccc").Run(); err != nil {
		return fmt.Errorf("failed to start service: %w (is XDG_RUNTIME_DIR set? see the README)", err)
	}

	fmt.Printf("✅ Service installed and started (systemd --user): %s\n", servicePath)
	fmt.Println("   Make it survive logout: loginctl enable-linger $USER")
	return nil
}

// serviceRestartCommand is how an already-installed service is bounced so the
// binary now on disk (make install overwrites ~/bin/ccc while listen is still
// mapped to the old inode) is the one that comes back. macOS is launchd's
// kickstart -k; Linux is systemd --user. KeepAlive / Restart= bring it up
// either way, but kickstart and systemctl restart do not wait on a crash loop.
func serviceRestartCommand(mac bool, uid int) (string, []string) {
	if mac {
		return "launchctl", []string{"kickstart", "-k", fmt.Sprintf("gui/%d/com.ccc", uid)}
	}
	return "systemctl", []string{"--user", "restart", "ccc"}
}

// restartService stops the running `ccc listen` and starts it again.
// The caller is a different process from the service (the shell after
// `make install`, or a bot), so this must not os.Exit itself.
func restartService() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("home directory: %w", err)
	}
	mac := isMacOS()
	if mac {
		if _, err := os.Stat(filepath.Join(home, "Library", "LaunchAgents", "com.ccc.plist")); err != nil {
			return fmt.Errorf("service is not installed — run: ccc install")
		}
	} else if _, err := os.Stat(filepath.Join(home, ".config", "systemd", "user", "ccc.service")); err != nil {
		return fmt.Errorf("service is not installed — run: ccc install")
	}
	name, args := serviceRestartCommand(mac, os.Getuid())
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
		}
		return fmt.Errorf("%s %s: %w (%s)", name, strings.Join(args, " "), err, msg)
	}
	fmt.Println("Restarted ccc. It comes back on its own; in-flight turns are retried.")
	return nil
}

// renderSystemdUnit builds the unit text. It holds NO secrets: the
// env_passthrough values (DESIGN §3.1) live in <config_dir>/env, written 0600
// by `ccc env sync`, and the unit reads them with `EnvironmentFile=-` (the `-`
// means "carry on if it is not there").
//
// Earlier versions baked `Environment="NAME=value"` lines in here at install
// time. That was wrong twice over: it wrote live tokens into a world-readable
// unit file under ~/.config/systemd, and it captured only the variables that
// happened to be set in whatever shell ran `ccc install` — `systemctl --user`
// never sources ~/.profile or ~/.zshrc, so the usual answer of "export it in my
// shell rc" silently produced a service with no secrets at all.
//
// Everything else a bot may see is built by claudeEnv at spawn time, not here.
func renderSystemdUnit(binary string, config *Config) string {
	var env strings.Builder
	if len(passthroughNames(config)) > 0 {
		// %h is systemd's specifier for the user's home, which is where
		// configDir() lives; writing it that way keeps the unit correct if the
		// home is ever mounted somewhere else.
		env.WriteString("EnvironmentFile=-%h/.config/ccc/env\n")
	}
	return fmt.Sprintf(`[Unit]
Description=ccc — sessions in Telegram
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s listen
Restart=always
RestartSec=10
KillMode=mixed
TimeoutStopSec=30
%s
[Install]
WantedBy=default.target
`, binary, env.String())
}
