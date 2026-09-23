package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const maxTelegramFileSize = 50 * 1024 * 1024 // 50MB
const defaultRelayURL = "https://ccc-relay.fly.dev"

// handleSendFile sends a file to the current session's Telegram topic
func handleSendFile(filePath string) error {
	config, err := loadConfig()
	if err != nil {
		return fmt.Errorf("no config found: %w", err)
	}

	// Get absolute path
	if !filepath.IsAbs(filePath) {
		cwd, _ := os.Getwd()
		filePath = filepath.Join(cwd, filePath)
	}

	// Check file exists
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("file not found: %w", err)
	}

	// Find the bot that owns the current directory.
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cannot resolve the working directory: %w", err)
	}
	db, err := openStore(dbPath(config))
	if err != nil {
		return fmt.Errorf("open the ccc database: %w", err)
	}
	defer closeStore(db)
	bot, err := botByCwd(db, cwd)
	if err != nil {
		return fmt.Errorf("no bot owns %s — run this from a bot's working directory", cwd)
	}
	sessionName := bot.Name
	chat, thread, ok := destForTopic(config, 0)
	if !ok {
		return fmt.Errorf("no Telegram destination configured")
	}

	fileName := filepath.Base(filePath)
	fileSize := fileInfo.Size()

	// Small file: send directly via Telegram
	if fileSize < maxTelegramFileSize {
		fmt.Printf("📤 Sending %s (%d MB) via Telegram...\n", fileName, fileSize/(1024*1024))
		return sendFile(config, chat, thread, filePath, "")
	}

	// Large file: use streaming relay
	relayURL := config.RelayURL
	if relayURL == "" {
		relayURL = defaultRelayURL
	}

	fmt.Printf("📤 Preparing %s (%d MB) for streaming relay...\n", fileName, fileSize/(1024*1024))

	// Generate one-time token
	tokenBytes := make([]byte, 16)
	rand.Read(tokenBytes)
	token := hex.EncodeToString(tokenBytes)

	// Register with relay
	regPayload, _ := json.Marshal(map[string]interface{}{
		"token":    token,
		"filename": fileName,
		"size":     fileSize,
	})
	regData := string(regPayload)
	resp, err := http.Post(relayURL+"/register", "application/json", strings.NewReader(regData))
	if err != nil {
		return fmt.Errorf("failed to register with relay: %w", err)
	}
	resp.Body.Close()

	// Send download link to Telegram (include filename in URL for browser compatibility)
	downloadURL := fmt.Sprintf("%s/d/%s/%s", relayURL, token, fileName)
	msg := fmt.Sprintf("📦 %s (%d MB)\n\n🔗 Download:\n%s", fileName, fileSize/(1024*1024), downloadURL)

	fmt.Printf("📤 Sending link to %s...\n", sessionName)
	if err := sendMessage(config, chat, thread, msg); err != nil {
		return err
	}

	// Wait for download request and stream
	fmt.Printf("⏳ Waiting for download (link expires in 10 min)...\n")
	return streamFileToRelay(relayURL, token, filePath, fileName, fileSize)
}

func streamFileToRelay(relayURL, token, filePath, fileName string, fileSize int64) error {
	// Poll for download requests - loop to allow multiple downloads
	timeout := time.After(10 * time.Minute)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	downloadCount := 0

	for {
		select {
		case <-timeout:
			http.Get(relayURL + "/cancel/" + token)
			if downloadCount > 0 {
				fmt.Printf("⏰ Session expired after %d download(s)\n", downloadCount)
				return nil
			}
			return fmt.Errorf("download timed out (10 min)")
		case <-ticker.C:
			resp, err := http.Get(relayURL + "/status/" + token)
			if err != nil {
				continue
			}
			body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
			resp.Body.Close()

			status := string(body)
			if status == "waiting" {
				continue
			} else if status == "ready" {
				// Someone requested download, start streaming
				downloadCount++
				fmt.Printf("📤 Streaming %s (download #%d)...\n", fileName, downloadCount)

				file, err := os.Open(filePath)
				if err != nil {
					return err
				}

				// Stream to relay
				req, _ := http.NewRequest("POST", relayURL+"/stream/"+token, file)
				req.Header.Set("Content-Type", "application/octet-stream")
				req.Header.Set("X-Filename", fileName)
				req.ContentLength = fileSize

				client := &http.Client{Timeout: 30 * time.Minute}
				streamResp, err := client.Do(req)
				file.Close()
				if err != nil {
					fmt.Printf("⚠️ Streaming error: %v\n", err)
					continue
				}
				streamResp.Body.Close()

				fmt.Printf("✅ Download #%d complete! Waiting for more requests...\n", downloadCount)
				// Continue looping for more downloads
			} else if status == "cancelled" || status == "not_found" {
				if downloadCount > 0 {
					return nil
				}
				return fmt.Errorf("transfer %s", status)
			}
		}
	}
}

// Relay server - streams from sender to receiver without storing
var relayTransfers = struct {
	sync.RWMutex
	transfers map[string]*relayTransfer
}{transfers: make(map[string]*relayTransfer)}

type relayTransfer struct {
	Token    string
	Filename string
	Size     int64
	Status   string // "waiting", "ready", "streaming", "done", "cancelled"
	Created  time.Time
	DataChan chan []byte
	DoneChan chan struct{}
	// receiver is set while one /d/ request is reading DataChan. A second
	// concurrent download would split the bytes and double-close DoneChan.
	receiver bool
}

func runRelayServer(port string) {
	// Clean up old transfers periodically
	go func() {
		for {
			time.Sleep(1 * time.Minute)
			relayTransfers.Lock()
			for token, t := range relayTransfers.transfers {
				if time.Since(t.Created) > 15*time.Minute {
					t.Status = "cancelled"
					select {
					case <-t.DoneChan:
					default:
						close(t.DoneChan)
					}
					delete(relayTransfers.transfers, token)
				}
			}
			relayTransfers.Unlock()
		}
	}()

	// Register a new transfer
	http.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var data struct {
			Token    string `json:"token"`
			Filename string `json:"filename"`
			Size     int64  `json:"size"`
		}
		if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}

		relayTransfers.Lock()
		relayTransfers.transfers[data.Token] = &relayTransfer{
			Token:    data.Token,
			Filename: data.Filename,
			Size:     data.Size,
			Status:   "waiting",
			Created:  time.Now(),
			DataChan: make(chan []byte, 100),
			DoneChan: make(chan struct{}),
		}
		relayTransfers.Unlock()

		fmt.Printf("📋 Registered: %s (%s)\n", data.Filename, data.Token[:8])
		w.WriteHeader(http.StatusOK)
	})

	// Check transfer status
	http.HandleFunc("/status/", func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.URL.Path, "/status/")
		relayTransfers.RLock()
		t, exists := relayTransfers.transfers[token]
		relayTransfers.RUnlock()

		if !exists {
			fmt.Fprint(w, "not_found")
			return
		}
		fmt.Fprint(w, t.Status)
	})

	// Cancel transfer
	http.HandleFunc("/cancel/", func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.URL.Path, "/cancel/")
		relayTransfers.Lock()
		if t, exists := relayTransfers.transfers[token]; exists {
			t.Status = "cancelled"
			select {
			case <-t.DoneChan:
			default:
				close(t.DoneChan)
			}
			delete(relayTransfers.transfers, token)
		}
		relayTransfers.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	// Sender streams file data
	http.HandleFunc("/stream/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		token := strings.TrimPrefix(r.URL.Path, "/stream/")
		relayTransfers.Lock()
		t, exists := relayTransfers.transfers[token]
		if !exists || t.Status != "ready" {
			relayTransfers.Unlock()
			http.Error(w, "Transfer not ready", http.StatusBadRequest)
			return
		}
		t.Status = "streaming"
		relayTransfers.Unlock()
		fmt.Printf("📤 Streaming: %s (%s)\n", t.Filename, token[:8])

		var bytesSent int64
		// Read from sender and send to channel
		buf := make([]byte, 32*1024)
		for {
			n, err := r.Body.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				bytesSent += int64(n)
				select {
				case t.DataChan <- data:
				case <-t.DoneChan:
					// Receiver finished/disconnected early
					fmt.Printf("📤 Receiver done early: %s (%s) after %d bytes\n", t.Filename, token[:8], bytesSent)
					return
				}
			}
			if err != nil {
				break
			}
		}
		close(t.DataChan)
		<-t.DoneChan // Wait for receiver to finish

		// DON'T delete transfer - allow multiple downloads
		// Transfer is cleaned up by timeout goroutine or /cancel endpoint
		fmt.Printf("✅ Stream complete: %s (%s) - %d bytes\n", t.Filename, token[:8], bytesSent)
	})

	// Download endpoint - receiver gets file
	http.HandleFunc("/d/", func(w http.ResponseWriter, r *http.Request) {
		// Ignore Telegram link preview bots and HEAD requests
		ua := r.UserAgent()
		if strings.Contains(ua, "TelegramBot") || strings.Contains(ua, "Telegram") {
			fmt.Printf("🚫 Ignored Telegram preview bot: %s\n", ua)
			http.Error(w, "Preview not available", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodHead {
			fmt.Printf("🚫 Ignored HEAD request\n")
			w.WriteHeader(http.StatusOK)
			return
		}

		// URL format: /d/{token}/{filename} - extract just the token
		pathParts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/d/"), "/", 2)
		token := pathParts[0]
		relayTransfers.Lock()
		t, exists := relayTransfers.transfers[token]
		if !exists {
			relayTransfers.Unlock()
			http.Error(w, "File not found - sender may have disconnected", http.StatusNotFound)
			return
		}
		if t.receiver || (t.Status != "waiting" && t.Status != "ready") {
			relayTransfers.Unlock()
			http.Error(w, "Transfer in progress, please wait and retry", http.StatusConflict)
			return
		}
		if t.Status == "waiting" {
			t.Status = "ready"
			// Create fresh channels for this download
			t.DataChan = make(chan []byte, 100)
			t.DoneChan = make(chan struct{})
		}
		t.receiver = true
		relayTransfers.Unlock()

		fmt.Printf("📥 Download started: %s (%s) from %s\n", t.Filename, token[:8], r.UserAgent())

		// Sanitize filename: remove quotes, newlines, and control characters
		safeName := strings.Map(func(r rune) rune {
			if r == '"' || r == '\n' || r == '\r' || r < 32 {
				return '_'
			}
			return r
		}, t.Filename)
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, safeName))
		w.Header().Set("Content-Type", "application/octet-stream")
		if t.Size > 0 {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", t.Size))
		}

		flusher, _ := w.(http.Flusher)
		ctx := r.Context()
		var bytesWritten int64
		var writeErr error

		// Stream data from sender to receiver
	downloadLoop:
		for {
			select {
			case <-ctx.Done():
				// Client disconnected
				fmt.Printf("❌ Client disconnected: %s (%s) after %d bytes\n", t.Filename, token[:8], bytesWritten)
				writeErr = ctx.Err()
				break downloadLoop
			case data, ok := <-t.DataChan:
				if !ok {
					// Channel closed, transfer complete
					break downloadLoop
				}
				n, err := w.Write(data)
				bytesWritten += int64(n)
				if err != nil {
					fmt.Printf("❌ Write error: %s (%s) after %d bytes: %v\n", t.Filename, token[:8], bytesWritten, err)
					writeErr = err
					break downloadLoop
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
		}

		// Signal sender we're done (success or failure)
		relayTransfers.Lock()
		if cur, exists := relayTransfers.transfers[token]; exists {
			cur.receiver = false
			select {
			case <-cur.DoneChan:
			default:
				close(cur.DoneChan)
			}
			cur.Status = "waiting"
			if writeErr == nil {
				fmt.Printf("📥 Download complete: %s (%s) - %d bytes sent\n", cur.Filename, token[:8], bytesWritten)
			} else {
				fmt.Printf("📥 Download failed: %s (%s) - allowing retry\n", cur.Filename, token[:8])
			}
		}
		relayTransfers.Unlock()
	})

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "OK")
	})

	fmt.Printf("🚀 Streaming relay server on :%s\n", port)
	fmt.Println("   No files stored - direct sender→relay→receiver streaming!")
	http.ListenAndServe(":"+port, nil)
}
