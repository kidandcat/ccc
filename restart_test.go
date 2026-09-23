package main

import (
	"strings"
	"testing"
)

func TestServiceRestartCommand(t *testing.T) {
	t.Parallel()
	name, args := serviceRestartCommand(true, 501)
	if name != "launchctl" || strings.Join(args, " ") != "kickstart -k gui/501/com.ccc" {
		t.Errorf("mac = %s %v", name, args)
	}
	name, args = serviceRestartCommand(false, 501)
	if name != "systemctl" || strings.Join(args, " ") != "--user restart ccc" {
		t.Errorf("linux = %s %v", name, args)
	}
}

func TestRestartCommandIsOwnerOnlyAndAcksOffset(t *testing.T) {
	in, _, api := testInstance(t)
	in.config().AllowedUserIDs = []int64{99}
	in.nextUpdateOffset = 77

	var exited int
	prev := stopListen
	stopListen = func() { exited++ }
	t.Cleanup(func() { stopListen = prev })

	in.handleMessage(dmMessage(99, "/restart"))
	if exited != 0 {
		t.Fatal("a non-owner restarted the service")
	}
	if !strings.Contains(strings.Join(api.texts(""), "\n"), "owner-only") {
		t.Fatalf("non-owner reply:\n%s", strings.Join(api.texts(""), "\n"))
	}
	if got := api.since("getUpdates"); len(got) != 0 {
		t.Fatalf("non-owner confirmed the offset: %+v", got)
	}

	in.handleMessage(ownerMessage("/restart"))
	if exited != 1 {
		t.Fatalf("owner restart exits = %d, want 1", exited)
	}
	body := strings.Join(api.texts(""), "\n")
	if !strings.Contains(body, "Restarting") {
		t.Errorf("owner did not see the restart reply:\n%s", body)
	}
	acks := api.since("getUpdates")
	if len(acks) != 1 {
		t.Fatalf("getUpdates calls = %d, want 1", len(acks))
	}
	if got := acks[0].Params.Get("offset"); got != "77" {
		t.Errorf("confirmed offset = %q, want 77", got)
	}
	if got := acks[0].Params.Get("timeout"); got != "0" {
		t.Errorf("confirm timeout = %q, want 0 (must not long-poll)", got)
	}
}

func TestHelpMentionsRestart(t *testing.T) {
	t.Parallel()
	help := helpText()
	if !strings.Contains(help, "ccc restart") && !strings.Contains(help, "\n    restart") {
		t.Errorf("help does not document the shell command:\n%s", help)
	}
	if !strings.Contains(help, "/restart") {
		t.Errorf("help does not document /restart:\n%s", help)
	}
}
