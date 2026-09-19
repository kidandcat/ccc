package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// bannedBranding is the Hairok decision: the product is "ccc" with no
// expansion. Tests may mention these phrases only to assert they are absent.
var bannedBranding = regexp.MustCompile(`(?i)crew command|command center|crew of`)

func TestUserFacingCopyHasNoCrewBranding(t *testing.T) {
	t.Parallel()

	blobs := map[string]string{
		"ccc --help":   helpText(),
		"systemd unit": renderSystemdUnit("/usr/bin/ccc", &Config{}),
	}
	for name, body := range blobs {
		if loc := bannedBranding.FindString(body); loc != "" {
			t.Errorf("%s still says %q", name, loc)
		}
	}

	for _, path := range []string{"README.md", "docs/index.html", "docs/DESIGN.md"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if loc := bannedBranding.FindString(string(raw)); loc != "" {
			t.Errorf("%s still says %q", path, loc)
		}
	}

	help := helpText()
	if !strings.Contains(help, "ccc — sessions in Telegram") {
		t.Error("help banner should be «ccc — sessions in Telegram» with no expansion")
	}
	if strings.Contains(help, "/role") {
		t.Error("help still advertises /role")
	}
}
