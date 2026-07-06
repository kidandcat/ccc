package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Shared helpers retained from the previous hook-based implementation. The
// transcript parser (extractRecentAssistantTexts) is the core of the pull-based
// mirror: the poller reads the JSONL tail and delivers new assistant text.

// htmlEscape escapes special HTML characters for Telegram HTML parse mode.
func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// truncate shortens a string to n characters (rune-safe enough for previews).
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// hookLog / listen debug log (kept name for continuity with existing logs).
func hookLog(format string, args ...interface{}) {
	f, err := os.OpenFile(filepath.Join(cacheDir(), "hook-debug.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[%s] %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}

// assistantTextBlock pairs extracted text with its requestId for dedup.
type assistantTextBlock struct {
	requestID string
	text      string
}

// extractRecentAssistantTexts reads the last N assistant entries from a Claude
// Code transcript (JSONL) and returns their text blocks. Callers use ledger
// dedup (by requestID + content hash) to avoid resending delivered messages.
func extractRecentAssistantTexts(transcriptPath string, tailCount int) []assistantTextBlock {
	if transcriptPath == "" {
		return nil
	}

	f, err := os.Open(transcriptPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	type transcriptLine struct {
		Type              string `json:"type"`
		RequestID         string `json:"requestId,omitempty"`
		IsApiErrorMessage bool   `json:"isApiErrorMessage,omitempty"`
		Message           struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}

	type contentBlock struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}

	const tailBytes = 512 * 1024
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	offset := int64(0)
	if fi.Size() > tailBytes {
		offset = fi.Size() - tailBytes
		f.Seek(offset, 0)
	}
	tailData, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	if offset > 0 {
		if idx := bytes.IndexByte(tailData, '\n'); idx >= 0 {
			tailData = tailData[idx+1:]
		}
	}

	type entry struct {
		requestID string
		content   json.RawMessage
	}

	var entries []entry
	for _, line := range bytes.Split(tailData, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var tl transcriptLine
		if json.Unmarshal(line, &tl) != nil {
			continue
		}
		if tl.Type != "assistant" || tl.Message.Role != "assistant" {
			continue
		}
		if tl.IsApiErrorMessage || tl.RequestID == "" {
			continue
		}
		entries = append(entries, entry{
			requestID: tl.RequestID,
			content:   tl.Message.Content,
		})
	}

	if len(entries) > tailCount {
		entries = entries[len(entries)-tailCount:]
	}

	type ridText struct {
		requestID string
		texts     []string
	}
	seen := make(map[string]int)
	var ordered []ridText

	for _, e := range entries {
		var blocks []contentBlock
		if json.Unmarshal(e.content, &blocks) != nil {
			continue
		}
		var texts []string
		for _, b := range blocks {
			if b.Type != "text" {
				continue
			}
			t := strings.TrimSpace(b.Text)
			if t != "" && t != "(no content)" {
				texts = append(texts, t)
			}
		}
		if len(texts) == 0 {
			continue
		}
		if idx, ok := seen[e.requestID]; ok {
			ordered[idx].texts = texts
		} else {
			seen[e.requestID] = len(ordered)
			ordered = append(ordered, ridText{requestID: e.requestID, texts: texts})
		}
	}

	var result []assistantTextBlock
	for _, rt := range ordered {
		for _, t := range rt.texts {
			result = append(result, assistantTextBlock{requestID: rt.requestID, text: t})
		}
	}
	return result
}

// installSkill installs the ccc-send file-transfer skill for Claude Code.
func installSkill() error {
	home, _ := os.UserHomeDir()
	skillDir := filepath.Join(home, ".claude", "skills")
	skillPath := filepath.Join(skillDir, "ccc-send.md")

	if err := os.MkdirAll(skillDir, 0755); err != nil {
		return fmt.Errorf("failed to create skills directory: %w", err)
	}

	skillContent := `# CCC Send - File Transfer Skill

## Description
Send files to the user via Telegram using the ccc send command.

## Usage
When the user asks you to send them a file, or when you have generated/built a file that the user needs (like an APK, binary, or any other file), use this command:

` + "```bash" + `
ccc send <file_path>
` + "```" + `

## How it works
- **Small files (< 50MB)**: Sent directly via Telegram
- **Large files (≥ 50MB)**: Streamed via relay server with a one-time download link

## Important Notes
- The command detects the current session from your working directory
- For large files, the command will wait up to 10 minutes for the user to download
- Use this proactively when you've created files the user needs!
`

	if err := os.WriteFile(skillPath, []byte(skillContent), 0644); err != nil {
		return fmt.Errorf("failed to write skill file: %w", err)
	}

	fmt.Println("✅ CCC send skill installed!")
	return nil
}

// uninstallSkill removes the ccc-send skill.
func uninstallSkill() error {
	home, _ := os.UserHomeDir()
	skillPath := filepath.Join(home, ".claude", "skills", "ccc-send.md")
	os.Remove(skillPath)
	return nil
}
