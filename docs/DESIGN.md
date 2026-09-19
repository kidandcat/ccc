# ccc v3 — design

Status: implemented (Phases 2a and 2b, 2026-09-14; background jobs and
owner-only bot creation, 2026-09-15; owner secrets vault + blind inject,
2026-09-17). This document is the specification the
implementation follows. When code and this document disagree, fix one of them
in the same change. Everything below describes what ccc v3 actually does;
§14 lists where the built thing knowingly departs from the original plan, and
why.

## 1. What ccc v3 is

ccc is **sessions behind one Telegram DM**, driven by a coding CLI as a
stateless runner (Claude Code by default; Grok Build and Antigravity too),
with ccc owning everything the runner does not: session lifecycle, persistent
memory, scheduling, account (profile) management, access control, and the
Telegram UX.

The experience target: **the bot's 1:1 DM is General, the dispatcher**. You
talk to it; it sees live sessions in its envelope and can `spawn_session` /
`tell_session`. Sessions live in the backend — they have no Telegram topic.
Workers `report_to_general` only — they do not see the roster or message each
other. `/session <prompt>` is the escape hatch that starts a worker without
the dispatcher. The owner never writes into a session chat. Group messages
are ignored. No role, no `/role`, no ceremony.

Non-goals (explicitly dropped from v2): Claude Code background agents, the
agents view, `claude attach` handoff, transcript scraping, the AskUserQuestion
PreToolUse hook hack, a mesh of bots (`send_to_bot` between workers). One ccc
instance never talks to more than one Telegram bot.

## 2. Runtime model

| Concept | Definition |
|---|---|
| **Instance** | One `ccc listen` process on one machine, bound to one Telegram bot token. The owner's DM is General. Instance-level config: model, env passthrough, default profile, data dir. |
| **Profile** | One account for one engine (see `profiles.go`). Claude = one `CLAUDE_CONFIG_DIR`. Grok = isolated `GROK_HOME`. Antigravity = isolated HOME/`GEMINI_HOME`. Codex = isolated `CODEX_HOME`. Engine is set when the account is added. Same-engine accounts are interchangeable at turn granularity (§4). One instance may mix engines. |
| **Session** (`bots` table) | A backend worker. Identity = `name` + an **engine** from `pickSpawnEngine` (the configured account with the most 5-hour headroom; a cold cache falls back to the default account / `default_engine`) + an optional **model** override. Turns pick a healthy account of that engine and, if that pool is exhausted, fail over to another configured engine. `/engine` is a secondary pool assignment. `/model` in the DM sets the instance default. Claude, Grok and Codex sessions get the ccc MCP server (Claude: `--mcp-config`; Grok/Codex: isolated-home `config.toml`). Antigravity does not. Optional per-session `cwd` (default: `<data_dir>/bots/<name>/workspace`). Role is unused leftover. `archive_bot` archives the row. `topic_id` is the session key (not a Telegram forum topic): 0 is **General** (the owner's DM); any other value is a backend worker (new ones get `-id`). |
| **Conversation** | The engine transcript behind a session: a UUID ccc mints and resumes. A session has exactly one live conversation; `/new` rotates it. After `idle_compact_s` (default 1h) of no finished turn, ccc rotates it automatically — memories stay, the transcript does not. |
| **Turn** | One `claude -p` process: input = one user/system/background message (plus context envelope), output = streamed events until `result`. At most one turn per session at a time; further inputs queue (FIFO) and are delivered together on the next turn. A background job is **not** a turn: it must not hold `turns.status=running`. |
| **Background job** | A long-running shell command owned by a session, started with `run_background`. It runs in the session's cwd with `env_passthrough` while the topic stays responsive. Completion enqueues a `source=background` turn. |

## 3. Turn lifecycle

```
input (Telegram text | inbox message | schedule | watch diff | background job)
  → enqueue(bot)                       (SQLite: turns.status=queued)
  → pick profile                       (§4)
  → build envelope                     (§9)
  → spawn: engine CLI                  (§3.1) Claude: claude -p + claudeEnv(profile);
                                       Grok: grok --single; Antigravity: agy --print
  → consume stream-json                (§3.2) → Telegram progress message
  → on result: persist, post final text, run post-turn hooks (§3.3)
  → on failure: classify (§3.4) → retry on the other profile or surface
```

### 3.1 Invocation

First turn of a session:

```
claude -p --session-id <uuid> --output-format stream-json --verbose
          --permission-mode bypassPermissions          # decision: all bots bypass
          --system-prompt "<rendered template>"        # replaces Claude Code's prompt
          --mcp-config '<inline JSON pointing at `ccc mcp`>' --strict-mcp-config
          --setting-sources ''                         # no settings AND no CLAUDE.md (§14.1)
          --disable-slash-commands                     # no user/plugin skills (§14.1)
          --model <instance model>
          [--append-system-prompt …]                   # NOT used; see §9
          "<envelope + message>"
```

Later turns: same flags with `--resume <uuid>` instead of `--session-id`.
Flags must be passed on every turn (`--settings`/`--mcp-config`/`--add-dir` are
not restored on resume — verified on 2.1.259).

Rules:
- `cwd` = the bot's workspace dir. Coding work happens via absolute paths or
  the bot `cd`-ing in Bash. The repo's `CLAUDE.md` is therefore **not**
  auto-loaded; when a bot needs a project's conventions it reads them (the
  project registry in §5 points at them). The implementer must verify how
  `--setting-sources` interacts with `CLAUDE.md` discovery under `-p` and pick
  the combination that loads no `CLAUDE.md`, no project settings, and no
  auto-memory. `--bare` is NOT an option (it disables OAuth). Verified: only
  the EMPTY `--setting-sources` achieves this (§14.1); `runner.go` carries the
  probe and one comment per flag.
- Environment: `claudeEnv(profile)` whitelist only (already implemented) plus
  the instance's `env_passthrough` list (e.g. `GH_TOKEN`, `SLACK_USER_TOKEN`,
  `LINEAR_API_KEY`). Never inherit `CLAUDE*`/`ANTHROPIC*` from the parent.
- The MCP config points at `ccc mcp --bot <id> --turn <id>` (stdio). ccc is
  therefore both the Telegram client and an MCP server binary (§6).
- Streaming: `--output-format stream-json`. `--include-partial-messages` is NOT
  used: per-message granularity is enough for the progress line.

### 3.2 Progress in Telegram

General (the DM) gets one progress message per turn, posted with
`disable_notification` and edited in place (rate-limited to ~1 edit / 3 s):
current tool activity summarized ("editing poller.go", "running go test",
"reading PR #1234"), elapsed time. Telegram does not notify on
`editMessageText`, so when the turn ends the silent progress message is
deleted and the final assistant text is posted as a new message (the one
notification the owner gets; overflow chunks stay silent). A ✅ reaction is
added to the user's triggering message. Tool call payloads are never dumped
into the chat; `thinking` is never shown.

Backend workers do not post progress or final answers to Telegram. The owner
does not see General↔session messages (prompts, reports, transcripts). A
**live status card** in the owner's DM is posted silently and edited in
place: one block per working session (name, running/waiting/job, current
tool or pending `ask_owner` line). It is **pinned** (`pinChatMessage`,
`disable_notification`) while any worker is running, waiting, or has a
background job, and unpinned when that work ends. The same message is
reused across turns (id in `settings.session_panel_msg_id`). `/sessions`
stays the full list including idle sessions. The card is not a substitute
for an answer. After a worker turn that reported (`report_to_general`) or
finished with last-message output, listen wakes General with that inbox
(relay). General MUST post a short owner-facing summary in the DM. If
General cannot (60s cap, crash, empty reply), listen posts a short fallback
from the worker's last message — not the full transcript, not only a status
line. `notify_owner`, `ask_owner` and job pings still reach the owner.
Idle-session reminders wake General via inbox + enqueue once per idle spell
(same path as `report_to_general`); they are not posted to the DM and do not
use the fallback.

### 3.3 Post-turn

- Persist `turns` row: profile used, duration, cost/usage from `result`,
  `stop_reason`, session id.
- If the turn ended with `ask_owner` pending, the session is marked `waiting`.
  Workers stay parked until a button tap or a reply-to that question (answers
  are inputs). General keeps taking DM turns while a question is pending:
  free text is never the answer, so blocking the dispatcher would swallow it.
- If `set_name` changed the session name during the turn, rotate the
  conversation now that the turn has recorded its id (§14.14).
- Leftover `inbox` rows (from the old inter-session `send_to_bot` path) are
  still delivered if any exist; that tool is not registered and is not a
  product feature.

### 3.4 Failure classification and failover

Classify `stderr`/`result.error`/exit code into:
- `auth_stale` — matches the known text `organization has disabled Claude
  subscription access` or `Not logged in`/`login` variants → mark the profile
  `needs_login`, notify owner with a **Relogin** button (§8), retry the turn
  once on another healthy profile.
- `rate_limited` — usage/rate limit text → put profile in cooldown until the
  cached `resets_at` (or 30 min), retry once on another profile.
- `transient` — network/5xx → retry once, same profile, after 10 s.
- `fatal` — anything else → post the error summary to the topic, mark turn
  failed, dequeue.

A retried turn reuses the same session UUID when it stays on the same
engine: Claude profiles share `projects/` (§4) so the other account can
resume. When the current engine's pool is exhausted, failover **crosses
configured engines** (Claude → Codex, Grok, …). Transcripts do not transfer
across CLIs, so that hop starts a fresh conversation and reassigns the
session's engine. Codex thread-loss text (`thread not loaded` and
`thread … not found`) is classified as `session_lost` like Claude's.

### 3.5 General turn timeout

The dispatcher (topic id 0) has a **60 second cap** on **owner** turns
(`source=user` and the timeout follow-up). Other sessions do not. Session
reports (`source=bot`), idle nags, watches, schedules and routines are the
dispatcher's job and are **not** capped: killing those at 60s left the owner
with only "session ended" while General never summarized. When the cap
fires, ccc SIGTERMs the engine process (without dropping the queue — this
is not `/stop`). If this turn already `spawn_session` / `tell_session`'d,
that is the handoff. Otherwise ccc **starts a backend worker itself** (same
path as `spawn_session` / `/session`) with the **owner's** request
(`source=user`) plus a short note that General timed out, and the live
session card in the DM picks up the new worker.
Session reports, watches, schedules and routines are not owner work: if
one still fails (crash, empty reply) it does not auto-spawn and does not
inject a follow-up (that duplicated turns). A failed **relay** report still
posts listen's short owner-facing fallback from the worker's last message.
The owner is never told to `/session`.
A first timeout of owner work also enqueues a `source=system` turn whose
input is an error: this work is too long for General — spawn if you still
can, and do not spawn a duplicate if ccc already started one. The owner is
not shown a ❌; the injection *is* the next turn. A timeout of that
follow-up is not injected again (avoids a loop); if the episode still has
no handoff, ccc auto-spawns then. A follow-up turn that finishes without
`spawn_session` / `tell_session` is the same auto-spawn.

## 4. Profiles: selection and shared sessions

- `pickAccount(engine)` chooses per **turn** among accounts of that engine:
  lowest cached 5-hour utilization (Claude; Grok/Codex map their first window
  onto the same field), then lowest 7-day, then fewer turns currently running
  on that account (read from `turns.status = running`, §14.6), then name;
  excludes
  profiles in cooldown or `needs_login`. Same-engine failover is
  Claude↔Claude, Grok↔Grok, Codex↔Codex. When that pool is exhausted,
  `execute` hops to another **configured** engine (`nextEngineByHeadroom`)
  with a fresh conversation and reassigns the session. It does not invent an
  implicit CLI the owner never added.
  New workers (`spawn_session`, `/session`, routine fires, 60s auto-spawn)
  pick the **engine** of the account with the most 5-hour headroom across
  engines (`pickSpawnEngine`, usagefetch 5 min cache). A cold cache (no
  snapshot at all) falls back to `default_engine`. Once any account has
  numbers, an engine whose usage is still unknown is treated as idle so a
  configured Codex/Grok account is not starved by the default Claude
  profile. Utilization is fetched on `/account`, `/status`
  and the doctor (5 min TTL).
  Each window is `5h 62% · reset 1h20m` / `7d 40% · reset 3d` / `week 8% · reset 5d23h`
  when the API gives a reset time; omit ` · reset …` when it does not (do not
  invent a clock). Sources:
  - **Claude** — `GET https://api.anthropic.com/api/oauth/usage` with the
    profile's OAuth token (macOS keychain `Claude Code-credentials`, or
    `<config_dir>/.credentials.json`). `.claude.json`'s
    `cachedUsageUtilization` is a fallback: Claude Code 2.1.x often no
    longer writes it, which is why `/account` used to show `5h ? · 7d ?`.
    Reset is `five_hour.resets_at` / `seven_day.resets_at` (RFC3339; may be
    JSON null) or the matching `limits[].resets_at` (`session` / `weekly_all`).
  - **Grok** (Grok Build / SuperGrok OAuth, not an xAI API key) —
    `GET https://cli-chat-proxy.grok.com/v1/billing?format=credits` with the
    access token in `$GROK_HOME/auth.json`. Display is the weekly pool
    (`creditUsagePercent`, typically `week N% · reset …`). Reset is
    `currentPeriod.end`, falling back to `billingPeriodEnd`. Token refresh is
    `POST https://auth.x.ai/oauth2/token`. The Management API prepaid
    balance (`GET /v1/billing/teams/{id}/prepaid/balance`) is API-key
    billing and is not this login.
  - **Codex** (ChatGPT subscription in isolated `CODEX_HOME`, not a
    platform API key) — `GET https://chatgpt.com/backend-api/wham/usage`
    with `Authorization: Bearer` + `ChatGPT-Account-Id` from
    `$CODEX_HOME/auth.json`. Windows are labelled from
    `limit_window_seconds` (5h / 7d). Reset is `reset_at` (unix seconds),
    else `now + reset_after_seconds`. Token refresh is
    `POST https://auth.openai.com/oauth/token`. An API-key Codex login
    shows `n/a (API key, not ChatGPT quota)`.
  - **Antigravity** — `n/a (no public usage endpoint)`.
- **Shared sessions**: all profiles of an instance point their `projects/` at
  the same directory (`<data_dir>/projects`, symlinked into each config dir by
  `ccc` when a profile is added; for the implicit `~/.claude` profile the
  instance symlinks the other direction and documents it). Transcripts are
  plain files, so any account resumes any session. `jobs/`, `daemon/` and
  credentials stay per profile.
- `/account` (§8) is the only UI for profiles; the `ccc profile …` CLI remains
  as the underlying functions, and accepts an address or a legacy name.

## 5. Data model (SQLite via GORM, `<data_dir>/ccc.db`, WAL, FK on)

```
bots        id, name (unique), topic_id (0=General/DM, !=0 backend worker; new workers get -id), role (text), cwd, session_id, engine (claude|grok|antigravity, default claude),
            status (idle|running|waiting|disabled), created_at, archived_at, parent_bot_id (legacy; unused — bots cannot spawn children),
            idle_reminded_at (when General was last woken about this idle session)
turns       id, bot_id, session_id, profile, source (user|bot|schedule|watch|routine|system|background), input (text),
            output (text), status (queued|running|done|failed), stop_reason, error_class,
            started_at, ended_at, usage_json
inbox       id, to_bot_id, from_bot_id (nullable = owner/system), text, wake (bool), delivered_at, turn_id
memories    id, scope (user|project|bot), scope_key (''|project path|bot id), key, text,
            created_by_bot_id, created_at, updated_at        -- unique(scope, scope_key, key)
memories_archive  id, compaction_id, archived_at, scope, scope_key, key, text, created_by_bot_id,
            memory_created_at, memory_updated_at            -- what a compaction replaced (§7)
projects    id, path (unique), name, description, stack, deploy_notes, updated_at
watches     id, bot_id, name, command, interval_s, last_hash, last_output, last_run_at, enabled, created_at
            -- created_at is the TTL clock (watch_ttl_s, default 4h); re-upserting the name renews it
schedules   id, bot_id, fire_at, note, recurring_cron (nullable), fired_at
questions   id, bot_id, turn_id, question, options_json, answer, asked_message_id, answered_at
settings    key (pk), value                                  -- instance settings edited from Telegram
background_jobs  id, bot_id, name, status (queued|running|done|failed), kind (shell),
            command, env_json (env-var → secret name, names only), stdin_secret (name or empty),
            pid, deadline, exit_code, output, error, cancel_requested,
            created_by_turn_id, started_at, ended_at, created_at
            -- artifacts: <data_dir>/background/<id>/{out.log,exit.code,pid}
            -- output stored in sqlite is redacted of vault values; out.log on disk is not
```

Owner vault values are **not** in SQLite. They live at `<config_dir>/secrets`
(0600 JSON object, name → value). See §16.

Indexes beyond the ones the columns above imply: `turns(bot_id, created_at)`
for turn retention, `inbox(delivered_at)` and `questions(answered_at)` for
cleanup, `memories_archive(compaction_id)` for restore.

Settings actually used: `last_maintenance` (ccc's own marker) and
`session_panel_msg_id` (Telegram id of the live session card in the DM).
The table is bookkeeping only — nothing in it is user-editable.

Existing `config.json` (bot token, group id, profiles) stays as bootstrap
config, and it is also where the three tuning knobs live: `debounce_ms`
(default 2500, §14.18), `compaction_model` (default `haiku`, §7) and
`maintenance_hour` (default 4). They are set with `ccc config set <key>
<value>`, printed by `ccc config`, and have no Telegram command (§14.18).
Everything else runtime lives in SQLite. The v2 `sessions` map and the
JSONL ledger are not migrated (v3 is a fresh start; document it).

## 6. MCP server (`ccc mcp`)

Stdio JSON-RPC MCP server, one process per turn. Claude gets it via inline
`--mcp-config`; Grok and Codex via `[mcp_servers.ccc]` in the isolated account
home (Codex also gets per-turn `exec -c`). Identity is `--bot`/`--turn` or
`CCC_BOT_ID`/`CCC_TURN_ID` in the environment. Tools:

| Tool | Input | Behavior |
|---|---|---|
| `remember` | `scope` (user\|project\|bot), `key`, `text`, `project_path?` | Upsert a memory. `bot` scope is this session (not a persona). |
| `recall` | `query`, `scope?`, `limit?` | Full-text (SQLite FTS5) search over memories visible to this session: all `user`, all `project`, own session. Returns key+text+scope. |
| `forget` | `scope`, `key`, `project_path?` | Delete one memory. |
| `notify_owner` | `text`, `urgency` (normal\|urgent) | Post in General (the DM), labelled with the session name. Interruptions only — not a report dump. |
| `ask_owner` | `question`, `options?` (≤4 strings; last is always Omitir) | Post question with inline buttons in General. ccc always appends **Omitir** as the last button (3 content options + Omitir; if the model passes 4, the last is replaced). Tapping Omitir closes the question without choosing A/B and unblocks the worker (`The owner skipped the question "…" without choosing…`); similar pending asks are skipped too. Free-text questions (no content options) still take a reply-to and still get Omitir. Returns immediately with `{"status":"asked"}`; the session should end its turn. A content answer arrives as `Answer to "<question>": …` via a button tap or a reply-to that question in the DM. Free text in the DM is never an answer (it is always General) and, if anything is still pending, listen posts a list of unanswered questions with their buttons — no timeout. Tapping one also answers similar pending questions (same normalized text, answer maps onto their options) so General does not re-ask. Prompt contract: mandatory for every owner decision (yes/no, pick one, architectural fork); recommended option first; never ask in chat prose. |
| `set_name` | `name` | Rename this session: validate (§8 `/name`), update `bots.name`. Rotates the conversation (§14.14). `/name` in the DM only hits General, which refuses. No topic icon. |
| `watch` | `name`, `command`, `interval_s` (≥60) | Register a deterministic watch (§7). Polling tool: no change = zero tokens. Lasts `watch_ttl_s` (default 4h); re-upserting the name renews it. `unwatch(name)`, `list_watches()`. |
| `schedule_wakeup` | `in_seconds` or `at` (RFC3339), `note`, `cron?` | Wake at a time (§7). Each fire is a full turn. Not for polling. `cancel_schedule(id)`. |
| `set_routine` | `name`, `prompt`, `cron`, `timezone?` | Named recurring work. Each fire starts a fresh worker (§7). `list_routines()`, `cancel_routine(name)`. |
| `run_background` | `command`, `name?`, `env?`, `stdin_secret?` | Queue a long-running shell command in the bot's cwd with `env_passthrough`. Optional `env` is env-var-name → vault secret name (names only). Optional `stdin_secret` is a vault name piped to stdin. Values are never stored on the row. Returns a job id immediately; does not block the turn. Use when Bash/a tool is expected to exceed ~60s. |
| `secrets_list` | — | Owner vault names, sorted. Values are never returned. **There is no `secrets_get` / `secrets_show`.** |
| `secrets_delete` | `name` | Delete one vault secret by name. |
| `run` | `command`, `env?`, `stdin_secret?` | Foreground `/bin/sh -c` in the session cwd (2 min cap). `env` maps env-var-name → vault secret name; `stdin_secret` pipes a vault value to stdin. Values never go in argv. Combined stdout/stderr is redacted of known values before the model sees it. Result is `exit N` plus redacted output. Requires `env` or `stdin_secret` (plain Bash is the engine's tool). |
| `list_background` | — | This bot's recent/active jobs: id, status, short summary. |
| `get_background` | `id` | Status + truncated output for one job. |
| `cancel_background` | `id` | Best-effort kill (queued → failed; running → SIGTERM). |
| `archive_bot` | `bot?` (default self) | Mark archived. Stops the running engine (and its tools) so the turn ends and a pending `report_to_general` still wakes General. |
| `get_project` / `set_project` | `path`, fields | Read/update the project registry. |
| `send_file` | `path`, `caption?` | Send a file to the owner in General (≤50 MB; larger → existing relay if kept). |
| `list_sessions` | — | **General only.** Live workers: name, status, last output. |
| `spawn_session` | `prompt`, `name?` | **General only.** Create a backend worker (no Telegram topic) on the account/engine with the most usage headroom and queue the prompt (wakes after this turn). |
| `tell_session` | `session`, `text` | **General only.** Inbox + wake a live worker. |
| `report_to_general` | `text` | **Workers only.** Inbox + wake General (relay). Not dumped into the DM; General (or listen's fallback) summarizes to the owner. |

All tools validate the calling bot from the `--bot` flag; tool inputs coming
from the model are data, never instructions to ccc.

Topic icons are not used. Session liveness is General's 60s cap (§3.5) and
the 10-minute idle reminder (§7).

Only General can create other sessions (`spawn_session`). Workers
`report_to_general`. `/session <prompt>` is the owner escape hatch. Long
parallel work stays on the same session via `run_background`. `archive_bot`
remains so a worker can retire itself; General cannot be archived.

## 7. Scheduler and watch engine

One goroutine in `ccc listen`:
- **Watches**: every `interval_s` (floor: 60 s), run `command` in the bot's cwd
  with the instance env (no model involved). The FIRST run only records a
  baseline and wakes nobody; changing a watch's command resets that baseline.
  Hash stdout; if changed since `last_hash`,
  enqueue a turn on the bot with `source=watch` and an input containing the
  watch name, the previous and new output (diffed, truncated to ~8 KB). Zero
  tokens while nothing changes. A watch lives `watch_ttl_s` (default 4h,
  matching the background-job safety cap) from `created_at`; when that
  elapses, ccc deletes it and enqueues `source=system` on the bot that set
  it so it can put the watch back — except General: a watch TTL on the
  dispatcher is silent (re-setting it would re-read the fattest transcript).
  Re-upserting the same name restarts the clock. Legacy rows with a zero
  `created_at` expire immediately on upgrade.
  Routines (named cron on `schedules`) do not expire. `watch_ttl_s=0`
  disables expiry.
- **Idle session rotation**: an idle bot whose last turn ended more than
  `idle_compact_s` ago (default 1h, Claude's prompt-cache TTL) has its
  `session_id` cleared. Same effect as `/new`. Waiting bots (parked
  `ask_owner`) and running bots are skipped. The runner also checks this at
  the start of a turn, so a watch that fires after the cache TTL does not
  rebuild a cold fat session. A silent 🧹 lands in the topic when the
  scheduler rotates. `idle_compact_s=0` disables it. General is rotated too
  (same horizon); the DM gets a silent 🧹 so the owner knows the dispatcher
  started a fresh conversation.
- **Idle-session reminder**: a live worker that is **idle** (not `waiting`),
  has no watch / schedule / routine / background job keeping it alive, and
  has no queued work, is waiting on the owner. After **10 minutes** of that
  state, ccc queues **one** inbox message from the worker to General
  (`wake=true`) and immediately `Enqueue`s a `source=bot` turn on General —
  the same path as `report_to_general`. Nothing is posted to Telegram.
  `idle_reminded_at` latches until the worker leaves idle-waiting (run,
  keepalive, `ask_owner`, archive). A new spell after more work can remind
  once more. General then decides: `ask_owner`, `tell_session`, archive, or
  ignore. A parked `ask_owner` (`waiting`) is not reminded: the question is
  already in the DM.
- **Schedules**: unnamed `schedule_wakeup` enqueues a turn with
  `source=schedule` and the note on the owning bot when `fire_at` passes;
  recurring via cron expression. Polling a command is a watch, not a
  wakeup (a no-change wakeup is a full turn; a no-change watch is free).
- **Routines**: named cron on `schedules`. Each fire starts a **backend
  worker** (`routine-<name>`) with a short prompt and
  `source=routine:<name>`, then posts ⏰ in General. It does not run as a
  turn of the owning bot (usually General) — that inherited the
  dispatcher's whole transcript. A live worker with that name is reused
  with `session_id` cleared so a daily fire does not reread yesterday or
  pile idle sessions that nag General. The worker is told to do the work,
  `report_to_general`, and `archive_bot`. Unnamed one-shot wakeups are
  unchanged.
- **Background jobs**: every second, claim queued `background_jobs` (cap: 8
  running per bot) and start a detached `/bin/sh` wrapper in the bot's cwd
  with the instance env (same as watches: `env_passthrough`, no Claude
  profile). The wrapper runs the user command, redirects combined
  stdout/stderr to `<data_dir>/background/<id>/out.log`, and writes
  `exit.code` when it finishes. The process group is its own (`Setpgid`);
  listen's context is not the job's lifetime. The 4h safety cap is a
  `deadline` column the supervisor checks. On graceful shutdown the jobs
  are left `status=running` with their PID. A new listen **reattaches**:
  `exit.code` present → finish from the log and enqueue `source=background`;
  PID still alive (`kill(pid, 0)`) → keep supervising and ping ▶️ in the
  bot's topic; PID dead and no exit file → fail with a clear error, ping ❌
  in the topic, and wake the bot. A job that fails for any other reason
  (non-zero exit, timeout, process death) also pings ❌ immediately — the
  owner must not wait for the model to notice. A cancel is quiet: the owner
  already asked for it. This is **not** Claude Code background agents: no
  transcript scraping, no `claude attach`.
- **Doctor loop** (every 15 min): `claude auth status --json` per profile
  (exit code 1 = logged out — verified), disclaimer check, usage cache read.
  Transitions to `needs_login` → owner notification with **Relogin** button.
- **Maintenance** (once a day, at `maintenance_hour`, default 04:00 local; also
  `ccc maintain`): growth control, below. The marker is a DATE, so a machine
  that was asleep at 04:00 catches up when it wakes, and runs once either way.

### 7.1 Maintenance: growth control

Three steps, in `maintenance.go`. Nothing here is fatal: a step that fails is
reported (to the CLI, and to the owner in General when it abandoned a
compaction) and the rest still runs.

**Turn retention.** A turn is deleted only when it fails BOTH halves of "keep 30
days OR the last 200 per bot, whichever keeps more": it is older than 30 days
AND outside its bot's most recent 200. Turns older than 7 days keep their row,
their status and their `usage_json` — `/usage` reads those long after the text
is gone — but `input`/`output` are replaced by the first 500 characters plus a
marker. The cut is plain truncation, not a model call: summarising thousands of
rows would cost more than the bytes are worth, and the marker is also how the
next run recognises a body it already trimmed.

**Memory compaction, by threshold and not by calendar.** Per scope (`user`, each
`project` key, each `bot`): when the scope holds more than 120 entries or more
than 48 KB of key+text, ONE turn on a cheap model (`compaction_model`, default
`haiku` — verified accepted by 2.1.270; an unrecognised name falls back to the
instance model) rewrites it. That turn runs through the runner but **outside any
bot**, on **any healthy configured engine** (a Codex-only instance must still
compact): a fresh conversation nobody resumes, no MCP server at all (so it
cannot touch the database it is compacting), plain text in and out
(`claudePlainArgs` on Claude; Grok/Codex use a throwaway home with only
`auth.json`). The prompt is the scope as `key: text` lines,
oldest first, plus instructions to merge duplicates, drop what a later entry
contradicts, keep every distinct durable fact, and answer in the same format and
nothing else.

The result is parsed strictly — every non-blank line must be `key: text`, and a
bullet, a heading, a fence or a preamble fails the whole parse — because
guessing which lines were meant to be memories is how facts get lost. On a parse
failure, or when the result keeps less than 40% of the entries (i.e. more than
60% disappeared), NOTHING is applied and the owner is told why.

Applying is one transaction: the old rows are copied into `memories_archive`
under a fresh dense `compaction_id`, the scope's rows are deleted, the new ones
are inserted. The owner gets `🧹 Compacted user memories: 143 → 61 (/memory
restore <id> to undo)` in General. `/memory restore <id>` (owner only) puts the
originals back and consumes the archive rows; `/memory stats` shows count, bytes
and last compaction per scope.

**Cleanup.** Delivered `inbox` rows and answered `questions` older than 30 days;
finished `background_jobs` older than 30 days (and their
`<data_dir>/background/<id>/` artifact dirs);
`bot`-scope memories of bots archived more than 30 days ago (nothing can read
them — bot memories are visible to that bot alone); `memories_archive` rows
older than 90 days.

## 8. Telegram UX

### Conversation
- Plain text in the bot's **1:1 DM** → a turn of General (the dispatcher,
  topic id 0, created on listen). `/session <prompt>` starts a backend
  worker named from the first line and dispatches that prompt. General may
  `spawn_session` the same way. The owner never writes into a session chat.
  Group messages are dropped.
- Photos/documents → saved into General's workspace `inbox/`, path passed
  in the message. Voice → transcribed if the `voice` build is present, else
  the file path is passed (keep the existing whisper integration).
- `ask_owner` → inline buttons `q:<question_id>:<option_idx>` (last button is
  always **Omitir**). Tapping a content option ticks the original question
  message with ✓ and enqueues the answer. Tapping Omitir closes the question
  without choosing and unblocks the worker (skip envelope, not `Answer to`).
  Free-text answers: only a reply to that question message (Omitir is still
  offered). Free text in the DM never answers; if anything is still pending,
  listen posts a list of unanswered questions with their buttons (no timeout).
  Tapping one also answers similar pending questions (same normalized text;
  the tapped label maps onto their options; Omitir skips siblings even if they
  have no skip button) so General does not re-ask.
- An **edited** message is gated like any other update. If its text starts with
  `/` it goes through the same command dispatcher — editing a mistyped command
  in place is how a phone corrects one — deduped by
  `(chat_id, message_id, edit_date)` so a redelivered edit runs once. An edited
  plain message still does nothing: re-running a turn because somebody fixed a
  typo is worse than ignoring it.

### Commands
| Command | Where | Effect |
|---|---|---|
| `/sessions` | DM | List of open sessions (name, status, last turn). `/bots` is an alias. The live card (pinned while there is work) is a separate silent message, not this command. |
| `/name [text]` | DM | Show General's name. Rename is refused (General stays General). Workers rename via `set_name`. |
| `/new` | DM | Rotate General's conversation (fresh transcript, memory kept). |
| `/stop [name\|all]` | DM | Kill the named session's running turn and drop its queue. Bare `/stop` kills General if it is running (or has queued work); if that did nothing, it stops every live worker. `/stop all` stops every live session. Waiting (`ask_owner`) sessions stay parked. A hung CLI is SIGTERM, then SIGKILL after 5s. Workers (and General non-owner turns) also have `worker_turn_timeout_s` (default 30m, 0 disables). |
| `/cwd [path]` | DM | Show or set General's working dir. |
| `/engine [name]` | DM | Assign General to an engine's account pool (`claude`, `grok`/`grok-build`, `antigravity`/`agy`). Rotates the conversation. Engine itself is set at `/account add`. |
| `/memory [query]` | DM | List/search memories visible to General; `/forget <scope> <key>`. |
| `/memory stats` | DM | Per scope: entries, bytes, whether it is over the compaction threshold, last compaction (§7.1). |
| `/memory restore <id>` | DM | Undo one compaction. Owner only. |
| `/usage` | DM | Tokens, cache hit ratio, turns, average duration and cost per session, today and last 7 days (§14.19). |
| `/watches`, `/schedules` | DM | List and cancel General's watches/schedules. |
| `/account` | DM | Status card per account (engine + health + usage/limits) with buttons; subcommands `status`, `add <identity> <engine>`, `login`, `remove`, `default`. |
| `/model [engine] [slug]` | DM | Show each account with the model currently selected for it (inline picker). One slug sets Claude's default. Two args (`/model grok grok-4`) set that engine. `/model default` clears. |
| `/access` | DM | Show the config whitelist (`chat_id` + `allowed_user_ids`). Owner only. Mutating the list is `ccc config set allowed_user_ids`. |
| `/secret add <name>` | DM | Owner only. Prompt for the value; the next owner message is captured by listen and never sent to the model (§16). |
| `/secret list` | DM | Owner only. Names only. |
| `/secret delete <name>` | DM | Owner only. |
| `/status` | DM | Instance health: profiles (with per-engine usage/limits), running turns, queue, doctor findings. |

### Account management from Telegram (login without a terminal)
Engine is defined when the account is added (`/account add <identity> <engine>`),
not by flipping `/engine` on a session. Claude identities are the **email** of the
Claude account; Grok, Antigravity and Codex accept a short name or email. The
directory behind the account is never shown:

- The profile key in `config.json` is unique per **identity + engine**. Claude
  keeps the address, lowercased, so existing Claude-only configs stay valid.
  Every other engine uses a composite key (`jairo@agentero.com/codex`). The
  same email may exist once per engine — `/account add you@x.com claude` and
  `/account add you@x.com codex` are two accounts. `label` is the human
  identity (the email); status cards show that, with the engine beside it.
  The config dir is derived from the identity (`jairo@agentero.com` →
  `<data_dir>/profiles/jairo_at_agentero.com`, or
  `<data_dir>/accounts/<engine>/…` for non-Claude) and is never shown in
  Telegram. `/account login you@x.com` works when only one profile has that
  email; when several engines share it, the owner specifies the engine
  (`you@x.com/codex` or `you@x.com codex`).
- The address is learned from `claude auth status --json` and cached in the
  profile's `label`. Every doctor run (§7) refreshes it, and a profile that is
  still keyed by a legacy name is re-keyed onto its address then — the config
  dir does not move, and the legacy name keeps resolving until the migration
  happens. That is also how the pre-existing `~/.claude` account gets an entry
  and stops being shown as `default`.
- After a login, what the owner typed is compared with what `auth status`
  reports; a mismatch is reported and the account is stored under the address it
  actually logged in as.
- An argument that is not a plausible email (`x@y.z`) is answered with a
  one-line explanation. Only a genuinely empty argument gets a usage line.

`/account add <email>` (or **➕** button):
1. ccc creates the derived config dir under `<data_dir>/profiles/`, symlinks
   `projects/`, registers the profile under the address.
2. Runs `claude auth login` in a **pseudo-terminal** (`github.com/creack/pty`)
   with `claudeEnv(profile)` **plus browser suppression** (§14.8): a directory
   of no-op `open`/`xdg-open` shims first on `PATH` and `$BROWSER` pointed at
   one of them. Parses the login URL from the PTY output and posts it. The user
   opens it on the phone with the right account and pastes the code back into
   the chat; ccc writes it to the PTY. Success is confirmed by
   `claude auth status --json`, never by the TUI.
3. Disclaimer: ccc records it **directly** — `acceptBypassDisclaimer` merges
   `skipDangerousModePermissionPrompt: true` into the profile's `settings.json`
   (0600, atomic, other keys kept) and re-checks it with `bypassAccepted`. That
   key is the whole acceptance, so no TUI is driven for it (§14.23).
4. Times out after 10 min; the partial profile is removed.
`/account login <email>` (or `<email>/<engine>` when the same identity exists
on more than one engine) runs steps 2–3 only. Every PTY string and pattern lives
in `ptyflow.go`, each with a note on how it was verified against 2.1.270
(§14.9). Select lists are answered by reading the option number off the screen,
never by assuming a position.

### Access control
- Owner = the Telegram user id from bootstrap config (`chat_id`); always allowed.
- Extra allowed user ids live in `config.json` as `allowed_user_ids`, loaded at
  process start. Restart listen to change the list
  (`ccc config set allowed_user_ids <id,id,…>`). `/access` lists it; it does
  not mutate it.
- Unknown DMs and group messages are dropped in silence: no reply, no DB row,
  no owner notify, no pairing code. Messages, EDITS and callback queries are
  gated the same way.
- Allowed users may talk in the DM (General); only the owner can use
  `/account`, `/access`, `/model` and `/secret`.
- Until `chat_id` is configured there is no owner, so nobody is allowed.

## 9. System prompt and context envelope

**System prompt** (`--system-prompt`, rendered once per conversation; a change
of name or template rotates it):

```
You are a coding assistant in a backend session named <name>.
You run on machine <hostname>, working dir <cwd>.
Tools: you have the ccc MCP tools (memory, scheduling, watches,
background jobs, secrets_list, run) plus the standard tools (Bash, Read, Edit, …) with full
permissions. You cannot create other sessions. There is no secrets_get.
Rules: … (owner escalation, when to remember, never print secrets, keep
replies short for chat, always ask_owner with Telegram buttons for yes/no,
pick-one, or an architectural fork — up to 3 content options, recommended
first, last button always Omitir — never ask in chat/transcript prose;
omit options only when the answer cannot be a button…)
```

General's prompt is a dispatcher variant: the owner's DM, `spawn_session` /
`tell_session` / `list_sessions`, no `set_name` / `archive_bot`, and the 60s
cap (§3.5). It is still byte-stable (no live roster in the prompt).

**Envelope** (prepended to every input, because system-prompt changes are
ignored on resume until compaction):

```
<context>
today is <weekday date time tz>
active sessions: (General only — live workers, status, last output)
recent user memories (top 10 by recency/relevance to the message)
project memories for cwd (if any)
own session memories (top 10)
pending inbox summary (N messages from X)
</context>
<message source="user|watch:<name>|schedule|background">…</message>
```

Keep the envelope under ~4 KB; `recall` exists for everything else.

### 9.1 Prompt-cache discipline

The API's prompt cache keys on a PREFIX of the request, and the system prompt is
the first thing in it. One byte that differs between two turns of the same
conversation invalidates the cache for the whole conversation, and the entire
history is re-charged as fresh input. So the split above is not only about
14.5's snapshot: it is what makes a resumed turn cheap.

Audited and enforced (§14.20): the system prompt holds only facts fixed for the
life of a conversation — name, hostname, cwd, the tool list. There is no other-
session roster, no role, and no topic-icon list. The date was never in the
prompt. Everything per-turn (date, memories, leftover inbox) is in the
envelope, which is the TAIL of the request: it costs only itself and
invalidates nothing.

`/usage` reports the cache hit ratio (`cache_read / (cache_read + input)`) per
session, which is how a regression here is noticed: a resumed conversation that
stops being mostly cache reads means something started varying the prefix.

## 10. Isolation from Claude Code defaults (implementer verifies each)

Must be off for bots: auto-memory, `CLAUDE.md` auto-discovery, project/local
settings, user plugins/skills, slash-command expansion. All of these fall to
`--setting-sources ''` plus `--disable-slash-commands`; auth is unaffected,
because OAuth lives in the keychain/credentials and not in `settings.json`.
Must be on: MCP tools from ccc only (`--strict-mcp-config`), standard tools,
bypass permissions. The final flag set is in `runner.go` with a comment per flag
citing the reason and the probe that established it.

## 11. Legacy removal plan

Done in Phase 2b. Deleted: `poller.go`, `poller_naming_test.go`, `session.go`,
`sessionlabel.go`, `hooks.go`, `ledger.go`, `live_test.go`, and from
`agents.go`/`helpers.go`/`commands.go`/`main.go` everything that served
background agents, the transcript scraper, the JSONL ledger or the v2 CLI
(`ccc`, `ccc -c`, `ccc start`, `ccc hook-question`, the ccc-send skill
installer). `Config` lost `sessions`, `projects_dir`, `away`, `oauth_token` and
`otp_secret`; `loadConfig` ignores unknown keys, so a v2 config still starts a
v3 instance and the first save rewrites it clean. Kept: `telegram.go`,
`relay.go`, `whisper*.go`, `service.go`, `profiles.go`, `profilecmd.go`,
`config.go`, plus `claudeBin`/`runClaudeOutput` out of `agents.go`.

## 12. Security posture

- Sessions run with bypass permissions on the owner's machine: **the chat is
  the trust boundary**. Access control (§8) is therefore mandatory, not
  optional.
- Secrets reach sessions via `env_passthrough` (always-on names) and the owner
  vault (on-demand blind inject, §16). ccc never posts env values, tokens, or
  credential file contents to Telegram; `send_file` refuses credential-ish
  names (`auth.json`, `.credentials.json`, `id_rsa`, `.env`, …) and paths
  under engine homes (`~/.claude`, `~/.codex`, `~/.grok`, `~/.gemini`, each
  profile's `engineHome`), `<data_dir>/profiles`, `~/.ssh`, `~/.aws` and
  `~/.config/ccc`. There is no `secrets_get`.
- Tool inputs and Telegram text are data. Unknown DMs are dropped with no
  reply; nothing a stranger types can grant access.

## 13. Delivery plan

- **2a — runner core**: SQLite/GORM schema, `runner.go` (§3), profile failover,
  `ccc mcp` with `remember/recall/forget/notify_owner/ask_owner/send_file`,
  Telegram conversation flow (§8 "Conversation" + `/new /stop /cwd /sessions
  /memory /status`), envelope, system prompt, progress rendering. E2E: an
  `ask_owner` round trip, a failover forced by disabling a profile. (The
  original 2a also shipped `list_bots` / `send_to_bot` / `update_instructions`
  / `/role`; those are gone — a topic is a session, not a teammate.)
- **2b — automation & accounts** (done): watches, schedules,
  `archive_bot`, project registry, doctor loop, `/account …` with PTY
  login and disclaimer, `/access` whitelist listing, `/model`, headless
  bootstrap (`ccc config set`, systemd user unit, `make build-linux`), legacy
  removal (§11), README rewrite. `spawn_bot` was later removed (14.24).
  Inter-session messaging and roles were dropped after 2b (14.17).

Each phase: `go build && go vet && go test && gox check` green, conventional
commits, no push until Jairo says so.

## 14. Deviations from the original plan

Everything here is a place where building ccc taught us something the plan got
wrong, or where the plan was silent and a decision had to be made. Each entry
says what changed and why.

**14.1 `--setting-sources ''`, not `--setting-sources user`.** §3.1 originally
asked for `user`, on the assumption that `--system-prompt` already suppressed
`CLAUDE.md`. It does not. A probe against 2.1.270 — a workspace holding a
`CLAUDE.md` with a token, and a `~/.claude/CLAUDE.md` holding another, asking
the model which it could see — gave:

```
no flags               -> project=yes user=yes
--system-prompt only   -> project=yes user=yes   (!)
--setting-sources user -> project=no  user=yes
--setting-sources ''   -> project=no  user=no
```

Only the empty value loads no `CLAUDE.md` at all, so `user` would leak the
owner's personal memory into every bot. Auth is unaffected. `--disable-slash-
commands` was added alongside it so no installed skill can steer a bot.

**14.2 Role tools are gone; `set_name` still rotates AFTER the turn.** A
rename cannot take effect mid-conversation (the system prompt is recorded
per conversation, 14.5), and rotating during the turn would lose the
conversation id the turn is about to write. The runner compares the name
before and after and clears `session_id` once the turn has been persisted.
`update_instructions` / `/role` are not registered.

**14.3 Free text never answers `ask_owner`.** §8 used to treat any DM text
while a session was `waiting` as the answer. That stole the next owner
message (always meant for General) and, when several similar questions were
pending, answered only one — then the 10-minute idle nag made General
re-ask the rest. Now: a button tap or a reply-to that question is the
answer; free text lists the pending asks (no timeout) and goes to General.
Tapping one fans the same answer out to similar pending questions
(normalized text match; the label must exist on the sibling, except Omitir
which skips siblings even without that button). Workers stay parked;
General keeps taking DM turns. Idle-remind skips `waiting`.

**14.4 `linkSharedProjects` only creates symlinks.** §4 was silent about an
existing `projects/`. Replacing one would destroy real transcript history, so
ccc creates the link when the path is free and otherwise leaves it exactly as
it is — including the reverse link for the implicit `~/.claude` profile.

**14.5 The system prompt is snapshotted per conversation, and
`--system-prompt-snapshot off` does not change that.** Verified: a resumed
session whose launch passed a DIFFERENT `--system-prompt` still answered with
the original prompt's secret word, with the flag explicitly set to `off`. This
is what makes §9's envelope load-bearing and what makes `/name` rotate the
conversation.

**14.6 `WorkingAgents` comes from `turns.status = running`.** §4 inherited the
v2 idea of counting "working background agents" from the fleet view. v3 has no
fleet; one running turn is one `claude -p` process, so the turn table is both
cheaper and exactly right.

**14.7 Access is a config whitelist, not a SQLite table.** Pairing codes,
pending rows, Allow/Block buttons and `/access pair|add|remove|block` are gone.
Extra allowed Telegram user ids live in `config.json` (`allowed_user_ids`),
loaded at process start next to `chat_id`. Unknown DMs are dropped in silence.
Restart listen to change the list. A leftover `access` table is dropped on open.

**14.8 The PTY login flow suppresses the browser.** Not in the plan, and found
the hard way: `claude auth login` shells out to `open`/`xdg-open` (and honours
`$BROWSER`), so the first string-capture probe opened a real browser tab on the
Mac pointing at a `localhost` callback nothing was listening on. Every PTY flow
now runs with a directory of no-op shims first on `PATH` and `$BROWSER` pointed
at one of them. The login URL belongs in Telegram and nowhere else — the VM is
headless, and hijacking the owner's browser on the Mac is worse than useless.

**14.9 The disclaimer strings were read from the binary, not captured.** The
login prompts were captured verbatim from a throwaway config dir under
`/private/tmp` (process killed, directory deleted, no code entered, no login
completed, `~/.claude` and the Keychain untouched). The bypass-permissions
disclaimer only appears once a config dir is logged in, which that constraint
forbids, so its strings were extracted from the 2.1.270 binary instead — the
same technique that produced `bypassDisclaimerMsg`. Because neither probe pins
the option ORDER, the driver reads the numbered list off the screen and answers
with the number beside the label it wants, and success is always verified
against real state (`claude auth status --json`, `bypassAccepted`).

**14.10 Leftover inbox delivery still enqueues a turn.** The old
inter-session `send_to_bot` path wrote `inbox` rows and Phase 2a only
kicked the target's queue, which had nothing in it. Phase 2b creates the
queued turn, labelled with the sender. That machinery is still in the
database; the tool is not registered and sessions are not a crew.

**14.11 A watch's first run is a baseline.** §7 did not say what happens on the
very first run, when `last_hash` is empty. Treating that as a change would wake
the bot for "the watch exists", so the first run records the hash silently.
Changing a watch's command resets the baseline for the same reason.

**14.12 `/model` does not rotate conversations.** Unlike `/name`, `--model` is
passed on every turn including resumes, so a model change takes effect
immediately and there is nothing to rotate.

**14.13 Headless bootstrap.** §8 assumed `ccc setup`'s interactive Telegram
loop. A VM has no terminal to run it in, so `ccc config set <key> <value>` sets
every bootstrap key non-interactively. `ccc install` writes a systemd **user** unit whose only `Environment=`
lines are the `env_passthrough` names that are actually set.

**14.14 Renaming rotates the conversation.** The session name is in the
system prompt (`You are a coding assistant in a Telegram session named
<name>`, §9), and the system prompt is recorded per conversation (14.5), so
a rename would otherwise leave the model answering under the old title
until the next compaction. `/name` and `set_name`
therefore all clear `session_id` — memories are kept — and the confirmation
message says so. A `set_name` call made DURING a turn is rotated by the
same post-turn check (14.2), which also repairs the id a fresh conversation
wrote back after the tool cleared it. Other open sessions are unaffected:
there is no roster in their prompts.

**14.15 Session names are unique labels, not Telegram titles.** The name is
unique so it cannot collide. Workers rename via `set_name`; `/name` in the DM only hits General, which refuses. There is no
Telegram forum topic to keep in sync.

**14.16 Topic icons are gone.** They were a status indicator on the old
forum topic (and a large, order-unstable blob in the system prompt).
General's 60s cap and a single idle reminder per spell (after 10 minutes)
in General replace that.

**14.17 Sessions have no role onboarding.** A session is a backend worker.
General is the dispatcher (not a launcher). `/session <prompt>` (and
`spawn_session`) dispatch that prompt as the first worker turn. `/role` and
`update_instructions` are gone. Archiving a worker is `archive_bot` or the
phone.

**14.18 Inputs are debounced before a turn starts.** §2 only said further inputs
"queue and are delivered together on the next turn", which handles a burst that
arrives WHILE a turn runs but not the normal case: an idle bot, and an owner who
types a sentence, then the correction, then the link. Each of those was its own
`claude -p` run, which is the most wasteful thing ccc can do with a token
budget. So `runNext` now waits for the queue to be quiet for `debounce_ms`
(instance setting, default 2500) before folding it. Three guards keep a bot from
being parked: only a queue whose newest input has `source=user` waits (a watch,
a background job or another bot is delivering one thing, not typing); inputs that queued during
the previous turn are already older than the window, so the wait is zero; and
the total wait is capped at four windows. `ccc config set debounce_ms 0` turns
it off.

`debounce_ms`, `compaction_model`, `maintenance_hour`, `idle_compact_s`,
`watch_ttl_s` and `worker_turn_timeout_s` are **config.json keys, not a Telegram command**. An earlier
draft added `/set` for them; it was removed because a knob nobody remembers
is worse than a default that is right. `ccc config` prints each one with the
default in force; `ccc config set` validates the range.

**14.19 `/usage` reads `turns.usage_json`, and the cost is folded into it.** The
`result` event reports `total_cost_usd` NEXT TO `usage`, not inside it, so the
runner merges it in under `cost_usd` before storing: one column per turn, and
old rows (which carry `total_cost_usd` or nothing) still parse. Only turns with
a `started_at` are counted — an input that `foldQueue` merged into another turn
is bookkeeping, not a run, and counting it would make "turns" disagree with what
was spent.

**14.20 The system prompt is byte-stable on purpose.** 14.5 established that
Claude Code snapshots the system prompt per conversation. The prompt cache adds
a second, sharper reason to keep it identical from turn to turn: it keys on a
prefix of the request, so one differing byte re-charges the whole conversation
as fresh input. The audit found two things that could move — a leftover
roster that carried each session's live status, and the icon list that came
back from Telegram in whatever order it liked. Both are gone. The date was
already in the envelope, and stays there. §9.1 has the rule.

**14.21 Inter-session messaging is General ↔ worker only.** The old
mesh `send_to_bot` is not registered. General `tell_session`s a worker;
a worker `report_to_general`. Inbox delivery is still the kick path
(the MCP child cannot call `Runner.kick`; the sender's turn ends, then
`deliverInbox`).

**14.22 The service reads its secrets from a 0600 file, not from the unit.**
`ccc install` used to bake `Environment="NAME=value"` lines for every
`env_passthrough` name set in the shell that ran it. That was wrong twice: it
wrote live tokens into a 0644 unit file under `~/.config/systemd` (readable by
anything that can run `systemctl --user cat ccc`), and it captured only what
that shell happened to export — `systemctl --user` never sources `~/.profile` or
`~/.zshrc`, so the obvious "I exported it in my rc file" produced a service with
no secrets at all and no warning. Now `ccc install`, `ccc env sync` and
`ccc config set env_passthrough …` write `<config_dir>/env` (0600, atomic
replace) with one `NAME="value"` line per name found in the CURRENT process
environment, and the unit carries only `EnvironmentFile=-%h/.config/ccc/env`.
Each of those commands prints which names were found and which were missing —
**names only, never values** — and points at `bash -lc 'ccc env sync'` when
something is missing. `ccc listen` also reads the file itself, filling only the
variables that are not already set, which is what makes the same mechanism work
under launchd on macOS (a plist cannot source a file). `/status` shows the same
present/missing names. A value containing a newline cannot be written as one
line, so it is reported as skipped rather than mangled.

**14.23 The disclaimer is written, not answered.** §8 step 3 originally drove
Claude Code's bypass-permissions warning through the PTY, the same way as the
login. In production that lost a real login: the driver got as far as the
first-run theme picker, `settings.json` ended up as `{"theme":"dark"}`, and the
account came back "the disclaimer was answered but settings.json still does not
record it" — logged in but unusable. The acceptance is nothing more than
`skipDangerousModePermissionPrompt: true` in the profile's `settings.json`
(verified on 2.1.270: writing that key by hand makes `claude -p
--permission-mode bypassPermissions` run under the profile and `ccc doctor`
report it accepted), so ccc writes it itself — idempotent, no second claude
process, no menu to answer. It runs at the end of `/account add` and
`/account login`, and is exposed as `ccc profile accept-disclaimer <email>` and
`ccc doctor --fix`. The PTY driver and its fixtures are gone; the login flow,
which genuinely needs a terminal, is unchanged. 14.9 is now history: only the
login strings are still matched against the TUI.

**14.24 `spawn_bot` was removed; 14.30 put spawn back on General only.**
Workers still cannot create sessions or message each other. General
(`topic_id=0`) is the dispatcher: `spawn_session` / `tell_session` /
`list_sessions`, roster in the envelope (not the system prompt — 14.20).
Workers `report_to_general`. `/session <prompt>` is the owner escape hatch.
Long parallel work still uses `run_background` in the same topic.
`archive_bot` stays so a worker can retire itself; General cannot be
archived or renamed, and is skipped by idle rotation.

**14.25 Progress is silent; the final answer notifies.** Telegram does not
send a notification for `editMessageText`, so editing the "⏳ working"
message into the reply would never ping. The progress message is posted with
`disable_notification=true`; when the turn ends it is deleted and the final
text is a new `sendMessage` without that flag — the one notification the
owner gets.

**14.26 Listen restart is visible, and mid-flight turns are retried.** A
LaunchAgent / systemd restart used to be silent: running turns were marked
`failed` / `ccc restarted` in SQLite and leftover background jobs were
reattached, but Telegram never heard and the work was dropped.
`recoverAfterRestart` now posts 🔁 in General. Each `turns.status=running`
row is requeued in place (same id, so it stays older than anything that
arrived while it ran) and pinged ▶️ in its bot's topic; historical
`failed` rows are not touched. A bot whose process is gone cannot resume
in-process — the retry is a new `claude -p` / `grok` / `agy` spawn of the
same input on the same session UUID. Live background jobs ping ▶️;
a job that fails (except an explicit cancel) pings ❌ immediately, because
the `source=background` wake is another turn and would be equally silent
if listen died again.

**14.27 Model is per engine, and per bot.** A single instance `model` field
was a Claude leftover: passing `sonnet` to grok/agy/codex is a turn failure.
`config.models` is a map of engine → slug; the legacy `model` field is still
the Claude default. `/model <slug>` in a bot topic writes `bots.model` (an
override). `/model <engine> <slug>` sets the instance default. The account
does not carry a model — engine is the account's job. `/model` with no args
lists **each account** with that engine's stored slug (not a collapsed
`claude=default · grok=…` line that hides which profile uses what), and the
buttons open a per-account picker that writes the same per-engine map.

**14.28 Codex is a fourth engine.** Same isolation pattern as Grok
(`CODEX_HOME` under `<data_dir>/accounts/codex/<id>`), login via
`codex login --device-auth`. That login is RFC 8628: the CLI prints a
URL (`https://auth.openai.com/codex/device`) and a one-time user_code
(`XXXX-XXXXX`); the owner types the code **on the page**, not back into
Telegram. `/cancel` (including `/cancel@bot`) aborts the wait. Turns via
`codex exec --json` with
`--sandbox danger-full-access` and `--dangerously-bypass-approvals-and-sandbox`.
The CLI mints a `thread_id` (like agy's `conversation_id`); later turns
`codex exec resume <id>`. No ccc MCP. Verified against Codex CLI 0.133.0.

**14.29 Idle sessions rotate, and watches expire.** A day of CCC-only work on
the work VM showed the expensive pattern: a bot's conversation grows to
hundreds of thousands of tokens, Claude's prompt cache is 1h ephemeral, and
the next turn after that (a user ping or a watch fire) rewrites the prefix
as `cache_creation`. So an idle bot whose last turn ended more than
`idle_compact_s` ago (default 1h) has `session_id` cleared — `/new`, no
model call. Waiting bots (unanswered `ask_owner`) are skipped so the
answer still has the question. Watches are the other leak: they are
change-detectors for a finite job (a PR's CI, a deploy), not standing
monitors. After `watch_ttl_s` (default 4h) the watch is deleted and the
bot that set it is woken (`source=system`) to re-set it, except General
(silent delete — a dispatcher turn to re-set a watch is a no-op wakeup).
Re-upserting the same name restarts the clock. Routines do not expire. Both
knobs are config.json keys; 0 disables. General is idle-rotated on the same
horizon so the dispatcher transcript cannot grow without bound.

**14.30 Grok and Codex get ccc MCP.** Claude already had `--mcp-config`.
Grok/Codex have no inline equivalent; ccc writes `[mcp_servers.ccc]`
(`command` + `args = ["mcp"]`) into the isolated account `config.toml`.
Identity is `CCC_BOT_ID` / `CCC_TURN_ID` in the engine process env
(Codex also gets per-turn `-c` with `--bot`/`--turn` so concurrent
sessions on one CODEX_HOME cannot clobber each other). Antigravity is
still omitted: attaching MCP would mean writing a shared `~/.gemini`
file. Implicit (non-isolated) Grok/Codex homes are not patched, so a
user's interactive CLI does not pick up a broken `ccc mcp`.

**14.31 The owner's DM is General; sessions have no Telegram topic.** The
forum-group model (one topic per session) was the original v3 UX and is
gone. The owner talks only to General in the bot's 1:1 DM. Group messages
are ignored. `spawn_session` and `/session` create a backend worker
(`TopicID = -id`). Worker progress and final answers stay off Telegram;
the owner does not see General↔session prompts or reports. A live status
card in the DM (pinned while any worker is running, waiting, or on a
background job) is edited in place; `/sessions` remains the full list.
Full reports stay in General's inbox.
`notify_owner`, `ask_owner` and job pings still reach the owner.
Idle-session reminders stay in General's inbox (they are not a Telegram ping).
There is no `group_id`, `/setgroup`, or forum topic API. `isGeneralBot` stays
`TopicID == 0`. The `topic_id` column is the session key, not a Telegram
forum id; leftover positive ids from old installs identify those rows and
have no Telegram destination.

**14.32 Owner vault + blind inject.** Takan is gone; the vault is CCC-owned.
Values sit in `<config_dir>/secrets` (0600 JSON), the same class of store as
`<config_dir>/env` — not a KMS. `/secret add <name>` does not take the value
on the command line: listen prompts, and the owner's next DM is captured
before any bot sees it, never logged, never stored in the topic/inbox, then
deleted via `deleteMessage` when the Bot API allows. `/secret show` is
refused (for the owner too). MCP exposes `secrets_list` (names),
`secrets_delete`, and `run` / `run_background` with an `env` map
(env-var-name → secret name) plus optional `stdin_secret`. The child gets
the value on env/stdin; argv never carries it; captured stdout/stderr is
redacted of known values before the model sees it.

Honest limit: a hostile `ps eww` or `set -x` on the child, or a Bash `cat`
of the 0600 file (sessions already run as the owner), can still leak. The
happy path does not. DESIGN examples never include a value.

**14.33 Usage waste is a product bug, not an install tweak.** Two days of
work-CCC traffic showed the cost is rereading context, not writing: ~190
cache-read tokens per output token, and a 7-day window only at 32%. The
leaks were (1) `schedule_wakeup` used as a poll — each no-change fire is a
full turn; a watch is free until the output changes; (2) named routines
running as a turn of General, so a 381-token digest paid for the
dispatcher's whole transcript; (3) `pickAccount` ignoring 7-day utilization
and running-turn load (the second Claude account sat almost idle); (4) the
60s auto-spawn treating `source=bot` inbox reports as the owner's request
and starting duplicate workers. Fixes live in this repo: prompt + MCP copy,
routine fires run on a reused `routine-<name>` worker (fresh conversation),
chooseProfile is 5h then 7d then load, spawn picks the engine with most
headroom, auto-spawn is `source=user` only, watch TTL on General is silent.
Not per-machine hygiene.

## 15. Public hub (removed)

The phone companion (`ccc-app`), pairing URI (`ccc pair` / `ccc unpair`),
and public websocket hub (`ccc hub`, default `wss://hub.getccc.dev`) are
gone. Hub-on-by-default was a trust surface: a `hub_devices` row meant full
RPC, and `send` to General is a shell with bypass permissions. Access is the
config whitelist (`chat_id` + `allowed_user_ids`). Leftover
`hub_devices` / `hub_pair_codes` / `hub_files` / `access` tables are dropped
on open. File relay (`ccc relay`) for Telegram uploads over 50 MB stays.

## 16. Owner secrets vault

Instance-wide, owner-managed. Not per-session.

**Telegram (owner DM only)**

- `/secret add <name>` — validate the name, prompt, capture the next owner
  message (the value). `/cancel` aborts. Another `/…` command aborts the
  capture and then runs. Empty value is rejected and the capture stays open.
  On success: store, delete the Telegram message if allowed, reply
  `saved <name>` (name only).
- `/secret list` (also bare `/secret`) — names only.
- `/secret delete <name>` — aliases `rm` / `remove`.
- `/secret show` / `get` / `cat` / `print` — refused. Values are never shown.

**At rest.** `<config_dir>/secrets`, mode 0600, JSON object, atomic replace
under an flock. Names: letter, then letters/digits/._- (max 64). Values up
to 64 KiB (newlines allowed; PEM-style keys fit). Not in `ccc.db`.

**Blind inject.** The model lists names (`secrets_list`) and runs a command
with `env: { "GH_TOKEN": "github-token" }` or `stdin_secret: "name"`.
`ccc mcp` / listen resolve the names at spawn, set env/stdin on the child,
and redact known values (length ≥ 4, longest first, token `***`) from the
tool result, from `background_jobs.output`, and from the `source=background`
wake. Env var names that start `CLAUDE` / `ANTHROPIC` are rejected.

**What the model must not have:** `secrets_get`, a tool that returns a
value, a placeholder expansion that writes the value into argv.

Honest limit: `ps eww` / `set -x` / same-user `cat` of the file can leak;
the happy path does not. DESIGN examples never include a value.
