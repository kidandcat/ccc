# ccc v3 — design

Status: implemented (Phases 2a and 2b, 2026-09-14). This document is the
specification the implementation follows. When code and this document disagree,
fix one of them in the same change. Everything below describes what ccc v3
actually does; §14 lists where the built thing knowingly departs from the
original plan, and why.

## 1. What ccc v3 is

ccc is a **team of generic Claude bots living in one Telegram forum group**,
driven by `claude -p` as a stateless runner, with ccc owning everything the
runner does not: bot identity, persistent memory, inter-bot messaging,
scheduling, account (profile) management, access control, and the Telegram UX.

The experience target is Jairo's `grok-bot`: you talk to a topic like you talk
to a person. No commands in the normal flow, no ceremony, no terminal.

Non-goals (explicitly dropped from v2): Claude Code background agents, the
agents view, `claude attach` handoff, transcript scraping, the AskUserQuestion
PreToolUse hook hack. One ccc instance never talks to more than one Telegram
bot.

## 2. Runtime model

| Concept | Definition |
|---|---|
| **Instance** | One `ccc listen` process on one machine, bound to one Telegram bot token and one forum group. Instance-level config: model, env passthrough, default profile, data dir. |
| **Profile** | One Claude account = one `CLAUDE_CONFIG_DIR` (see `profiles.go`). Two profiles per instance is the normal case. Profiles are interchangeable at turn granularity (§4). |
| **Bot** | One forum topic. Identity = `name` + `role` (free text set with `/role`) + its own memory scope + an **engine** (`claude` default, or `grok` / `antigravity`). Claude bots share MCP tools, the system prompt template, the instance model and env. Grok/Antigravity bots spawn that CLI instead and do not get ccc MCP. Optional per-bot `cwd` (default: `<data_dir>/bots/<name>/workspace`). |
| **Session** | The Claude Code conversation behind a bot: a UUID ccc mints and resumes. A bot has exactly one live session; `/new` rotates it. |
| **Turn** | One `claude -p` process: input = one user/bot/system message (plus context envelope), output = streamed events until `result`. At most one turn per bot at a time; further inputs queue (FIFO) and are delivered together on the next turn. |

## 3. Turn lifecycle

```
input (Telegram text | inbox message | schedule | watch diff)
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

### 3.2 Progress in the topic

One progress message per turn, edited in place (rate-limited to ~1 edit / 3 s):
current tool activity summarized ("editing poller.go", "running go test",
"reading PR #1234"), elapsed time. When the turn ends the progress message is
replaced by the final assistant text (chunked at 4096, Telegram HTML), and a
✅ reaction is added to the user's triggering message. Tool call payloads are
never dumped into the topic; `thinking` is never shown.

### 3.3 Post-turn

- Persist `turns` row: profile used, duration, cost/usage from `result`,
  `stop_reason`, session id.
- Deliver any `send_to_bot` messages produced during the turn (they were
  written to `inbox` synchronously by the MCP tool; delivery = enqueue a turn
  on the target bot if `wake=true`, labelled with the sender). `wake=false`
  rows are not delivered as turns: they are summarized in the next envelope.
- If the turn ended with `ask_owner` pending, the bot is marked `waiting` and
  no queued inputs are delivered until the answer arrives (answers are inputs).
- If `update_instructions` changed the role during the turn, rotate the session
  now that the turn has recorded its id (§14.2).

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

A retried turn reuses the same session UUID: because profiles share
`projects/` (§4) the other account can resume the same conversation.

## 4. Profiles: selection and shared sessions

- `pickProfile()` chooses per **turn**: lowest cached 5-hour utilization,
  tie-break fewer turns currently running on that account (read from
  `turns.status = running`, §14.6), then name; excludes profiles in cooldown or
  `needs_login`.
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
bots        id, name (unique), topic_id, role (text), cwd, session_id, engine (claude|grok|antigravity, default claude),
            status (idle|running|waiting|disabled), created_at, archived_at, parent_bot_id (for spawned workers)
turns       id, bot_id, session_id, profile, source (user|bot|schedule|watch|system), input (text),
            output (text), status (queued|running|done|failed), stop_reason, error_class,
            started_at, ended_at, usage_json
inbox       id, to_bot_id, from_bot_id (nullable = owner/system), text, wake (bool), delivered_at, turn_id
memories    id, scope (user|project|bot), scope_key (''|project path|bot id), key, text,
            created_by_bot_id, created_at, updated_at        -- unique(scope, scope_key, key)
memories_archive  id, compaction_id, archived_at, scope, scope_key, key, text, created_by_bot_id,
            memory_created_at, memory_updated_at            -- what a compaction replaced (§7)
projects    id, path (unique), name, description, stack, deploy_notes, updated_at
watches     id, bot_id, name, command, interval_s, last_hash, last_output, last_run_at, enabled
schedules   id, bot_id, fire_at, note, recurring_cron (nullable), fired_at
questions   id, bot_id, turn_id, question, options_json, answer, asked_message_id, answered_at
access      telegram_user_id (pk), display, state (pending|approved|blocked), pair_code, code_expires_at,
            replies (how many times ccc has answered this stranger, §14.7)
settings    key (pk), value                                  -- instance settings edited from Telegram
```

Indexes beyond the ones the columns above imply: `turns(bot_id, created_at)`
for turn retention, `inbox(delivered_at)` and `questions(answered_at)` for
cleanup, `memories_archive(compaction_id)` for restore.

Settings actually used: `last_maintenance` (ccc's own marker) and `topic_icons`
(the cached sticker set, §14.16). The table is bookkeeping only — nothing in it
is user-editable.

Existing `config.json` (bot token, group id, profiles) stays as bootstrap
config, and it is also where the three tuning knobs live: `debounce_ms`
(default 2500, §14.18), `compaction_model` (default `haiku`, §7) and
`maintenance_hour` (default 4). They are set with `ccc config set <key>
<value>`, printed by `ccc config`, and have no Telegram command (§14.18).
Everything else runtime lives in SQLite. The v2 `sessions` map and the
JSONL ledger are not migrated (v3 is a fresh start; document it).

## 6. MCP server (`ccc mcp`)

Stdio JSON-RPC MCP server, one process per turn, spawned by Claude Code from
the inline `--mcp-config`. It opens the same SQLite file and knows the calling
bot/turn from its flags. Tools (all bots get all of them):

| Tool | Input | Behavior |
|---|---|---|
| `remember` | `scope` (user\|project\|bot), `key`, `text`, `project_path?` | Upsert a memory. `bot` scope is implicitly this bot. |
| `recall` | `query`, `scope?`, `limit?` | Full-text (SQLite FTS5) search over memories visible to this bot: all `user`, all `project`, own `bot`. Returns key+text+scope. |
| `forget` | `scope`, `key`, `project_path?` | Delete one memory. |
| `list_bots` | — | Names, roles, status, topic links of all live bots. |
| `send_to_bot` | `bot`, `text`, `wake` (default true) | Append to `inbox`; mirrored into both topics as `🤝 <from> → <to>: …`. Delivery happens post-turn (§3.3). |
| `notify_owner` | `text`, `urgency` (normal\|urgent) | Post in this bot's topic mentioning the owner; `urgent` also DMs the owner. |
| `ask_owner` | `question`, `options?` (≤4 strings) | Post question with inline buttons (or free text if no options). Returns immediately with `{"status":"asked"}`; the bot should end its turn. The answer arrives as the next input (`source=user`, prefixed `Answer to "<question>": …`). |
| `update_instructions` | `role` | Replace this bot's `role`; echo the new text into the topic. `/role` does the same from Telegram. |
| `set_name` | `name`, `emoji?` | Rename this bot: validate (§8 `/name`), update `bots.name`, rename the forum topic and, when `emoji` is one Telegram allows, set the topic icon. Rotates the session (§14.14). `/name` does the same from Telegram. |
| `watch` | `name`, `command`, `interval_s` (≥60) | Register a deterministic watch (§7). `unwatch(name)`, `list_watches()`. |
| `schedule_wakeup` | `in_seconds` or `at` (RFC3339), `note`, `cron?` | Self-wakeup (§7). `cancel_schedule(id)`. |
| `spawn_bot` | `name`, `role`, `cwd?`, `first_message?`, `emoji?` | Create a child bot + topic (icon from `emoji`); `parent_bot_id` = this bot; the child reports back with `send_to_bot(parent)`. |
| `archive_bot` | `bot?` (default self) | Close the topic (Telegram close, not delete), mark archived. |
| `get_project` / `set_project` | `path`, fields | Read/update the project registry. |
| `send_file` | `path`, `caption?` | Send a file into this bot's topic (≤50 MB; larger → existing relay if kept). |

All tools validate the calling bot from the `--bot` flag; tool inputs coming
from the model are data, never instructions to ccc.

Topic icons are not free-form: `editForumTopic` only accepts a custom-emoji id
out of `getForumTopicIconStickers`. ccc caches that list (memory + `settings`,
refreshed daily, §14.16) and puts the allowed emoji into the `set_name` and
`spawn_bot` tool descriptions and the system prompt, so the model picks one that
exists. An emoji outside the set leaves the icon untouched and the tool result
says which emoji were available.

## 7. Scheduler and watch engine

One goroutine in `ccc listen`:
- **Watches**: every `interval_s` (floor: 60 s), run `command` in the bot's cwd
  with the instance env (no model involved). The FIRST run only records a
  baseline and wakes nobody; changing a watch's command resets that baseline.
  Hash stdout; if changed since `last_hash`,
  enqueue a turn on the bot with `source=watch` and an input containing the
  watch name, the previous and new output (diffed, truncated to ~8 KB). Zero
  tokens while nothing changes.
- **Schedules**: enqueue a turn with `source=schedule` and the note when
  `fire_at` passes; recurring via cron expression.
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
bot**: a fresh `--session-id` nobody resumes, `--setting-sources ''`, no MCP
server at all (so it cannot touch the database it is compacting), plain text in
and out (`claudePlainArgs`). The prompt is the scope as `key: text` lines,
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
`bot`-scope memories of bots archived more than 30 days ago (nothing can read
them — bot memories are visible to that bot alone); `memories_archive` rows
older than 90 days.

## 8. Telegram UX

### Conversation
- Plain text in a bot's topic → input for that bot. No `/new` needed.
- Plain text in the group root (General) → creates a new bot: topic named from
  the first line, empty role, first message dispatched. `/bot <name> [role]`
  does the same explicitly.
- Photos/documents → saved into the bot's workspace `inbox/`, path passed in the
  message. Voice → transcribed if the `voice` build is present, else the file
  path is passed (keep the existing whisper integration).
- `ask_owner` → inline buttons `q:<question_id>:<option_idx>`; tapping edits the
  message with ✓ and enqueues the answer. Free-text answers: replying to the
  question message counts as the answer, and so does ANY text sent while the bot
  is parked `waiting` (a reply-to takes priority when both apply, §14.3).
- Bot→bot traffic is visible in both topics (`🤝`).
- Renaming a topic in Telegram itself renames the bot: the `forum_topic_edited`
  service message is validated like `/name` and, when it passes, `bots.name`
  follows the title (§14.15).
- An **edited** message is gated like any other update. If its text starts with
  `/` it goes through the same command dispatcher — editing a mistyped command
  in place is how a phone corrects one — deduped by
  `(chat_id, message_id, edit_date)` so a redelivered edit runs once. An edited
  plain message still does nothing: re-running a turn because somebody fixed a
  typo is worse than ignoring it.

### Commands
| Command | Where | Effect |
|---|---|---|
| `/role [text]` | topic | Show or set the bot's role. |
| `/name [text] [emoji]` | topic | Show or set the bot's name: renames the forum topic, sets its icon and rotates the session (§14.14). |
| `/new` | topic | Rotate the session (fresh conversation, memory kept). |
| `/stop` | topic | Kill the running turn (SIGTERM the `claude` process), drop the queue. |
| `/cwd [path]` | topic | Show or set the bot's working dir. |
| `/engine [name]` | topic | Show or set the bot's engine (`claude`, `grok`/`grok-build`, `antigravity`/`agy`). Rotates the session. New bots take `default_engine` from config.json (default `claude`). |
| `/memory [query]` | topic | List/search memories visible to this bot; `/forget <scope> <key>`. |
| `/memory stats` | topic | Per scope: entries, bytes, whether it is over the compaction threshold, last compaction (§7.1). |
| `/memory restore <id>` | topic | Undo one compaction. Owner only. |
| `/usage` | anywhere | Tokens, cache hit ratio, turns, average duration and cost per bot, today and last 7 days (§14.19). |
| `/watches`, `/schedules` | topic | List and cancel. |
| `/bots` | anywhere | Table of bots, status, last activity. |
| `/account` | anywhere | Status card per account with buttons; subcommands `status`, `add <email>`, `login <email>`, `remove <email>`, `default <email>`. |
| `/model [name]` | anywhere | Show/set the instance model. |
| `/access` | anywhere | Pairing/allowlist management (below). Owner only. |
| `/watches`, `/schedules` | topic | List and cancel (also listed above). |
| `/setgroup` | group | Bind the instance to this forum group. Owner only, and the headless alternative to `ccc setgroup`. |
| `/status` | anywhere | Instance health: profiles, running turns, queue, doctor findings. |

### Account management from Telegram (login without a terminal)
An account is addressed by the **email** of the Claude account behind it, never
by a name the owner invents and never by the directory it lives in:

- The profile key in `config.json` is the address, lowercased. The config dir is
  derived from it (`jairo@agentero.com` → `<data_dir>/profiles/jairo_at_agentero.com`)
  and is an implementation detail: it is never shown in Telegram.
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
`/account login <email>` runs steps 2–3 only. Every PTY string and pattern lives
in `ptyflow.go`, each with a note on how it was verified against 2.1.270
(§14.9). Select lists are answered by reading the option number off the screen,
never by assuming a position.

### Access control (copied from the official Telegram channel plugin)
- Owner = the Telegram user id from bootstrap config; always allowed.
- Unknown DM → 6-hex pairing code (1 h TTL, ≤3 pending, ≤2 replies per stranger
  and then silence); owner approves with `/access pair <code>` (or a button in
  the owner's DM). Approved users may talk to bots in the group; only the owner
  can use `/account`, `/access`, `/model` and `/setgroup`.
- Every inbound update from a non-approved user is dropped silently after the
  pairing reply. Messages, EDITS and callback queries are gated the same way; an
  unknown user in the GROUP gets no reply at all, because answering there would
  let anyone who finds the group make the bot talk.
- Until `chat_id` is configured there is no owner, so nobody is allowed.

## 9. System prompt and context envelope

**System prompt** (`--system-prompt`, rendered once per session; a change of
role or template rotates the session):

```
You are <name>, a bot in Jairo's ccc team. Role: <role>.
You run on machine <hostname>, working dir <cwd>. Today is <date>.
Tools: you have the ccc MCP tools (memory, messaging, scheduling, watches,
spawning) plus the standard tools (Bash, Read, Edit, …) with full permissions.
Other bots: <name — role> list.
Rules: … (owner escalation, when to remember, never print secrets, keep
replies short for chat, prefer ask_owner over guessing on architecture…)
```

**Envelope** (prepended to every input, because system-prompt changes are
ignored on resume until compaction):

```
<context>
onboarding instruction (only while the bot has no role, §14.17)
recent user memories (top 10 by recency/relevance to the message)
project memories for cwd (if any)
own bot memories (top 10)
pending inbox summary (N messages from X)
</context>
<message source="user|bot:<name>|watch:<name>|schedule">…</message>
```

Keep the envelope under ~4 KB; `recall` exists for everything else.

### 9.1 Prompt-cache discipline

The API's prompt cache keys on a PREFIX of the request, and the system prompt is
the first thing in it. One byte that differs between two turns of the same
conversation invalidates the cache for the whole conversation, and the entire
history is re-charged as fresh input. So the split above is not only about
14.5's snapshot: it is what makes a resumed turn cheap.

Audited and enforced (§14.20): the system prompt holds only facts fixed for the
life of a session — name, role, hostname, cwd, the tool list — plus two lists
that are SORTED before rendering (the other-bots roster, by name; the topic-icon
emoji). A bot's live status was removed from the roster because it changes every
turn; the date was never in the prompt. Everything per-turn (date, memories,
inbox, onboarding) is in the envelope, which is the TAIL of the request: it
costs only itself and invalidates nothing.

`/usage` reports the cache hit ratio (`cache_read / (cache_read + input)`) per
bot, which is how a regression here is noticed: a resumed conversation that
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

- Bots run with bypass permissions on the owner's machine: **the chat is the
  trust boundary**. Access control (§8) is therefore mandatory, not optional.
- Secrets reach bots only via `env_passthrough`; ccc never posts env values,
  tokens, or credential file contents to Telegram; `send_file` refuses paths
  under config dirs and `<data_dir>/profiles`.
- Tool inputs and Telegram text are data. Pairing is never approved because a
  message asked for it.

## 13. Delivery plan

- **2a — runner core**: SQLite/GORM schema, `runner.go` (§3), profile failover,
  `ccc mcp` with `remember/recall/forget/list_bots/send_to_bot/notify_owner/
  ask_owner/update_instructions/send_file`, Telegram conversation flow (§8
  "Conversation" + `/role /new /stop /cwd /bots /memory /status`), envelope,
  system prompt, progress rendering. E2E: two bots talking to each other via
  `send_to_bot`, an `ask_owner` round trip, a failover forced by disabling a
  profile.
- **2b — automation & accounts** (done): watches, schedules,
  `spawn_bot/archive_bot`, project registry, doctor loop, `/account …` with PTY
  login and disclaimer, `/access` pairing, `/model`, `/setgroup`, headless
  bootstrap (`ccc config set`, systemd user unit, `make build-linux`), legacy
  removal (§11), README rewrite.

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

**14.2 `update_instructions` rotates the session AFTER the turn.** A role change
cannot take effect mid-conversation (the system prompt is recorded per
conversation, 14.5), and rotating during the turn would lose the session id the
turn is about to write. The runner compares the role before and after and
clears `session_id` once the turn has been persisted.

**14.3 Any text to a `waiting` bot counts as the answer.** §8 only specified
"replying to the question message". In practice people answer without using
reply-to, and the bot is parked either way. Reply-to still takes priority when
both could apply, so answering an older question explicitly still works.

**14.4 `linkSharedProjects` only creates symlinks.** §4 was silent about an
existing `projects/`. Replacing one would destroy real transcript history, so
ccc creates the link when the path is free and otherwise leaves it exactly as
it is — including the reverse link for the implicit `~/.claude` profile.

**14.5 The system prompt is snapshotted per conversation, and
`--system-prompt-snapshot off` does not change that.** Verified: a resumed
session whose launch passed a DIFFERENT `--system-prompt` still answered with
the original prompt's secret word, with the flag explicitly set to `off`. This
is what makes §9's envelope load-bearing and what makes `/role` rotate the
session.

**14.6 `WorkingAgents` comes from `turns.status = running`.** §4 inherited the
v2 idea of counting "working background agents" from the fleet view. v3 has no
fleet; one running turn is one `claude -p` process, so the turn table is both
cheaper and exactly right.

**14.7 `access` has a `replies` column.** §5's column list has nowhere to record
"at most two replies to a stranger, then silence", which §8 requires. One
integer per row.

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

**14.10 A waking inbox message becomes a turn.** §3.3 says delivery "= enqueue a
turn on the target bot"; Phase 2a only kicked the target's queue, which had
nothing in it, so `send_to_bot` never actually reached anybody. Phase 2b creates
the queued turn, labelled with the sender. This is what makes `spawn_bot` with a
`first_message` — and the child's report back — work.

**14.11 A watch's first run is a baseline.** §7 did not say what happens on the
very first run, when `last_hash` is empty. Treating that as a change would wake
the bot for "the watch exists", so the first run records the hash silently.
Changing a watch's command resets the baseline for the same reason.

**14.12 `/model` does not rotate sessions.** Unlike `/role`, `--model` is passed
on every turn including resumes, so a model change takes effect immediately and
there is nothing to rotate.

**14.13 Headless bootstrap.** §8 assumed `ccc setup`'s interactive Telegram
loop. A VM has no terminal to run it in, so `ccc config set <key> <value>` sets
every bootstrap key non-interactively and `/setgroup` binds the forum group from
Telegram. `ccc install` writes a systemd **user** unit whose only `Environment=`
lines are the `env_passthrough` names that are actually set.

**14.14 Renaming rotates the session, like `/role`.** The bot's name is in the
system prompt (`You are <name>, …`, §9), and the system prompt is recorded per
conversation (14.5), so a rename would otherwise leave the model answering to
its old name until the next compaction. `/name`, `set_name` and the topic-title
sync therefore all clear `session_id` — memories are kept, exactly like `/role`,
and the confirmation message says so. A `set_name` call made DURING a turn is
rotated by the same post-turn check as `update_instructions` (14.2), which also
repairs the id a fresh session wrote back after the tool cleared it. The OTHER
bots keep their sessions: their system prompt roster goes stale, which is what
`list_bots` is for, and `send_to_bot` resolves names against the live table.

**14.15 The topic title and `bots.name` are kept in sync in both directions.**
The name is unique and addressable (`send_to_bot`, `spawn_bot`, `list_bots`), so
it cannot be a free-text label; the topic title is the same string to the person
reading the chat. `/name` and `set_name` rename the topic; a rename made in
Telegram arrives as a `forum_topic_edited` service message and renames the bot.
A title that fails validation (taken, empty, too long) is NOT renamed back —
that would fight the person renaming it, and could loop — the old name is kept
and the topic is told why.

**14.16 Topic icons come from a cached sticker set.** Forum topics cannot take
an arbitrary emoji: `editForumTopic` wants an `icon_custom_emoji_id` from
`getForumTopicIconStickers`. The list is identical for every bot and changes
rarely, so ccc caches it in memory and in `settings` and refreshes it once a
day; when the fetch fails the stale list is used, because a missing icon is much
cheaper than a failed rename. Matching is exact first, then the same emoji
ignoring variation selectors and skin tones; no match leaves the icon alone and
reports the available emoji instead of guessing a different icon.

**14.17 A role-less bot is onboarded from the envelope, not the system prompt.**
A bot created from a line in General starts with an empty role, and a generic
assistant is not what the owner asked for. Every turn of a role-less bot carries
an instruction to introduce itself, ask what it is for, and then store the answer
with `update_instructions` and pick a name and icon with `set_name`. It lives in
the envelope because the system prompt is frozen per conversation (14.5): from
there it could not disappear the moment the role is set, which is exactly when
it has to.

**14.18 Inputs are debounced before a turn starts.** §2 only said further inputs
"queue and are delivered together on the next turn", which handles a burst that
arrives WHILE a turn runs but not the normal case: an idle bot, and an owner who
types a sentence, then the correction, then the link. Each of those was its own
`claude -p` run, which is the most wasteful thing ccc can do with a token
budget. So `runNext` now waits for the queue to be quiet for `debounce_ms`
(instance setting, default 2500) before folding it. Three guards keep a bot from
being parked: only a queue whose newest input has `source=user` waits (a watch
or another bot is delivering one thing, not typing); inputs that queued during
the previous turn are already older than the window, so the wait is zero; and
the total wait is capped at four windows. `ccc config set debounce_ms 0` turns
it off.

`debounce_ms`, `compaction_model` and `maintenance_hour` are **config.json keys,
not a Telegram command**. An earlier draft added `/set` for them; it was removed
because a knob nobody remembers is worse than a default that is right, and three
keys did not justify a command, a whitelist and an owner-only branch. `ccc config`
prints each one with the default in force; `ccc config set` validates the range.

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
as fresh input. The audit found two things that could move — the roster carried
each bot's live status, and the icon list came back from Telegram in whatever
order it liked. The status is gone (`list_bots` is where live status belongs)
and both lists are sorted before rendering. The date was already in the
envelope, and stays there. §9.1 has the rule.

**14.21 `send_to_bot(wake=true)` is now taught as the expensive option.** Every
waking message starts a turn on the recipient (14.10), so the system prompt
tells bots to use `wake=false` for anything the other bot only needs to know and
`wake=true` only when it must act now, and to say everything they have in ONE
message rather than several.

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
