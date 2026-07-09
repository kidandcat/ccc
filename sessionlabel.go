package main

import (
	"os"
	"path/filepath"
	"strings"
)

// sessionNamesDir / sessionLabel / carrySessionLabel — integration with the
// user's fleet-view labeling convention: Claude sessions record a short human
// label in ~/.claude/session-names/<uuid> (manual, written by the session
// itself), <uuid>.autolabel (heuristic fallback derived from the first prompt)
// and <uuid>.cwd (captured project dir). Everything degrades gracefully when
// the files/dir don't exist.

// sessionNamesDir returns ~/.claude/session-names.
func sessionNamesDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "session-names")
}

// sessionLabel returns the human label for a conversation UUID: the contents of
// <uuid> (the manual label) else <uuid>.autolabel (the heuristic fallback), with
// whitespace collapsed. Returns "" when the uuid is empty or no non-blank label
// file exists.
func sessionLabel(uuid string) string {
	if uuid == "" {
		return ""
	}
	dir := sessionNamesDir()
	for _, name := range []string{uuid, uuid + ".autolabel"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if s := strings.Join(strings.Fields(string(data)), " "); s != "" {
			return s
		}
	}
	return ""
}

// carrySessionLabel copies the user's label files (<uuid>, <uuid>.autolabel,
// <uuid>.cwd) from an old conversation UUID to a new one. A resume mints a fresh
// UUID, orphaning the labels; copying keeps the fleet title and topic title
// stable across resumes. No-op when either uuid is empty, they're equal, or the
// old file is missing/blank.
func carrySessionLabel(oldUUID, newUUID string) {
	if oldUUID == "" || newUUID == "" || oldUUID == newUUID {
		return
	}
	dir := sessionNamesDir()
	for _, suffix := range []string{"", ".autolabel", ".cwd"} {
		data, err := os.ReadFile(filepath.Join(dir, oldUUID+suffix))
		if err != nil || len(strings.TrimSpace(string(data))) == 0 {
			continue
		}
		os.WriteFile(filepath.Join(dir, newUUID+suffix), data, 0644) // safe-ignore: label continuity is best-effort cosmetics
	}
}
