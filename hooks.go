package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// AskUserQuestion handling. Installed as a PreToolUse hook via --settings on
// every ccc bg agent (see agents.go ccSettingsJSON). When Claude is about to
// ask the user a question, this hook fires with the full structured questions
// in stdin — the one reliable place to get the options, since they are NOT
// written to the transcript while the agent is blocked.
//
// The hook renders the options as Telegram inline buttons, then BLOCKS until
// the user taps (the listener writes answer files), and returns the chosen
// answer to Claude as the tool decision. No resume / session churn is needed —
// the agent stays alive and continues with the answer.

const questionWaitTimeout = 10 * time.Minute

func qPendingPath(sid8 string) string      { return filepath.Join(cacheDir(), "qpending-"+sid8) }
func qMetaPath(sid8 string) string         { return filepath.Join(cacheDir(), "qmeta-"+sid8+".json") }
func qAnswerPath(sid8 string, q int) string {
	return filepath.Join(cacheDir(), fmt.Sprintf("qans-%s-%d", sid8, q))
}

// questionPending reports whether an AskUserQuestion hook is currently waiting
// on the user for the given short session id.
func questionPending(sid8 string) bool {
	_, err := os.Stat(qPendingPath(sid8))
	return err == nil
}

// qMeta is persisted so the listener can show the chosen label on a tap.
type qMeta struct {
	TopicID   int64         `json:"topic_id"`
	SessName  string        `json:"session"`
	Questions []qMetaAnswer `json:"questions"`
}
type qMetaAnswer struct {
	Header  string   `json:"header"`
	Options []string `json:"options"`
}

func writeQMeta(sid8 string, m *qMeta) {
	data, _ := json.Marshal(m)
	os.WriteFile(qMetaPath(sid8), data, 0600)
}
func readQMeta(sid8 string) *qMeta {
	data, err := os.ReadFile(qMetaPath(sid8))
	if err != nil {
		return nil
	}
	var m qMeta
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	return &m
}

// handleAskQuestionHook is the `ccc hook-question` PreToolUse handler.
func handleAskQuestionHook() error {
	defer func() { recover() }()

	raw, _ := readHookStdin()
	if len(raw) == 0 {
		outputPermissionDecision("allow", "no hook data")
		return nil
	}
	hd, err := parseHookData(raw)
	if err != nil {
		outputPermissionDecision("allow", "unparseable hook data")
		return nil
	}

	config, err := loadConfig()
	if err != nil || config == nil {
		outputPermissionDecision("allow", "no config")
		return nil
	}

	sessName, topicID := findSessionByCwd(config, hd.Cwd)
	if sessName == "" || config.GroupID == 0 || topicID == 0 {
		// Not a ccc-managed session — let Claude handle it normally.
		outputPermissionDecision("allow", "not a ccc session")
		return nil
	}

	questions := hd.ToolInput.Questions
	if len(questions) == 0 {
		outputPermissionDecision("allow", "no questions")
		return nil
	}

	sid8 := shortOf(hd.SessionID)

	// Persist metadata for the listener + mark this session as awaiting input.
	meta := &qMeta{TopicID: topicID, SessName: sessName}
	for _, q := range questions {
		var opts []string
		for _, o := range q.Options {
			opts = append(opts, o.Label)
		}
		meta.Questions = append(meta.Questions, qMetaAnswer{Header: q.Header, Options: opts})
	}
	writeQMeta(sid8, meta)
	os.WriteFile(qPendingPath(sid8), []byte("1"), 0600)
	// Clear any stale answers.
	for i := range questions {
		os.Remove(qAnswerPath(sid8, i))
	}

	// Render one message with buttons per question.
	expected := 0
	for qIdx, q := range questions {
		if q.Question == "" {
			continue
		}
		expected++
		msg := fmt.Sprintf("❓ *%s*\n%s", firstNonEmpty(q.Header, "Question"), q.Question)
		var buttons [][]InlineKeyboardButton
		for i, opt := range q.Options {
			if opt.Label == "" {
				continue
			}
			cb := fmt.Sprintf("q:%s:%d:%d", sid8, qIdx, i)
			buttons = append(buttons, []InlineKeyboardButton{{Text: opt.Label, CallbackData: cb}})
		}
		if len(buttons) > 0 {
			sendMessageWithKeyboard(config, config.GroupID, topicID, msg, buttons)
		}
	}
	if expected == 0 {
		cleanupQuestion(sid8, len(questions))
		outputPermissionDecision("allow", "no answerable questions")
		return nil
	}

	// Block until every question is answered (or timeout).
	deadline := time.Now().Add(questionWaitTimeout)
	answers := map[int]string{}
	for len(answers) < expected && time.Now().Before(deadline) {
		for qIdx, q := range questions {
			if q.Question == "" {
				continue
			}
			if _, ok := answers[qIdx]; ok {
				continue
			}
			data, err := os.ReadFile(qAnswerPath(sid8, qIdx))
			if err != nil {
				continue
			}
			optIdx, _ := strconv.Atoi(strings.TrimSpace(string(data)))
			label := fmt.Sprintf("option %d", optIdx+1)
			if optIdx >= 0 && optIdx < len(q.Options) {
				label = q.Options[optIdx].Label
			}
			answers[qIdx] = label
		}
		if len(answers) < expected {
			time.Sleep(500 * time.Millisecond)
		}
	}

	cleanupQuestion(sid8, len(questions))

	if len(answers) < expected {
		outputPermissionDecision("deny", "The user did not answer in time. Proceed with your best judgment, or ask again if you must.")
		return nil
	}

	// Build the answer feedback for Claude.
	var lines []string
	for qIdx, q := range questions {
		if q.Question == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: %s", firstNonEmpty(q.Header, "Answer"), answers[qIdx]))
	}
	reason := "The user answered via Telegram — " + strings.Join(lines, "; ") + ". Use these choices and proceed; do not ask this again."
	outputPermissionDecision("deny", reason)
	return nil
}

func cleanupQuestion(sid8 string, nQuestions int) {
	os.Remove(qPendingPath(sid8))
	os.Remove(qMetaPath(sid8))
	for i := 0; i < nQuestions; i++ {
		os.Remove(qAnswerPath(sid8, i))
	}
}

// outputPermissionDecision writes a PreToolUse hook decision to stdout.
func outputPermissionDecision(decision, reason string) {
	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       decision,
			"permissionDecisionReason": reason,
		},
	}
	json.NewEncoder(os.Stdout).Encode(out)
}

// readHookStdin reads stdin JSON with a short timeout.
func readHookStdin() ([]byte, error) {
	ch := make(chan []byte, 1)
	go func() {
		defer func() { recover() }()
		data, _ := io.ReadAll(os.Stdin)
		ch <- data
	}()
	select {
	case data := <-ch:
		return data, nil
	case <-time.After(3 * time.Second):
		return nil, nil
	}
}

// findSessionByCwd matches a hook's cwd to a configured session.
func findSessionByCwd(config *Config, cwd string) (string, int64) {
	for name, info := range config.Sessions {
		if name == "" || info == nil || info.Path == "" {
			continue
		}
		if cwd == info.Path || strings.HasPrefix(cwd, info.Path+"/") {
			return name, info.TopicID
		}
	}
	return "", 0
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
