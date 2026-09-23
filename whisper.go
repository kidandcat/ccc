//go:build voice

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mutablelogic/go-whisper/pkg/schema"
	whisper "github.com/mutablelogic/go-whisper/pkg/whisper"
)

const voiceSupported = true

const whisperModelName = "ggml-small.bin"
const whisperModelURL = "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-small.bin"

// whisperMu serializes the download. Two first voice notes used to Create
// the same .tmp and Rename it while the other was still writing.
var (
	whisperMu   sync.Mutex
	whisperHTTP = &http.Client{Timeout: 15 * time.Minute}
)

func getModelsDir() string {
	return filepath.Join(cacheDir(), "models")
}

// ensureModel downloads the whisper model if not present
func ensureModel() (string, error) {
	whisperMu.Lock()
	defer whisperMu.Unlock()
	modelsDir := getModelsDir()
	modelPath := filepath.Join(modelsDir, whisperModelName)
	if _, err := os.Stat(modelPath); err == nil {
		return modelPath, nil
	}

	if err := os.MkdirAll(modelsDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create models dir: %w", err)
	}

	fmt.Printf("Downloading whisper model %s...\n", whisperModelName)
	resp, err := whisperHTTP.Get(whisperModelURL)
	if err != nil {
		return "", fmt.Errorf("failed to download model: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("failed to download model: HTTP %d", resp.StatusCode)
	}

	f, err := os.CreateTemp(modelsDir, whisperModelName+".*")
	if err != nil {
		return "", fmt.Errorf("failed to create model file: %w", err)
	}
	tmpPath := f.Name()

	written, err := io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("failed to write model: %w", err)
	}

	if err := os.Rename(tmpPath, modelPath); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("failed to rename model: %w", err)
	}

	fmt.Printf("Model downloaded: %s (%d MB)\n", whisperModelName, written/1024/1024)
	return modelPath, nil
}

// transcribeAudio transcribes audio using native go-whisper
func transcribeAudio(config *Config, audioPath string) (string, error) {
	modelsDir := getModelsDir()

	// Ensure model exists
	if _, err := ensureModel(); err != nil {
		return "", fmt.Errorf("model setup failed: %w", err)
	}

	manager, err := whisper.New(modelsDir)
	if err != nil {
		return "", fmt.Errorf("failed to create whisper manager: %w", err)
	}
	defer manager.Close()

	model := manager.GetModelById("ggml-small")
	if model == nil {
		return "", fmt.Errorf("model ggml-small not found in %s", modelsDir)
	}

	var result strings.Builder
	err = manager.WithModel(model, func(task *whisper.Task) error {
		if config.TranscriptionLang != "" {
			if err := task.SetLanguage(config.TranscriptionLang); err != nil {
				return fmt.Errorf("failed to set language: %w", err)
			}
		}
		f, err := os.Open(audioPath)
		if err != nil {
			return fmt.Errorf("failed to open audio: %w", err)
		}
		defer f.Close()
		return task.TranscribeReader(context.Background(), f, func(seg *schema.Segment) {
			result.WriteString(seg.Text)
		})
	})
	if err != nil {
		return "", fmt.Errorf("transcription failed: %w", err)
	}

	return strings.TrimSpace(result.String()), nil
}

func doctorCheckWhisper() {
	fmt.Print("whisper model..... ")
	modelPath := filepath.Join(getModelsDir(), whisperModelName)
	if _, err := os.Stat(modelPath); err == nil {
		fmt.Printf("✅ %s\n", modelPath)
	} else {
		fmt.Println("⚠️  not downloaded (will auto-download on first voice message)")
		fmt.Println("   Model: " + whisperModelName)
	}
}
