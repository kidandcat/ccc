package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"gorm.io/gorm"
)

// hubClient is the instance side of the public hub: one outbound websocket
// the phone can reach even when this machine is behind NAT.

type hubClient struct {
	in   *instance
	id   hubIdentity
	url  string
	name string

	mu    sync.Mutex
	wmu   sync.Mutex
	conn  *websocket.Conn
	peers map[string][32]byte // device pk hex -> pubkey

	progMu sync.Mutex
	prog   map[int64]string // live progress line per bot, for phones that reopen a chat

	upMu    sync.Mutex
	uploads map[string]*hubUpload

	syncMu   sync.Mutex
	pendingQ map[int64]struct{} // unanswered question ids last pushed to phones
	roster   string             // live bot id|name|status fingerprint
	rosterOK bool
}

func instanceDisplayName(cfg *Config) string {
	if cfg != nil && strings.TrimSpace(cfg.InstanceName) != "" {
		return strings.TrimSpace(cfg.InstanceName)
	}
	h, _ := os.Hostname()
	if h == "" {
		return "ccc"
	}
	return h
}

func startHubClient(in *instance) *hubClient {
	cfg := in.config()
	if !hubEnabled(cfg) {
		return nil
	}
	id, err := loadOrCreateHubIdentity()
	if err != nil {
		listenLog("hub: identity: %v", err)
		return nil
	}
	hc := &hubClient{
		in:    in,
		id:    id,
		url:   hubURLFromConfig(cfg),
		name:  instanceDisplayName(cfg),
		peers: map[string][32]byte{},
	}
	var devices []HubDevice
	in.db.Find(&devices)
	for _, d := range devices {
		if pk, err := parseHubPublic(d.PubKey); err == nil {
			hc.peers[d.PubKey] = pk
		}
	}
	go hc.loop()
	go hc.fileLoop()
	listenLog("hub: identity %s via %s", id.ID()[:12], hc.url)
	return hc
}

func (h *hubClient) loop() {
	for {
		if err := h.connect(); err != nil {
			listenLog("hub: %v (retry in 5s)", err)
			time.Sleep(5 * time.Second)
			continue
		}
	}
}

func (h *hubClient) connect() error {
	u, err := url.Parse(h.url)
	if err != nil {
		return err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "wss", "ws":
	default:
		return fmt.Errorf("hub_url must be ws(s)://, got %s", u.Scheme)
	}
	u.Path = "/v1/ws"
	dialer := websocket.Dialer{Proxy: http.ProxyFromEnvironment, HandshakeTimeout: 15 * time.Second}
	c, _, err := dialer.Dial(u.String(), nil)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.conn = c
	h.mu.Unlock()
	defer func() {
		c.Close()
		h.mu.Lock()
		if h.conn == c {
			h.conn = nil
		}
		h.mu.Unlock()
	}()
	if err := c.WriteJSON(hubFrame{V: 1, T: "open", Role: "instance", PK: h.id.ID()}); err != nil {
		return err
	}
	h.offerPending()
	c.SetReadLimit(1 << 20)
	armHubConn(c)
	stopPing := make(chan struct{})
	defer close(stopPing)
	go h.pingLoop(stopPing)
	for {
		_ = c.SetReadDeadline(time.Now().Add(hubIdle))
		_, raw, err := c.ReadMessage()
		if err != nil {
			return err
		}
		var f hubFrame
		if err := json.Unmarshal(raw, &f); err != nil {
			continue
		}
		switch f.T {
		case "open":
			h.offerPending()
		case "ping":
			h.send(hubFrame{V: 1, T: "pong"})
		case "pong":
			// keepalive
		case "pair":
			h.handlePair(f)
		case "fwd":
			h.handleFwd(f)
		}
	}
}

func (h *hubClient) pingLoop(stop <-chan struct{}) {
	t := time.NewTicker(hubPingEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			h.send(hubFrame{V: 1, T: "ping"})
		}
	}
}

func (h *hubClient) offerPending() {
	var codes []HubPairCode
	now := time.Now()
	h.in.db.Where("expires_at > ?", now).Find(&codes)
	for _, c := range codes {
		h.send(hubFrame{V: 1, T: "offer", Code: c.Code})
	}
}

func (h *hubClient) send(f hubFrame) {
	h.mu.Lock()
	c := h.conn
	h.mu.Unlock()
	if c == nil {
		return
	}
	h.wmu.Lock()
	defer h.wmu.Unlock()
	_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = c.WriteJSON(f)
}

func (h *hubClient) handlePair(f hubFrame) {
	from, err := parseHubPublic(f.PK)
	if err != nil {
		return
	}
	nonce, err := decodeNonce(f.N)
	if err != nil {
		return
	}
	ct, err := decodeBox(f.B)
	if err != nil {
		return
	}
	plain, err := hubOpen(h.id, from, nonce, ct)
	if err != nil {
		listenLog("hub: pair box rejected")
		return
	}
	var intro hubPairIntro
	if err := json.Unmarshal(plain, &intro); err != nil || intro.PK != f.PK {
		return
	}
	now := time.Now()
	_ = h.in.db.Where("code = ? AND expires_at > ?", strings.ToLower(f.Code), now).Delete(&HubPairCode{})
	name := strings.TrimSpace(intro.Name)
	if name == "" {
		name = "phone"
	}
	var existing HubDevice
	if err := h.in.db.Where("pub_key = ?", intro.PK).First(&existing).Error; err == nil {
		existing.Name = name
		existing.LastSeen = &now
		h.in.db.Save(&existing)
	} else {
		h.in.db.Create(&HubDevice{PubKey: intro.PK, Name: name, PairedAt: now, LastSeen: &now})
	}
	h.mu.Lock()
	h.peers[intro.PK] = from
	h.mu.Unlock()
	hello, _ := json.Marshal(hubHello{Name: h.name, Hostname: h.name, Bots: h.botCount()})
	rpc, _ := json.Marshal(hubRPC{Kind: "pair", OK: true, Body: hello})
	h.sendBox(intro.PK, from, rpc)
	listenLog("hub: paired device %s (%s)", intro.PK[:12], name)
}

func (h *hubClient) botCount() int {
	var n int64
	h.in.db.Model(&Bot{}).Where("archived_at IS NULL").Count(&n)
	return int(n)
}

func (h *hubClient) handleFwd(f hubFrame) {
	from, err := parseHubPublic(f.PK)
	if err != nil {
		return
	}
	h.mu.Lock()
	_, allowed := h.peers[f.PK]
	h.mu.Unlock()
	if !allowed {
		return
	}
	nonce, err := decodeNonce(f.N)
	if err != nil {
		return
	}
	ct, err := decodeBox(f.B)
	if err != nil {
		return
	}
	plain, err := hubOpen(h.id, from, nonce, ct)
	if err != nil {
		return
	}
	var rpc hubRPC
	if err := json.Unmarshal(plain, &rpc); err != nil || rpc.Kind != "req" {
		return
	}
	now := time.Now()
	h.in.db.Model(&HubDevice{}).Where("pub_key = ?", f.PK).Update("last_seen", now)
	res := h.dispatch(rpc)
	raw, _ := json.Marshal(res)
	h.sendBox(f.PK, from, raw)
}

func (h *hubClient) sendBox(toHex string, toPub [32]byte, plain []byte) {
	n, ct, err := hubSeal(h.id, toPub, plain)
	if err != nil {
		return
	}
	h.send(hubFrame{V: 1, T: "fwd", To: toHex, N: encodeNonce(n), B: encodeBox(ct)})
}

func (h *hubClient) pushEvent(kind string, body any) {
	h.rememberProgress(kind, body)
	raw, _ := json.Marshal(body)
	rpc, _ := json.Marshal(hubRPC{Kind: "event", Method: kind, Body: raw})
	h.mu.Lock()
	peers := make(map[string][32]byte, len(h.peers))
	for k, v := range h.peers {
		peers[k] = v
	}
	h.mu.Unlock()
	for id, pk := range peers {
		h.sendBox(id, pk, rpc)
	}
}

func botIDFromBody(body any) int64 {
	m, ok := body.(map[string]any)
	if !ok {
		return 0
	}
	switch v := m["bot_id"].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	default:
		return 0
	}
}

func (h *hubClient) rememberProgress(kind string, body any) {
	id := botIDFromBody(body)
	if id == 0 {
		return
	}
	text := ""
	if m, ok := body.(map[string]any); ok {
		text, _ = m["text"].(string)
	}
	h.progMu.Lock()
	defer h.progMu.Unlock()
	if h.prog == nil {
		h.prog = map[int64]string{}
	}
	if kind == "post" || strings.TrimSpace(text) == "" {
		delete(h.prog, id)
		return
	}
	if kind == "progress" {
		h.prog[id] = text
	}
}

func (h *hubClient) progressOf(botID int64) string {
	h.progMu.Lock()
	defer h.progMu.Unlock()
	return h.prog[botID]
}

func liveProgress(status, stored string) string {
	switch status {
	case turnRunning:
		if strings.TrimSpace(stored) != "" {
			return stored
		}
		return "working"
	case turnQueued:
		return "queued"
	default:
		return ""
	}
}

func (h *hubClient) dispatch(rpc hubRPC) hubRPC {
	out := hubRPC{Kind: "res", ID: rpc.ID, OK: true}
	switch rpc.Method {
	case "hello":
		body, _ := json.Marshal(hubHello{Name: h.name, Hostname: h.name, Bots: h.botCount()})
		out.Body = body
	case "bots":
		h.rpcBots(false, &out)
	case "archived":
		h.rpcBots(true, &out)
	case "history":
		h.rpcHistory(rpc.Params, &out)
	case "send":
		h.rpcSend(rpc.Params, &out)
	case "put_begin":
		h.rpcPutBegin(rpc.Params, &out)
	case "put_chunk":
		h.rpcPutChunk(rpc.Params, &out)
	case "put_commit":
		h.rpcPutCommit(rpc.Params, &out)
	case "get_chunk":
		h.rpcGetChunk(rpc.Params, &out)
	case "archive":
		h.rpcArchive(rpc.Params, &out)
	case "unarchive":
		h.rpcUnarchive(rpc.Params, &out)
	case "rename":
		h.rpcRename(rpc.Params, &out)
	case "questions":
		h.rpcQuestions(&out)
	case "answer":
		h.rpcAnswer(rpc.Params, &out)
	default:
		out.OK, out.Error = false, "unknown method"
	}
	return out
}

func (h *hubClient) rpcBots(archived bool, out *hubRPC) {
	var bots []Bot
	var err error
	if archived {
		bots, err = archivedBots(h.in.db)
	} else {
		bots, err = liveBots(h.in.db)
	}
	if err != nil {
		out.OK, out.Error = false, err.Error()
		return
	}
	pending := pendingQuestionsByBot(h.in.db)
	list := make([]hubBotInfo, 0, len(bots))
	for i := range bots {
		var q *Question
		if found, ok := pending[bots[i].ID]; ok {
			q = &found
		}
		list = append(list, fillBotInfo(h.in.db, &bots[i], h.progressOf(bots[i].ID), q))
	}
	if !archived {
		sort.SliceStable(list, func(i, j int) bool { return list[i].Last > list[j].Last })
	}
	out.Body, _ = json.Marshal(list)
}

func fillBotInfo(db *gorm.DB, b *Bot, progress string, pending *Question) hubBotInfo {
	info := hubBotInfo{
		ID: b.ID, Name: b.Name, Role: b.Role, Status: b.Status, Engine: botEngine(b),
		TopicID: b.TopicID, General: isGeneralBot(b), Archived: b.ArchivedAt != nil,
	}
	var last Turn
	if err := db.Where("bot_id = ?", b.ID).Order("id DESC").First(&last).Error; err == nil {
		info.Last = last.CreatedAt.UTC().Format(time.RFC3339)
		preview := strings.TrimSpace(last.Output)
		if preview == "" {
			preview = strings.TrimSpace(last.Input)
		}
		info.LastText = truncate(preview, 80)
		info.Progress = liveProgress(last.Status, progress)
	}
	if pending != nil && pending.AnsweredAt == nil {
		q := hubQuestionFrom(pending, b.Name)
		info.Question = &q
	}
	return info
}

func hubQuestionFrom(q *Question, botName string) hubQuestionInfo {
	opts := questionOptions(q)
	if opts == nil {
		opts = []string{}
	}
	at := ""
	if !q.CreatedAt.IsZero() {
		at = q.CreatedAt.UTC().Format(time.RFC3339)
	}
	return hubQuestionInfo{
		ID: q.ID, BotID: q.BotID, Bot: botName,
		Question: q.Question, Options: opts, At: at,
	}
}

func pendingQuestionsByBot(db *gorm.DB) map[int64]Question {
	var rows []Question
	db.Where("answered_at IS NULL").Order("id").Find(&rows)
	out := map[int64]Question{}
	for i := range rows {
		if _, ok := out[rows[i].BotID]; !ok {
			out[rows[i].BotID] = rows[i]
		}
	}
	return out
}

func (h *hubClient) rpcHistory(params json.RawMessage, out *hubRPC) {
	var p struct {
		BotID int64 `json:"bot_id"`
		Limit int   `json:"limit"`
	}
	_ = json.Unmarshal(params, &p)
	if p.Limit <= 0 || p.Limit > 100 {
		p.Limit = 40
	}
	var turns []Turn
	h.in.db.Where("bot_id = ?", p.BotID).Order("id DESC").Limit(p.Limit).Find(&turns)
	list := make([]hubTurnInfo, 0, len(turns))
	live := h.progressOf(p.BotID)
	ids := make([]int64, 0, len(turns))
	for i := range turns {
		ids = append(ids, turns[i].ID)
	}
	files := h.filesForTurns(p.BotID, ids)
	for i := len(turns) - 1; i >= 0; i-- {
		t := turns[i]
		list = append(list, hubTurnInfo{
			ID: t.ID, Source: t.Source, Input: t.Input, Output: t.Output,
			Status: t.Status, At: t.CreatedAt.UTC().Format(time.RFC3339),
			Progress: liveProgress(t.Status, live),
			Files:    files[t.ID],
		})
	}
	out.Body, _ = json.Marshal(list)
}

func (h *hubClient) rpcSend(params json.RawMessage, out *hubRPC) {
	var p struct {
		BotID int64  `json:"bot_id"`
		Text  string `json:"text"`
		Image *struct {
			MIME string `json:"mime"`
			Name string `json:"name"`
			Data string `json:"data"`
		} `json:"image"`
		File *struct {
			MIME string `json:"mime"`
			Name string `json:"name"`
			Data string `json:"data"`
		} `json:"file"`
	}
	_ = json.Unmarshal(params, &p)
	p.Text = strings.TrimSpace(p.Text)
	b, err := botByID(h.in.db, p.BotID)
	if err != nil || b.ArchivedAt != nil {
		out.OK, out.Error = false, "unknown bot"
		return
	}
	if p.File != nil && strings.TrimSpace(p.File.Data) != "" {
		data, err := decodeHubBytes(p.File.Data)
		if err != nil || len(data) == 0 {
			out.OK, out.Error = false, "bad file"
			return
		}
		if len(data) > hubFileInlineMax {
			out.OK, out.Error = false, "file too large for inline send"
			return
		}
		name := p.File.Name
		if strings.TrimSpace(name) == "" {
			name = "file"
		}
		if err := h.in.ingestOwnerBytes(b, data, name, p.Text); err != nil {
			out.OK, out.Error = false, err.Error()
			return
		}
		out.Body, _ = json.Marshal(map[string]any{"queued": true, "bot": b.Name, "file": true})
		return
	}
	if p.Image != nil && strings.TrimSpace(p.Image.Data) != "" {
		data, err := decodeHubBytes(p.Image.Data)
		if err != nil || len(data) == 0 {
			out.OK, out.Error = false, "bad image"
			return
		}
		if len(data) > hubImageMaxBytes {
			out.OK, out.Error = false, "image too large"
			return
		}
		ext := imageExt(p.Image.MIME, p.Image.Name)
		if ext == "" {
			out.OK, out.Error = false, "unsupported image type"
			return
		}
		name := p.Image.Name
		if filepath.Ext(name) == "" {
			name = "photo" + ext
		}
		if err := h.in.ingestOwnerBytes(b, data, name, p.Text); err != nil {
			out.OK, out.Error = false, err.Error()
			return
		}
		out.Body, _ = json.Marshal(map[string]any{"queued": true, "bot": b.Name, "image": true})
		return
	}
	if p.Text == "" {
		out.OK, out.Error = false, "empty message"
		return
	}
	// Send to a waiting worker is the answer (the phone targeted that session).
	// Send to General is the DM: free text never answers an ask_owner.
	if !isGeneralBot(b) {
		if q, ok := pendingQuestion(h.in.db, b.ID); ok {
			if err := h.answerPending(q, p.Text); err != nil {
				out.OK, out.Error = false, err.Error()
				return
			}
			out.Body, _ = json.Marshal(map[string]any{"queued": true, "bot": b.Name, "answered": q.ID})
			return
		}
	}
	if _, err := h.in.runner.Enqueue(b.ID, sourceUser, p.Text, 0); err != nil {
		out.OK, out.Error = false, err.Error()
		return
	}
	out.Body, _ = json.Marshal(map[string]any{"queued": true, "bot": b.Name})
}

func (h *hubClient) rpcArchive(params json.RawMessage, out *hubRPC) {
	var p struct {
		BotID int64 `json:"bot_id"`
	}
	_ = json.Unmarshal(params, &p)
	b, err := botByID(h.in.db, p.BotID)
	if err != nil || b.ArchivedAt != nil {
		out.OK, out.Error = false, "unknown bot"
		return
	}
	if isGeneralBot(b) {
		out.OK, out.Error = false, "General cannot be archived"
		return
	}
	if err := archiveBotRow(h.in.db, b.ID); err != nil {
		out.OK, out.Error = false, err.Error()
		return
	}
	h.pushEvent("archive", map[string]any{"bot_id": b.ID, "bot": b.Name, "archived": true})
	out.Body, _ = json.Marshal(map[string]any{"archived": true, "bot": b.Name})
}

func (h *hubClient) rpcUnarchive(params json.RawMessage, out *hubRPC) {
	var p struct {
		BotID int64 `json:"bot_id"`
	}
	_ = json.Unmarshal(params, &p)
	b, err := botByID(h.in.db, p.BotID)
	if err != nil || b.ArchivedAt == nil {
		out.OK, out.Error = false, "unknown bot"
		return
	}
	if err := unarchiveBotRow(h.in.db, b.ID); err != nil {
		out.OK, out.Error = false, err.Error()
		return
	}
	h.pushEvent("session", map[string]any{"action": "unarchive", "bot_id": b.ID, "bot": b.Name})
	out.Body, _ = json.Marshal(map[string]any{"archived": false, "bot": b.Name})
}

func (h *hubClient) rpcRename(params json.RawMessage, out *hubRPC) {
	var p struct {
		BotID int64  `json:"bot_id"`
		Name  string `json:"name"`
	}
	_ = json.Unmarshal(params, &p)
	b, err := botByID(h.in.db, p.BotID)
	if err != nil || b.ArchivedAt != nil {
		out.OK, out.Error = false, "unknown bot"
		return
	}
	if isGeneralBot(b) {
		out.OK, out.Error = false, "General stays General"
		return
	}
	name, err := validateBotName(h.in.db, b.ID, p.Name)
	if err != nil {
		out.OK, out.Error = false, err.Error()
		return
	}
	old := b.Name
	if name != old {
		if err := renameBot(h.in.db, h.in.config(), b, name); err != nil {
			out.OK, out.Error = false, err.Error()
			return
		}
	}
	out.Body, _ = json.Marshal(map[string]any{"name": name, "old": old})
}

func (h *hubClient) rpcQuestions(out *hubRPC) {
	list, err := h.liveQuestions()
	if err != nil {
		out.OK, out.Error = false, err.Error()
		return
	}
	out.Body, _ = json.Marshal(list)
}

func (h *hubClient) liveQuestions() ([]hubQuestionInfo, error) {
	var rows []Question
	if err := h.in.db.Where("answered_at IS NULL").Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}
	list := make([]hubQuestionInfo, 0, len(rows))
	for i := range rows {
		b, err := botByID(h.in.db, rows[i].BotID)
		if err != nil || b.ArchivedAt != nil {
			continue
		}
		list = append(list, hubQuestionFrom(&rows[i], b.Name))
	}
	return list, nil
}

func (h *hubClient) rpcAnswer(params json.RawMessage, out *hubRPC) {
	var p struct {
		QuestionID int64  `json:"question_id"`
		Option     *int   `json:"option"`
		Text       string `json:"text"`
	}
	_ = json.Unmarshal(params, &p)
	var q Question
	if err := h.in.db.First(&q, p.QuestionID).Error; err != nil {
		out.OK, out.Error = false, "unknown question"
		return
	}
	if q.AnsweredAt != nil {
		out.OK, out.Error = false, "already answered"
		return
	}
	b, err := botByID(h.in.db, q.BotID)
	if err != nil || b.ArchivedAt != nil {
		out.OK, out.Error = false, "unknown bot"
		return
	}
	answer := strings.TrimSpace(p.Text)
	if p.Option != nil {
		opts := questionOptions(&q)
		if *p.Option < 0 || *p.Option >= len(opts) {
			out.OK, out.Error = false, "unknown option"
			return
		}
		answer = opts[*p.Option]
	}
	if answer == "" {
		out.OK, out.Error = false, "empty answer"
		return
	}
	if err := h.answerPending(&q, answer); err != nil {
		out.OK, out.Error = false, err.Error()
		return
	}
	out.Body, _ = json.Marshal(map[string]any{"answered": q.ID, "bot": b.Name, "answer": answer})
}

func (h *hubClient) answerPending(q *Question, answer string) error {
	answered, err := applyQuestionAnswer(h.in.db, h.in.runner, q, answer)
	if err != nil {
		return err
	}
	for i := range answered {
		a := &answered[i]
		h.in.tickAskedMessage(a, a.Answer)
		h.noteQuestionGone(a.ID)
		name := ""
		if b, err := botByID(h.in.db, a.BotID); err == nil {
			name = b.Name
		}
		h.pushEvent("answered", map[string]any{"id": a.ID, "bot_id": a.BotID, "bot": name, "answer": a.Answer, "text": a.Answer})
	}
	return nil
}

func (h *hubClient) noteQuestionGone(id int64) {
	h.syncMu.Lock()
	defer h.syncMu.Unlock()
	delete(h.pendingQ, id)
}

func rosterFingerprint(bots []Bot) string {
	parts := make([]string, len(bots))
	for i := range bots {
		parts[i] = fmt.Sprintf("%d|%s|%s", bots[i].ID, bots[i].Name, bots[i].Status)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func (h *hubClient) flushRoster() {
	if h.in == nil || h.in.db == nil {
		return
	}
	bots, err := liveBots(h.in.db)
	if err != nil {
		return
	}
	next := rosterFingerprint(bots)
	h.syncMu.Lock()
	if !h.rosterOK {
		h.roster = next
		h.rosterOK = true
		h.syncMu.Unlock()
		return
	}
	changed := h.roster != next
	h.roster = next
	h.syncMu.Unlock()
	if !changed {
		return
	}
	h.pushEvent("session", map[string]any{"action": "sync", "text": "roster"})
}

func (h *hubClient) flushQuestions() {
	if h.in == nil || h.in.db == nil {
		return
	}
	list, err := h.liveQuestions()
	if err != nil {
		return
	}
	next := map[int64]struct{}{}
	byID := map[int64]hubQuestionInfo{}
	for _, q := range list {
		next[q.ID] = struct{}{}
		byID[q.ID] = q
	}
	type ev struct {
		kind string
		body map[string]any
	}
	var out []ev
	h.syncMu.Lock()
	if h.pendingQ == nil {
		h.pendingQ = next
		h.syncMu.Unlock()
		return
	}
	for id, q := range byID {
		if _, ok := h.pendingQ[id]; ok {
			continue
		}
		out = append(out, ev{"question", map[string]any{
			"id": q.ID, "bot_id": q.BotID, "bot": q.Bot,
			"question": q.Question, "options": q.Options, "at": q.At, "text": q.Question,
		}})
	}
	for id := range h.pendingQ {
		if _, ok := next[id]; ok {
			continue
		}
		out = append(out, ev{"answered", map[string]any{"id": id}})
	}
	h.pendingQ = next
	h.syncMu.Unlock()
	for _, e := range out {
		h.pushEvent(e.kind, e.body)
	}
}

func decodeHubBytes(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil && len(b) > 0 {
		return b, nil
	}
	return base64.StdEncoding.DecodeString(s)
}

func imageExt(mime, name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif":
		return strings.ToLower(filepath.Ext(name))
	}
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ""
	}
}

// HubPairCode is a one-time pairing offer `ccc pair` writes; listen advertises
// it to the hub until it expires or is consumed.
type HubPairCode struct {
	Code      string    `gorm:"primaryKey"`
	ExpiresAt time.Time `gorm:"index"`
}
