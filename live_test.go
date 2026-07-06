package main

import (
	"os"
	"testing"
	"time"
)

// TestLiveQuestionFlow exercises the real background-agent primitives against
// the local claude daemon. It is gated behind CCC_LIVE_TEST=1 because it spawns
// real Claude sessions (costs tokens/time) and requires an authenticated CLI.
//
//	CCC_LIVE_TEST=1 go test -run TestLiveQuestionFlow -v -timeout 300s
func TestLiveQuestionFlow(t *testing.T) {
	if os.Getenv("CCC_LIVE_TEST") != "1" {
		t.Skip("set CCC_LIVE_TEST=1 to run the live background-agent test")
	}

	dir := t.TempDir()

	// 1. Dispatch an agent that produces plain text (no questions).
	prompt := "Reply with a one-sentence greeting and nothing else. Do not ask any questions."
	short, err := dispatchAgent("ccc-live-test", dir, prompt)
	if err != nil {
		t.Fatalf("dispatchAgent: %v", err)
	}
	t.Logf("dispatched short=%s", short)
	defer stopAgent(short)

	// 2. Resolve the conversation UUID from the fleet.
	uuid := resolveSessionUUID(short, 15*time.Second)
	if uuid == "" {
		t.Fatalf("could not resolve session uuid for short=%s", short)
	}
	t.Logf("resolved uuid=%s", uuid)

	// 3. Wait for the transcript to appear and contain assistant text.
	var texts []assistantTextBlock
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if tp := transcriptPathForUUID(uuid); tp != "" {
			if b := extractRecentAssistantTexts(tp, 20); len(b) > 0 {
				texts = b
				break
			}
		}
		time.Sleep(2 * time.Second)
	}
	if len(texts) == 0 {
		t.Fatalf("never saw assistant text in the transcript")
	}
	t.Logf("delivered %d assistant text block(s), last=%q", len(texts), truncate(texts[len(texts)-1].text, 60))

	// 4. Resume the conversation and confirm a new short id (id changes on resume).
	newShort, err := resumeAgent(short, uuid, "ccc-live-test", dir, "Thanks, now stop.")
	if err != nil {
		t.Fatalf("resumeAgent: %v", err)
	}
	defer stopAgent(newShort)
	if newShort == short {
		t.Errorf("expected a new short id after resume, got same: %s", short)
	}
	t.Logf("resumed newShort=%s", newShort)
}
