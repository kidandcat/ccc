package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 1×1 PNG. Small enough that a hub send of it stays well under the 1 MiB frame cap.
var tinyPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53, 0xde,
	0x00, 0x00, 0x00, 0x0c, 0x49, 0x44, 0x41, 0x54,
	0x08, 0xd7, 0x63, 0xf8, 0xcf, 0xc0, 0x00, 0x00,
	0x00, 0x03, 0x00, 0x01, 0x00, 0x05, 0xfe, 0xd4, 0xef,
	0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44,
	0xae, 0x42, 0x60, 0x82,
}

func testHub(t *testing.T) (*hubClient, *instance, *fakeRunner, *fakeBotAPI) {
	t.Helper()
	in, runner, api := testInstance(t)
	return &hubClient{in: in, name: "mac", peers: map[string][32]byte{}}, in, runner, api
}

func TestHubSendImageLandsInInbox(t *testing.T) {
	h, in, runner, _ := testHub(t)
	b, err := in.createBot("chat", "")
	if err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(map[string]any{
		"bot_id": b.ID,
		"text":   "look at this",
		"image": map[string]any{
			"mime": "image/png",
			"name": "shot.png",
			"data": base64.StdEncoding.EncodeToString(tinyPNG),
		},
	})
	res := h.dispatch(hubRPC{Kind: "req", ID: "1", Method: "send", Params: params})
	if !res.OK {
		t.Fatalf("send: %s", res.Error)
	}
	last, ok := runner.last()
	if !ok {
		t.Fatal("nothing enqueued")
	}
	wantDir := filepath.Join(b.Cwd, "inbox")
	if !strings.Contains(last.Text, wantDir) || !strings.Contains(last.Text, "look at this") {
		t.Errorf("enqueue text %q does not mention the inbox or caption", last.Text)
	}
	got, err := os.ReadFile(filepath.Join(wantDir, "shot.png"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(tinyPNG) {
		t.Error("saved file does not match the bytes the phone sent")
	}
}

func TestHubSendFileLandsInInbox(t *testing.T) {
	h, in, runner, _ := testHub(t)
	b, err := in.createBot("chat", "")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("hello-apk-bytes")
	params, _ := json.Marshal(map[string]any{
		"bot_id": b.ID,
		"text":   "install this",
		"file": map[string]any{
			"mime": "application/vnd.android.package-archive",
			"name": "ccc.apk",
			"data": base64.StdEncoding.EncodeToString(payload),
		},
	})
	res := h.dispatch(hubRPC{Kind: "req", ID: "1", Method: "send", Params: params})
	if !res.OK {
		t.Fatalf("send: %s", res.Error)
	}
	last, ok := runner.last()
	if !ok {
		t.Fatal("nothing enqueued")
	}
	if !strings.Contains(last.Text, "ccc.apk") || !strings.Contains(last.Text, "install this") {
		t.Errorf("enqueue text %q", last.Text)
	}
	got, err := os.ReadFile(filepath.Join(b.Cwd, "inbox", "ccc.apk"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Error("saved file does not match")
	}
	var files []HubFile
	in.db.Find(&files)
	if len(files) != 1 || files[0].Name != "ccc.apk" || files[0].Direction != "in" {
		t.Fatalf("hub file row = %+v", files)
	}
	hist := h.dispatch(hubRPC{Kind: "req", ID: "2", Method: "history", Params: mustJSON(map[string]any{"bot_id": b.ID})})
	var turns []hubTurnInfo
	if err := json.Unmarshal(hist.Body, &turns); err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || len(turns[0].Files) != 1 || turns[0].Files[0].Name != "ccc.apk" {
		t.Fatalf("history files = %+v", turns)
	}
}

func TestHubChunkedUploadAndDownload(t *testing.T) {
	h, in, runner, _ := testHub(t)
	b, _ := in.createBot("chat", "")
	payload := make([]byte, hubChunkBytes+7)
	for i := range payload {
		payload[i] = byte(i)
	}
	begin := h.dispatch(hubRPC{Kind: "req", ID: "1", Method: "put_begin", Params: mustJSON(map[string]any{
		"bot_id": b.ID, "name": "blob.bin", "size": len(payload),
	})})
	if !begin.OK {
		t.Fatalf("begin: %s", begin.Error)
	}
	var started struct {
		UploadID string `json:"upload_id"`
		N        int    `json:"n"`
	}
	json.Unmarshal(begin.Body, &started)
	if started.N != 2 || started.UploadID == "" {
		t.Fatalf("begin body %+v", started)
	}
	for i := 0; i < started.N; i++ {
		start := i * hubChunkBytes
		end := start + hubChunkBytes
		if end > len(payload) {
			end = len(payload)
		}
		res := h.dispatch(hubRPC{Kind: "req", ID: "c", Method: "put_chunk", Params: mustJSON(map[string]any{
			"upload_id": started.UploadID,
			"i":         i,
			"data":      base64.StdEncoding.EncodeToString(payload[start:end]),
		})})
		if !res.OK {
			t.Fatalf("chunk %d: %s", i, res.Error)
		}
	}
	commit := h.dispatch(hubRPC{Kind: "req", ID: "2", Method: "put_commit", Params: mustJSON(map[string]any{
		"upload_id": started.UploadID, "text": "here",
	})})
	if !commit.OK {
		t.Fatalf("commit: %s", commit.Error)
	}
	if _, ok := runner.last(); !ok {
		t.Fatal("commit must enqueue")
	}
	got, err := os.ReadFile(filepath.Join(b.Cwd, "inbox", "blob.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatal("roundtrip mismatch")
	}
	var row HubFile
	if err := in.db.Where("name = ?", "blob.bin").First(&row).Error; err != nil {
		t.Fatal(err)
	}
	var rebuilt []byte
	n := chunkCount(row.Size)
	for i := 0; i < n; i++ {
		res := h.dispatch(hubRPC{Kind: "req", ID: "g", Method: "get_chunk", Params: mustJSON(map[string]any{
			"file_id": row.ID, "i": i,
		})})
		if !res.OK {
			t.Fatalf("get %d: %s", i, res.Error)
		}
		var chunk struct {
			Data string `json:"data"`
		}
		json.Unmarshal(res.Body, &chunk)
		part, err := base64.StdEncoding.DecodeString(chunk.Data)
		if err != nil {
			t.Fatal(err)
		}
		rebuilt = append(rebuilt, part...)
	}
	if string(rebuilt) != string(payload) {
		t.Fatal("download mismatch")
	}
}

func TestRecordOutgoingFileIsOfferedToPhones(t *testing.T) {
	h, in, _, _ := testHub(t)
	b, _ := in.createBot("chat", "")
	path := filepath.Join(t.TempDir(), "out.apk")
	if err := os.WriteFile(path, []byte("apk"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := recordOutgoingFile(in.db, b.ID, 0, path, "out.apk", 3)
	if err != nil {
		t.Fatal(err)
	}
	h.flushOutgoingFiles()
	var fresh HubFile
	in.db.First(&fresh, f.ID)
	if fresh.PushedAt == nil {
		t.Fatal("flush must mark the file pushed")
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func TestHubSendImageTooLarge(t *testing.T) {
	h, in, runner, _ := testHub(t)
	b, _ := in.createBot("chat", "")
	params, _ := json.Marshal(map[string]any{
		"bot_id": b.ID,
		"image": map[string]any{
			"mime": "image/jpeg",
			"name": "big.jpg",
			"data": base64.StdEncoding.EncodeToString(make([]byte, hubImageMaxBytes+1)),
		},
	})
	res := h.dispatch(hubRPC{Kind: "req", ID: "1", Method: "send", Params: params})
	if res.OK || res.Error != "image too large" {
		t.Fatalf("got ok=%v err=%q", res.OK, res.Error)
	}
	if _, ok := runner.last(); ok {
		t.Error("oversize image must not enqueue a turn")
	}
}

func TestHubArchiveHidesFromBotsList(t *testing.T) {
	h, in, _, api := testHub(t)
	live, _ := in.createBot("keep", "")
	gone, _ := in.createBot("gone", "")
	params, _ := json.Marshal(map[string]any{"bot_id": gone.ID})
	before := len(api.since("sendMessage"))
	res := h.dispatch(hubRPC{Kind: "req", ID: "1", Method: "archive", Params: params})
	if !res.OK {
		t.Fatalf("archive: %s", res.Error)
	}
	if len(api.since("closeForumTopic")) != 0 {
		t.Error("backend sessions have no Telegram topic to close")
	}
	if extra := len(api.since("sendMessage")) - before; extra != 0 {
		t.Fatalf("archive must not post a Telegram alert, extra sendMessage=%d", extra)
	}
	listed := h.dispatch(hubRPC{Kind: "req", ID: "2", Method: "bots"})
	var liveList []hubBotInfo
	if err := json.Unmarshal(listed.Body, &liveList); err != nil {
		t.Fatal(err)
	}
	if len(liveList) != 1 || liveList[0].ID != live.ID {
		t.Fatalf("live list = %+v, want only %d", liveList, live.ID)
	}
	archived := h.dispatch(hubRPC{Kind: "req", ID: "3", Method: "archived"})
	var goneList []hubBotInfo
	if err := json.Unmarshal(archived.Body, &goneList); err != nil {
		t.Fatal(err)
	}
	if len(goneList) != 1 || goneList[0].ID != gone.ID || !goneList[0].Archived {
		t.Fatalf("archived list = %+v", goneList)
	}
	res = h.dispatch(hubRPC{Kind: "req", ID: "4", Method: "unarchive", Params: params})
	if !res.OK {
		t.Fatalf("unarchive: %s", res.Error)
	}
	listed = h.dispatch(hubRPC{Kind: "req", ID: "5", Method: "bots"})
	if err := json.Unmarshal(listed.Body, &liveList); err != nil {
		t.Fatal(err)
	}
	if len(liveList) != 2 {
		t.Fatalf("after unarchive live = %+v", liveList)
	}
}

func TestHubHistoryIncludesLiveProgress(t *testing.T) {
	h, in, _, _ := testHub(t)
	b, err := in.createBot("chat", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := in.db.Create(&Turn{BotID: b.ID, Source: sourceUser, Input: "hi", Status: turnRunning}).Error; err != nil {
		t.Fatal(err)
	}
	h.pushEvent("progress", map[string]any{"bot_id": b.ID, "bot": b.Name, "text": "reading main.dart · 8s"})

	params, _ := json.Marshal(map[string]any{"bot_id": b.ID})
	res := h.dispatch(hubRPC{Kind: "req", ID: "1", Method: "history", Params: params})
	if !res.OK {
		t.Fatalf("history: %s", res.Error)
	}
	var turns []hubTurnInfo
	if err := json.Unmarshal(res.Body, &turns); err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].Progress != "reading main.dart · 8s" {
		t.Fatalf("history progress = %+v", turns)
	}

	listed := h.dispatch(hubRPC{Kind: "req", ID: "2", Method: "bots"})
	var bots []hubBotInfo
	if err := json.Unmarshal(listed.Body, &bots); err != nil {
		t.Fatal(err)
	}
	if len(bots) != 1 || bots[0].Progress != "reading main.dart · 8s" {
		t.Fatalf("bots progress = %+v", bots)
	}

	h.pushEvent("post", map[string]any{"bot_id": b.ID, "bot": b.Name, "text": "done"})
	in.db.Model(&Turn{}).Where("bot_id = ?", b.ID).Update("status", turnDone)
	listed = h.dispatch(hubRPC{Kind: "req", ID: "3", Method: "bots"})
	bots = nil
	if err := json.Unmarshal(listed.Body, &bots); err != nil {
		t.Fatal(err)
	}
	if len(bots) != 1 || bots[0].Progress != "" {
		t.Fatalf("progress after post = %+v", bots)
	}
}

func TestHubRename(t *testing.T) {
	h, in, _, api := testHub(t)
	b, _ := in.createBot("old-name", "")
	params, _ := json.Marshal(map[string]any{"bot_id": b.ID, "name": "new-name"})
	res := h.dispatch(hubRPC{Kind: "req", ID: "1", Method: "rename", Params: params})
	if !res.OK {
		t.Fatalf("rename: %s", res.Error)
	}
	got, err := botByID(in.db, b.ID)
	if err != nil || got.Name != "new-name" {
		t.Fatalf("stored name = %q err=%v", got.Name, err)
	}
	if len(api.since("editForumTopic")) != 0 {
		t.Error("backend sessions have no forum topic to rename")
	}
}

func TestHubBotsMarksGeneral(t *testing.T) {
	h, in, _, _ := testHub(t)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	worker, err := in.createBot("deploy-watch", "")
	if err != nil {
		t.Fatal(err)
	}
	listed := h.dispatch(hubRPC{Kind: "req", ID: "1", Method: "bots"})
	if !listed.OK {
		t.Fatalf("bots: %s", listed.Error)
	}
	var bots []hubBotInfo
	if err := json.Unmarshal(listed.Body, &bots); err != nil {
		t.Fatal(err)
	}
	var g, w *hubBotInfo
	for i := range bots {
		if bots[i].ID == chief.ID {
			g = &bots[i]
		}
		if bots[i].ID == worker.ID {
			w = &bots[i]
		}
	}
	if g == nil || !g.General || g.TopicID != 0 {
		t.Fatalf("General = %+v", g)
	}
	if w == nil || w.General || w.TopicID == 0 {
		t.Fatalf("worker = %+v", w)
	}
}

func TestHubRenameAndArchiveRefuseGeneral(t *testing.T) {
	h, in, _, _ := testHub(t)
	chief, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(map[string]any{"bot_id": chief.ID, "name": "Chief"})
	res := h.dispatch(hubRPC{Kind: "req", ID: "1", Method: "rename", Params: params})
	if res.OK || !strings.Contains(res.Error, "General stays General") {
		t.Fatalf("rename General: ok=%v err=%q", res.OK, res.Error)
	}
	got, err := botByID(in.db, chief.ID)
	if err != nil || got.Name != generalBotName {
		t.Fatalf("stored name = %q err=%v", got.Name, err)
	}
	arch, _ := json.Marshal(map[string]any{"bot_id": chief.ID})
	res = h.dispatch(hubRPC{Kind: "req", ID: "2", Method: "archive", Params: arch})
	if res.OK || !strings.Contains(res.Error, "General cannot be archived") {
		t.Fatalf("archive General: ok=%v err=%q", res.OK, res.Error)
	}
	got, err = botByID(in.db, chief.ID)
	if err != nil || got.ArchivedAt != nil {
		t.Fatal("General must stay live")
	}
}

func TestHubQuestionsAndAnswer(t *testing.T) {
	h, in, runner, _ := testHub(t)
	live, err := in.createBot("worker", "")
	if err != nil {
		t.Fatal(err)
	}
	gone, _ := in.createBot("gone", "")
	opts, _ := json.Marshal([]string{"ship", "hold"})
	q := Question{BotID: live.ID, Question: "Deploy?", OptionsJSON: string(opts)}
	if err := in.db.Create(&q).Error; err != nil {
		t.Fatal(err)
	}
	orphaned := Question{BotID: gone.ID, Question: "hidden?"}
	if err := in.db.Create(&orphaned).Error; err != nil {
		t.Fatal(err)
	}
	if err := archiveBotRow(in.db, gone.ID); err != nil {
		t.Fatal(err)
	}

	listed := h.dispatch(hubRPC{Kind: "req", ID: "1", Method: "questions"})
	if !listed.OK {
		t.Fatalf("questions: %s", listed.Error)
	}
	var qs []hubQuestionInfo
	if err := json.Unmarshal(listed.Body, &qs); err != nil {
		t.Fatal(err)
	}
	if len(qs) != 1 || qs[0].ID != q.ID || qs[0].Bot != "worker" || len(qs[0].Options) != 2 {
		t.Fatalf("questions = %+v", qs)
	}

	botsRes := h.dispatch(hubRPC{Kind: "req", ID: "2", Method: "bots"})
	var bots []hubBotInfo
	if err := json.Unmarshal(botsRes.Body, &bots); err != nil {
		t.Fatal(err)
	}
	var worker hubBotInfo
	for _, b := range bots {
		if b.ID == live.ID {
			worker = b
		}
	}
	if worker.Question == nil || worker.Question.Question != "Deploy?" {
		t.Fatalf("bots question = %+v", worker)
	}

	opt := 0
	params, _ := json.Marshal(map[string]any{"question_id": q.ID, "option": opt})
	ans := h.dispatch(hubRPC{Kind: "req", ID: "3", Method: "answer", Params: params})
	if !ans.OK {
		t.Fatalf("answer: %s", ans.Error)
	}
	last, ok := runner.last()
	if !ok || !strings.Contains(last.Text, "ship") {
		t.Fatalf("enqueued %v ok=%v", last, ok)
	}
	listed = h.dispatch(hubRPC{Kind: "req", ID: "4", Method: "questions"})
	qs = nil
	if err := json.Unmarshal(listed.Body, &qs); err != nil {
		t.Fatal(err)
	}
	if len(qs) != 0 {
		t.Fatalf("answered question still listed: %+v", qs)
	}
	again := h.dispatch(hubRPC{Kind: "req", ID: "5", Method: "answer", Params: params})
	if again.OK {
		t.Fatal("second answer must fail")
	}
}

func TestHubSendAnswersPendingQuestion(t *testing.T) {
	h, in, runner, _ := testHub(t)
	b, _ := in.createBot("worker", "")
	q := Question{BotID: b.ID, Question: "Which branch?"}
	if err := in.db.Create(&q).Error; err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(map[string]any{"bot_id": b.ID, "text": "main"})
	res := h.dispatch(hubRPC{Kind: "req", ID: "1", Method: "send", Params: params})
	if !res.OK {
		t.Fatalf("send: %s", res.Error)
	}
	last, ok := runner.last()
	if !ok || !strings.Contains(last.Text, "main") || !strings.Contains(last.Text, "Which branch?") {
		t.Fatalf("send to a waiting worker must answer, got %+v", last)
	}
	var stored Question
	if err := in.db.First(&stored, q.ID).Error; err != nil || stored.AnsweredAt == nil {
		t.Fatalf("question not answered: %+v err=%v", stored, err)
	}
}

func TestHubSendToGeneralDoesNotAnswer(t *testing.T) {
	h, in, runner, _ := testHub(t)
	g, err := in.ensureGeneralBot()
	if err != nil {
		t.Fatal(err)
	}
	q := Question{BotID: g.ID, Question: "Which branch?"}
	if err := in.db.Create(&q).Error; err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(map[string]any{"bot_id": g.ID, "text": "do the other thing"})
	res := h.dispatch(hubRPC{Kind: "req", ID: "1", Method: "send", Params: params})
	if !res.OK {
		t.Fatalf("send: %s", res.Error)
	}
	var stored Question
	if err := in.db.First(&stored, q.ID).Error; err != nil || stored.AnsweredAt != nil {
		t.Fatalf("send to General must not answer: %+v err=%v", stored, err)
	}
	last, ok := runner.last()
	if !ok || last.BotID != g.ID || last.Text != "do the other thing" {
		t.Fatalf("send to General must enqueue the text, got %+v ok=%v", last, ok)
	}
}

func TestHubAnswerFansOutToSimilar(t *testing.T) {
	h, in, runner, _ := testHub(t)
	a, err := in.createBot("one", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := in.createBot("two", "")
	if err != nil {
		t.Fatal(err)
	}
	opts, err := json.Marshal([]string{"ship", "hold"})
	if err != nil {
		t.Fatal(err)
	}
	q1 := Question{BotID: a.ID, Question: "Deploy?", OptionsJSON: string(opts)}
	q2 := Question{BotID: b.ID, Question: "deploy?", OptionsJSON: string(opts)}
	if err := in.db.Create(&q1).Error; err != nil {
		t.Fatal(err)
	}
	if err := in.db.Create(&q2).Error; err != nil {
		t.Fatal(err)
	}
	opt := 0
	params, _ := json.Marshal(map[string]any{"question_id": q1.ID, "option": opt})
	ans := h.dispatch(hubRPC{Kind: "req", ID: "3", Method: "answer", Params: params})
	if !ans.OK {
		t.Fatalf("answer: %s", ans.Error)
	}
	var first, second Question
	if err := in.db.First(&first, q1.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := in.db.First(&second, q2.ID).Error; err != nil {
		t.Fatal(err)
	}
	if first.AnsweredAt == nil || second.AnsweredAt == nil {
		t.Fatalf("similar questions not both answered: %+v %+v", first, second)
	}
	if first.Answer != "ship" || second.Answer != "ship" {
		t.Fatalf("answers = %q %q", first.Answer, second.Answer)
	}
	runner.mu.Lock()
	n := len(runner.enqueued)
	runner.mu.Unlock()
	if n != 2 {
		t.Errorf("want 2 answer turns, got %d", n)
	}
}

func TestHubFlushQuestionsAndRoster(t *testing.T) {
	h, in, _, _ := testHub(t)
	b, _ := in.createBot("worker", "")
	h.flushRoster()
	h.flushQuestions()
	q := Question{BotID: b.ID, Question: "Go?"}
	if err := in.db.Create(&q).Error; err != nil {
		t.Fatal(err)
	}
	h.flushQuestions()
	h.syncMu.Lock()
	_, seen := h.pendingQ[q.ID]
	h.syncMu.Unlock()
	if !seen {
		t.Fatal("new question must enter the snapshot")
	}
	if err := archiveBotRow(in.db, b.ID); err != nil {
		t.Fatal(err)
	}
	h.flushRoster()
	h.flushQuestions()
	h.syncMu.Lock()
	_, still := h.pendingQ[q.ID]
	fp := h.roster
	h.syncMu.Unlock()
	if still {
		t.Fatal("archived session questions must leave the snapshot")
	}
	if strings.Contains(fp, fmt.Sprintf("%d|", b.ID)) {
		t.Fatalf("archived bot still in roster fingerprint %q", fp)
	}
}
