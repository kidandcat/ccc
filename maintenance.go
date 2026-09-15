package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
)

// maintenance.go is ccc's growth control (DESIGN §7): one job, run daily at a
// quiet hour by the scheduler and on demand by `ccc maintain`, that keeps the
// database from growing without bound.
//
// Three things happen, in this order:
//
//  1. Turn retention — old turns are deleted, and the bodies of turns nobody is
//     going to re-read are replaced by a short summary. Usage and status
//     columns survive, because /usage reads them long after the text is gone.
//  2. Memory compaction — a scope whose memories have grown past a threshold is
//     rewritten by ONE cheap model turn that merges duplicates and drops what is
//     obsolete. The originals are archived, so the owner can undo it.
//  3. Cleanup — delivered inbox rows, answered questions, the memories of bots
//     that are long gone, and archives past their retention.
//
// Everything is threshold-driven rather than calendar-driven: memory is
// compacted when it is big, not when a month has passed.

const (
	// turnRetentionDays and turnRetentionPerBot are the two halves of "keep 30
	// days OR the last 200 per bot, whichever keeps more": a turn is deleted
	// only when it fails BOTH tests.
	turnRetentionDays   = 30
	turnRetentionPerBot = 200
	// turnBodyAgeDays is when a turn's input/output stop being worth their
	// bytes; turnBodyLimit is what is left of them.
	turnBodyAgeDays = 7
	turnBodyLimit   = 500
	// turnBodyMarker ends a trimmed body. It is also how a second maintenance
	// run recognises a body it already trimmed and leaves it alone.
	turnBodyMarker = "… [trimmed by maintenance]"

	// memCompactMaxCount and memCompactMaxBytes are the per-scope thresholds
	// above which a compaction turn is worth its tokens.
	memCompactMaxCount = 120
	memCompactMaxBytes = 48 * 1024
	// memCompactMinKeep aborts a compaction that dropped more than 60% of the
	// entries: that is a model that summarised instead of consolidating, and
	// applying it would lose facts.
	memCompactMinKeep = 0.4

	// Cleanup retentions.
	inboxRetentionDays         = 30
	questionRetentionDays      = 30
	archivedBotMemoryDays      = 30
	memoryArchiveRetentionDay  = 90
	backgroundJobRetentionDays = 30
)

// ---------------------------------------------------------------------------
// Report
// ---------------------------------------------------------------------------

// compactionResult is one compacted scope.
type compactionResult struct {
	Label        string
	Before       int
	After        int
	CompactionID int64
}

// maintenanceReport is what one run did, for the CLI and the owner's topic.
type maintenanceReport struct {
	TurnsDeleted       int64
	TurnsTrimmed       int64
	InboxDeleted       int64
	QuestionsDeleted   int64
	BotMemoriesDeleted int64
	ArchivesDeleted    int64
	JobsDeleted        int64
	Compactions        []compactionResult
	// Problems are the scopes whose compaction was abandoned, with the reason.
	Problems []string
}

func (r maintenanceReport) String() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "turns: %d deleted, %d trimmed\n", r.TurnsDeleted, r.TurnsTrimmed)
	fmt.Fprintf(&sb, "cleanup: %d inbox, %d questions, %d bot memories, %d archived memories, %d background jobs\n",
		r.InboxDeleted, r.QuestionsDeleted, r.BotMemoriesDeleted, r.ArchivesDeleted, r.JobsDeleted)
	if len(r.Compactions) == 0 {
		sb.WriteString("memories: nothing over the compaction threshold\n")
	}
	for _, c := range r.Compactions {
		fmt.Fprintf(&sb, "memories: %s %d → %d (compaction %d)\n", c.Label, c.Before, c.After, c.CompactionID)
	}
	for _, p := range r.Problems {
		fmt.Fprintf(&sb, "problem: %s\n", p)
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// The job
// ---------------------------------------------------------------------------

// plainTurner runs one model call outside any bot: no session of a bot's, no
// MCP tools, plain text in and out. The Runner implements it; the tests inject
// a fake so maintenance can be exercised without spending tokens.
type plainTurner interface {
	PlainTurn(model, prompt string) (string, error)
}

// maintenanceDeps is what runMaintenance needs from the outside world: a way to
// run a model turn, and a way to reach the owner. Both may be nil — without a
// turner, compaction is skipped and reported; without a notifier, the report is
// only the return value.
type maintenanceDeps struct {
	Turner plainTurner
	Notify func(text string)
}

// runMaintenance does one full pass. It never returns an error: a step that
// fails is reported and the rest still runs, because a job that gives up on the
// first problem is a job that stops keeping the database small.
func runMaintenance(db *gorm.DB, cfg *Config, deps maintenanceDeps, now time.Time) maintenanceReport {
	var rep maintenanceReport
	rep.TurnsDeleted, rep.TurnsTrimmed = applyTurnRetention(db, now, &rep)
	compactMemories(db, cfg, deps, now, &rep)
	runCleanup(db, now, &rep)
	return rep
}

// ---------------------------------------------------------------------------
// 1. Turn retention
// ---------------------------------------------------------------------------

// applyTurnRetention deletes turns that are both older than the retention
// window and outside their bot's most recent turnRetentionPerBot, then trims
// the bodies of the turns that stayed but are older than turnBodyAgeDays.
func applyTurnRetention(db *gorm.DB, now time.Time, rep *maintenanceReport) (deleted, trimmed int64) {
	cutoff := now.AddDate(0, 0, -turnRetentionDays)

	var botIDs []int64
	if err := db.Model(&Turn{}).Distinct().Pluck("bot_id", &botIDs).Error; err != nil {
		rep.Problems = append(rep.Problems, "turn retention: "+err.Error())
		return 0, 0
	}
	for _, botID := range botIDs {
		// The id of the newest turn that is NOT one of the last N for this bot.
		// Everything at or below it is a candidate; anything above is kept by
		// the count half of the rule no matter how old it is.
		var floor []int64
		if err := db.Model(&Turn{}).Where("bot_id = ?", botID).
			Order("id DESC").Offset(turnRetentionPerBot).Limit(1).Pluck("id", &floor).Error; err != nil {
			rep.Problems = append(rep.Problems, fmt.Sprintf("turn retention for bot %d: %v", botID, err))
			continue
		}
		if len(floor) == 0 {
			continue // fewer than N turns: the count half keeps all of them
		}
		res := db.Where("bot_id = ? AND id <= ? AND created_at < ?", botID, floor[0], cutoff).Delete(&Turn{})
		if res.Error != nil {
			rep.Problems = append(rep.Problems, fmt.Sprintf("turn retention for bot %d: %v", botID, res.Error))
			continue
		}
		deleted += res.RowsAffected
	}
	return deleted, trimTurnBodies(db, now, rep)
}

// trimTurnBodies replaces the input/output of turns older than turnBodyAgeDays
// with a truncated copy. No model is involved: a plain cut with a marker is
// enough to keep a turn readable as history, and a summarisation pass over
// thousands of rows would cost more than the bytes are worth.
func trimTurnBodies(db *gorm.DB, now time.Time, rep *maintenanceReport) int64 {
	cutoff := now.AddDate(0, 0, -turnBodyAgeDays)
	const page = 500
	var trimmed int64
	// The cursor is what makes this terminate: an already-trimmed body is still
	// longer than the limit, so it keeps matching the query, and paging by
	// offset-less "give me the next 500 matches" would hand back the same rows
	// for ever. Walking ids forward visits each row exactly once.
	var after int64
	for {
		var rows []Turn
		err := db.Select("id, input, output").
			Where("id > ? AND created_at < ? AND (length(input) > ? OR length(output) > ?)",
				after, cutoff, turnBodyLimit, turnBodyLimit).
			Order("id").Limit(page).Find(&rows).Error
		if err != nil {
			rep.Problems = append(rep.Problems, "turn trimming: "+err.Error())
			return trimmed
		}
		for _, t := range rows {
			after = t.ID
			in, inCut := trimBody(t.Input)
			out, outCut := trimBody(t.Output)
			if !inCut && !outCut {
				continue
			}
			if err := db.Model(&Turn{}).Where("id = ?", t.ID).
				Updates(map[string]any{"input": in, "output": out}).Error; err != nil {
				rep.Problems = append(rep.Problems, fmt.Sprintf("turn %d trimming: %v", t.ID, err))
				continue
			}
			trimmed++
		}
		if len(rows) < page {
			return trimmed
		}
	}
}

// trimBody cuts one body down to turnBodyLimit, and reports whether it did.
// An already-trimmed body is left exactly as it is, so repeated runs do not
// eat into the summary they produced last time.
func trimBody(s string) (string, bool) {
	if len(s) <= turnBodyLimit || strings.HasSuffix(s, turnBodyMarker) {
		return s, false
	}
	return s[:turnBodyLimit] + turnBodyMarker, true
}

// ---------------------------------------------------------------------------
// 2. Memory compaction
// ---------------------------------------------------------------------------

// memoryScopeStat is one (scope, scope_key) group and how big it has grown.
type memoryScopeStat struct {
	Scope    string
	ScopeKey string
	Count    int
	Bytes    int
}

// overThreshold is what makes a compaction worth a model call.
func (m memoryScopeStat) overThreshold() bool {
	return m.Count > memCompactMaxCount || m.Bytes > memCompactMaxBytes
}

// memoryScopeStats groups every memory by scope, with the byte size of the text
// it holds. This is what both compaction and /memory stats read.
func memoryScopeStats(db *gorm.DB) ([]memoryScopeStat, error) {
	var out []memoryScopeStat
	err := db.Model(&Memory{}).
		Select("scope, scope_key, count(*) as count, sum(length(key) + length(text)) as bytes").
		Group("scope, scope_key").Order("scope, scope_key").Scan(&out).Error
	return out, err
}

// scopeLabel names a scope the way the owner sees it in Telegram.
func scopeLabel(db *gorm.DB, scope, scopeKey string) string {
	switch scope {
	case scopeUser:
		return "user memories"
	case scopeProject:
		return "project memories for " + filepath.Base(scopeKey)
	case scopeBot:
		if b, err := botByID(db, parseBotScopeKey(scopeKey)); err == nil {
			return b.Name + "'s memories"
		}
		return "memories of a deleted bot"
	}
	return scope + " memories"
}

func parseBotScopeKey(scopeKey string) int64 {
	var id int64
	fmt.Sscanf(scopeKey, "%d", &id) // safe-ignore: an unparseable key yields 0, which matches no bot
	return id
}

// compactMemories compacts every scope that is over the threshold.
func compactMemories(db *gorm.DB, cfg *Config, deps maintenanceDeps, now time.Time, rep *maintenanceReport) {
	stats, err := memoryScopeStats(db)
	if err != nil {
		rep.Problems = append(rep.Problems, "memory stats: "+err.Error())
		return
	}
	for _, st := range stats {
		if !st.overThreshold() {
			continue
		}
		label := scopeLabel(db, st.Scope, st.ScopeKey)
		if deps.Turner == nil {
			rep.Problems = append(rep.Problems, label+": over the compaction threshold but no model is available")
			continue
		}
		res, err := compactScope(db, cfg, deps.Turner, st, label, now)
		if err != nil {
			problem := fmt.Sprintf("%s: compaction abandoned (%v)", label, err)
			rep.Problems = append(rep.Problems, problem)
			// Abandoning is not silent: the owner has to know the memories are
			// still growing, and why nothing was applied.
			notifyMaintenance(deps, "🧹 "+problem)
			continue
		}
		rep.Compactions = append(rep.Compactions, *res)
		notifyMaintenance(deps, fmt.Sprintf("🧹 Compacted %s: %d → %d (/memory restore %d to undo)",
			res.Label, res.Before, res.After, res.CompactionID))
	}
}

func notifyMaintenance(deps maintenanceDeps, text string) {
	if deps.Notify != nil {
		deps.Notify(text)
	}
}

// compactScope runs the compaction turn for one scope and applies the result.
// Nothing is written unless the output parses and keeps enough of the entries.
func compactScope(db *gorm.DB, cfg *Config, turner plainTurner, st memoryScopeStat, label string, now time.Time) (*compactionResult, error) {
	var mems []Memory
	if err := db.Where("scope = ? AND scope_key = ?", st.Scope, st.ScopeKey).
		Order("updated_at, id").Find(&mems).Error; err != nil {
		return nil, err
	}
	if len(mems) == 0 {
		return nil, errors.New("the scope is empty")
	}

	out, err := compactionTurn(turner, db, cfg, renderCompactionPrompt(label, mems))
	if err != nil {
		return nil, err
	}
	next, err := parseMemoryList(out)
	if err != nil {
		return nil, err
	}
	if float64(len(next)) < float64(len(mems))*memCompactMinKeep {
		return nil, fmt.Errorf("the result kept only %d of %d entries, which is more than the %d%% the rule allows to disappear",
			len(next), len(mems), int((1-memCompactMinKeep)*100))
	}

	id, err := applyCompaction(db, st.Scope, st.ScopeKey, mems, next, now)
	if err != nil {
		return nil, err
	}
	return &compactionResult{Label: label, Before: len(mems), After: len(next), CompactionID: id}, nil
}

// compactionTurn runs the model call, falling back to the instance model when
// the configured compaction model is not one this Claude Code knows. The alias
// "haiku" is accepted by 2.1.270 (verified), but the catalog moves and a
// rejected alias must not stop maintenance.
func compactionTurn(turner plainTurner, db *gorm.DB, cfg *Config, prompt string) (string, error) {
	model := compactionModel(cfg)
	out, err := turner.PlainTurn(model, prompt)
	if err == nil {
		return out, nil
	}
	fallback := instanceModel(cfg)
	if !isUnknownModelError(err.Error()) || fallback == model {
		return "", err
	}
	hookLog("compaction model %q was rejected (%v); falling back to %q", model, err, fallback)
	return turner.PlainTurn(fallback, prompt)
}

// isUnknownModelError recognises Claude Code refusing a model name. Verified
// against 2.1.270, which answers an unknown --model with
// "[claude-code:unrecognized_model]" and "isn't described by this version's
// model catalog".
func isUnknownModelError(text string) bool {
	l := strings.ToLower(text)
	return strings.Contains(l, "unrecognized_model") ||
		strings.Contains(l, "model catalog") ||
		strings.Contains(l, "issue with the selected model")
}

// renderCompactionPrompt is the whole input of a compaction turn: the scope's
// memories as `key: text` lines, and what to do with them. It asks for the same
// format back so the result can be parsed strictly.
func renderCompactionPrompt(label string, mems []Memory) string {
	var sb strings.Builder
	sb.WriteString("You are consolidating a list of remembered facts (" + label + ").\n\n")
	sb.WriteString("Rules:\n")
	sb.WriteString("- Merge entries that say the same thing into one.\n")
	sb.WriteString("- Drop facts that are obsolete or contradicted by a later entry; the entries are listed oldest first, so the newest wins.\n")
	sb.WriteString("- Keep every distinct durable fact. This is a consolidation, not a summary: do not drop information because it seems unimportant.\n")
	sb.WriteString("- Keep each entry's key when you keep its fact; invent a short key only for an entry you merged.\n\n")
	sb.WriteString("Output format: one entry per line, exactly `key: text`.\n")
	sb.WriteString("Output NOTHING else: no preamble, no closing remark, no numbering, no bullets, no code fences.\n\n")
	sb.WriteString("Entries (oldest first):\n")
	for _, m := range mems {
		fmt.Fprintf(&sb, "%s: %s\n", m.Key, collapseWhitespace(m.Text))
	}
	return sb.String()
}

// parsedMemory is one line of a compaction result.
type parsedMemory struct {
	Key  string
	Text string
}

// maxMemoryKeyLen is how long a key may be before the line is treated as prose
// that happens to contain a colon rather than as an entry.
const maxMemoryKeyLen = 80

// parseMemoryList reads a compaction result strictly: every non-blank line must
// be `key: text`. Anything else — a preamble, a bullet list, a code fence —
// fails the whole parse, because guessing which lines were meant to be memories
// is how facts get lost.
func parseMemoryList(out string) ([]parsedMemory, error) {
	var parsed []parsedMemory
	seen := map[string]int{}
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if isMarkdownLine(line) {
			return nil, fmt.Errorf("the model wrote markdown instead of a bare `key: text` line: %q", truncate(line, 80))
		}
		idx := strings.Index(line, ":")
		if idx <= 0 {
			return nil, fmt.Errorf("the model wrote a line that is not `key: text`: %q", truncate(line, 80))
		}
		key := strings.TrimSpace(line[:idx])
		text := strings.TrimSpace(line[idx+1:])
		if key == "" || text == "" || len(key) > maxMemoryKeyLen || strings.ContainsAny(key, "`*#|") {
			return nil, fmt.Errorf("the model wrote a line that is not `key: text`: %q", truncate(line, 80))
		}
		if at, ok := seen[key]; ok {
			parsed[at].Text = text // a repeated key is one entry, last one wins
			continue
		}
		seen[key] = len(parsed)
		parsed = append(parsed, parsedMemory{Key: key, Text: text})
	}
	if len(parsed) == 0 {
		return nil, errors.New("the model returned no entries")
	}
	return parsed, nil
}

// isMarkdownLine spots the shapes a chatty model reaches for — a bullet, a
// numbered item, a heading, a fence, a quote — which would otherwise parse as
// an entry whose key is "- deploy".
func isMarkdownLine(line string) bool {
	switch line[0] {
	case '-', '*', '+', '>', '#', '`', '|':
		return true
	}
	if i := strings.Index(line, ". "); i > 0 && i <= 3 {
		if _, err := strconv.Atoi(line[:i]); err == nil {
			return true
		}
	}
	return false
}

// applyCompaction swaps a scope's memories for the compacted list in one
// transaction: archive the old rows under a fresh compaction id, delete them,
// insert the new ones. Either the whole scope moves or nothing does.
func applyCompaction(db *gorm.DB, scope, scopeKey string, old []Memory, next []parsedMemory, now time.Time) (int64, error) {
	var compactionID int64
	err := db.Transaction(func(tx *gorm.DB) error {
		var maxID []int64
		if err := tx.Model(&MemoryArchive{}).Order("compaction_id DESC").Limit(1).
			Pluck("compaction_id", &maxID).Error; err != nil {
			return err
		}
		compactionID = 1
		if len(maxID) > 0 {
			compactionID = maxID[0] + 1
		}
		for _, m := range old {
			row := MemoryArchive{
				CompactionID: compactionID, ArchivedAt: now,
				Scope: m.Scope, ScopeKey: m.ScopeKey, Key: m.Key, Text: m.Text,
				CreatedByBotID: m.CreatedByBotID, MemoryCreatedAt: m.CreatedAt, MemoryUpdatedAt: m.UpdatedAt,
			}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
		}
		if err := tx.Where("scope = ? AND scope_key = ?", scope, scopeKey).Delete(&Memory{}).Error; err != nil {
			return err
		}
		for _, p := range next {
			row := Memory{Scope: scope, ScopeKey: scopeKey, Key: p.Key, Text: p.Text, CreatedAt: now, UpdatedAt: now}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return compactionID, nil
}

// restoreCompaction undoes one compaction: the archived rows go back and the
// compacted ones they replaced are dropped. The archive rows are consumed, so
// a compaction id can be restored once — after that the memories ARE the
// originals again.
func restoreCompaction(db *gorm.DB, compactionID int64) (label string, restored int, err error) {
	var rows []MemoryArchive
	if err := db.Where("compaction_id = ?", compactionID).Order("id").Find(&rows).Error; err != nil {
		return "", 0, err
	}
	if len(rows) == 0 {
		return "", 0, fmt.Errorf("there is no compaction %d to restore", compactionID)
	}
	label = scopeLabel(db, rows[0].Scope, rows[0].ScopeKey)
	err = db.Transaction(func(tx *gorm.DB) error {
		scopes := map[[2]string]bool{}
		for _, r := range rows {
			scopes[[2]string{r.Scope, r.ScopeKey}] = true
		}
		for s := range scopes {
			if err := tx.Where("scope = ? AND scope_key = ?", s[0], s[1]).Delete(&Memory{}).Error; err != nil {
				return err
			}
		}
		for _, r := range rows {
			m := Memory{
				Scope: r.Scope, ScopeKey: r.ScopeKey, Key: r.Key, Text: r.Text,
				CreatedByBotID: r.CreatedByBotID, CreatedAt: r.MemoryCreatedAt, UpdatedAt: r.MemoryUpdatedAt,
			}
			if err := tx.Create(&m).Error; err != nil {
				return err
			}
		}
		return tx.Where("compaction_id = ?", compactionID).Delete(&MemoryArchive{}).Error
	})
	if err != nil {
		return "", 0, err
	}
	return label, len(rows), nil
}

// ---------------------------------------------------------------------------
// 3. Cleanup
// ---------------------------------------------------------------------------

func runCleanup(db *gorm.DB, now time.Time, rep *maintenanceReport) {
	del := func(what string, res *gorm.DB) int64 {
		if res.Error != nil {
			rep.Problems = append(rep.Problems, what+": "+res.Error.Error())
			return 0
		}
		return res.RowsAffected
	}

	rep.InboxDeleted = del("inbox cleanup", db.Where("delivered_at IS NOT NULL AND delivered_at < ?",
		now.AddDate(0, 0, -inboxRetentionDays)).Delete(&InboxMessage{}))
	rep.QuestionsDeleted = del("question cleanup", db.Where("answered_at IS NOT NULL AND answered_at < ?",
		now.AddDate(0, 0, -questionRetentionDays)).Delete(&Question{}))

	// A bot's own notes outlive the bot on purpose (archiveBotRow keeps them),
	// but not forever: once the bot has been archived for a month, nothing can
	// read them any more, because bot-scope memories are visible to that bot
	// alone.
	var goneBots []int64
	if err := db.Model(&Bot{}).Where("archived_at IS NOT NULL AND archived_at < ?",
		now.AddDate(0, 0, -archivedBotMemoryDays)).Pluck("id", &goneBots).Error; err != nil {
		rep.Problems = append(rep.Problems, "archived-bot memory cleanup: "+err.Error())
	} else if len(goneBots) > 0 {
		keys := make([]string, 0, len(goneBots))
		for _, id := range goneBots {
			keys = append(keys, fmt.Sprint(id))
		}
		rep.BotMemoriesDeleted = del("archived-bot memory cleanup",
			db.Where("scope = ? AND scope_key IN ?", scopeBot, keys).Delete(&Memory{}))
	}

	rep.ArchivesDeleted = del("memory archive cleanup", db.Where("archived_at < ?",
		now.AddDate(0, 0, -memoryArchiveRetentionDay)).Delete(&MemoryArchive{}))
	rep.JobsDeleted = del("background job cleanup", db.Where("status IN ? AND ended_at IS NOT NULL AND ended_at < ?",
		[]string{jobDone, jobFailed}, now.AddDate(0, 0, -backgroundJobRetentionDays)).Delete(&BackgroundJob{}))
}

// ---------------------------------------------------------------------------
// Scheduling and the CLI
// ---------------------------------------------------------------------------

// maintenanceDue reports whether the daily job should run now: the configured
// hour has passed today and today's run has not happened yet. A day ccc was
// switched off is caught up on at the next start, which is why the marker is a
// date and not a timestamp.
func maintenanceDue(db *gorm.DB, cfg *Config, now time.Time) bool {
	if now.Hour() < maintenanceHour(cfg) {
		return false
	}
	return getSetting(db, settingLastMaintenance, "") != now.Format("2006-01-02")
}

func markMaintenanceRun(db *gorm.DB, now time.Time) {
	if err := setSetting(db, settingLastMaintenance, now.Format("2006-01-02")); err != nil {
		hookLog("record maintenance run: %v", err)
	}
}

// runMaintainCommand is `ccc maintain`: one maintenance pass, printed. It runs
// against the same database the listener uses, and the compaction turn goes
// through the same runner machinery, so what it does is exactly what the
// nightly job does.
func runMaintainCommand() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	db, err := openStore(dbPath(cfg))
	if err != nil {
		return err
	}
	defer closeStore(db)

	runner := newRunner(db, cfg, nil)
	defer runner.Close()
	rep := runMaintenance(db, cfg, maintenanceDeps{
		Turner: runner,
		Notify: func(text string) { fmt.Println(text) },
	}, time.Now())
	markMaintenanceRun(db, time.Now())
	fmt.Print(rep.String())
	return nil
}

// ---------------------------------------------------------------------------
// /memory stats
// ---------------------------------------------------------------------------

// memoryStatLine is one row of `/memory stats`.
type memoryStatLine struct {
	Label     string
	Count     int
	Bytes     int
	Over      bool
	LastAt    *time.Time
	LastID    int64
	LastCount int
}

// memoryStatsFor gathers the numbers behind `/memory stats`: how much each
// scope holds, whether it is over the compaction threshold, and when it was
// last compacted.
func memoryStatsFor(db *gorm.DB) ([]memoryStatLine, error) {
	stats, err := memoryScopeStats(db)
	if err != nil {
		return nil, err
	}
	out := make([]memoryStatLine, 0, len(stats))
	for _, st := range stats {
		line := memoryStatLine{
			Label: scopeLabel(db, st.Scope, st.ScopeKey),
			Count: st.Count, Bytes: st.Bytes, Over: st.overThreshold(),
		}
		var last []MemoryArchive
		if err := db.Where("scope = ? AND scope_key = ?", st.Scope, st.ScopeKey).
			Order("compaction_id DESC").Limit(1).Find(&last).Error; err == nil && len(last) > 0 {
			at := last[0].ArchivedAt
			line.LastAt = &at
			line.LastID = last[0].CompactionID
			var n int64
			db.Model(&MemoryArchive{}).Where("compaction_id = ?", last[0].CompactionID).Count(&n)
			line.LastCount = int(n)
		}
		out = append(out, line)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out, nil
}

// renderMemoryStats is the /memory stats card.
func renderMemoryStats(db *gorm.DB) string {
	lines, err := memoryStatsFor(db)
	if err != nil {
		return "Could not read the memory stats: " + htmlEscape(err.Error())
	}
	if len(lines) == 0 {
		return "No memories yet."
	}
	var sb strings.Builder
	sb.WriteString("🧠 <b>Memory</b>\n")
	for _, l := range lines {
		fmt.Fprintf(&sb, "• <b>%s</b>: %d entries, %s", htmlEscape(l.Label), l.Count, humanBytes(l.Bytes))
		if l.Over {
			sb.WriteString(" ⚠️ over the compaction threshold")
		}
		sb.WriteString("\n")
		if l.LastAt != nil {
			fmt.Fprintf(&sb, "  last compaction #%d on %s (%d entries archived)\n",
				l.LastID, l.LastAt.Format("2006-01-02"), l.LastCount)
		}
	}
	fmt.Fprintf(&sb, "\nCompaction runs when a scope passes %d entries or %s.",
		memCompactMaxCount, humanBytes(memCompactMaxBytes))
	return sb.String()
}

func humanBytes(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f KB", float64(n)/1024)
}
