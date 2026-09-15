# ccc

**Crew Command Center** — a team of Claude, Grok or Antigravity bots living in
one Telegram forum group. Each topic is a bot with its own role, memory and
workspace; you talk to it like you talk to a person.

[![Go Version](https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat&logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

---

## What ccc is

Crew Command Center: a **topic is a bot**, not a session. Each bot has a name, a
free-text role you set with `/role`, its own working directory, its own
conversation and a memory shared with the rest of the team. You send it a
message; it does the work and answers in the topic. There are no commands in
the normal flow.

Under the hood ccc drives a coding CLI as a **stateless runner**. The default
engine is Claude Code: every message is one `claude -p` process with a session
id ccc mints and resumes. A bot can also run on **Grok Build** (`grok`) or
**Antigravity** (`agy`) — same topic, same envelope, that CLI's native
print/resume flags. ccc owns everything the runner does not — bot identity,
memory, inter-bot messaging, scheduling, account management, access control and
the Telegram UX.

```
┌────────────┐   message    ┌──────────┐   claude -p --resume  ┌──────────────┐
│  Telegram  │─────────────▶│   ccc    │──────────────────────▶│   one turn   │
│   topic    │◀─────────────│  listen  │◀──  stream-json   ────│              │
└────────────┘   progress   └──────────┘                       └───────┬──────┘
                                  ▲                                    │
                                  │        ccc mcp (stdio)             │
                                  └────────────────────────────────────┘
              remember · recall · ask_owner · send_to_bot · watch ·
              schedule_wakeup · run_background · set_name · get_project · send_file
```

### Concepts

| | |
|---|---|
| **Instance** | One `ccc listen` process on one machine, bound to one Telegram bot token and one forum group. |
| **Bot** | One forum topic. A name, a role, a working directory and a memory scope. |
| **Turn** | One engine process (`claude -p`, `grok --single`, or `agy --print`): one input, one answer. One turn per bot at a time; messages that arrive meanwhile are folded into the next turn. |
| **Engine** | Which CLI an **account** runs: `claude`, `grok` / `grok-build`, or `antigravity` / `agy`. Set when you add the account (`/account add <identity> <engine>`). A bot's turns pick a healthy account from that engine's pool. `/engine` is a secondary way to assign a bot onto another pool. |
| **Account** | One login for one engine. Claude = `CLAUDE_CONFIG_DIR`. Grok = isolated `GROK_HOME`. Antigravity = isolated `HOME` (`~/.gemini`). One ccc process can hold several Claude emails + several Grok logins + several agy logins at once. Failover stays inside the same engine. |
| **Memory** | Durable facts in three scopes: `user` (about you, shared by all bots), `project` (about one code base) and `bot` (private). |
| **Watch** | A command re-run on an interval. The bot is woken **only when the output changes**, with a diff. Nothing changing costs nothing. |
| **Schedule** | A wakeup at a time, or on a cron expression. |
| **Background job** | A long shell command in the same topic. The bot stays responsive; it is woken when the job finishes. |

### What a bot sees

- A system prompt with its name, role, machine, working directory and the other
  bots on the team.
- Per message, a small `<context>` envelope: today's date, the most relevant
  memories and a summary of anything waiting in its inbox.
- **No `CLAUDE.md`, no user/project settings, no skills.** Bots run with
  `--setting-sources ''`, so nothing you have installed for yourself leaks into
  them. They run with bypassed permissions in their own working directory.

---

## Bootstrap on a VM, step by step

This is the headless path: no terminal is ever attached to Telegram, and no
browser is ever needed on the server.

### 1. Build and copy the binary

On your machine:

```bash
git clone https://github.com/kidandcat/ccc && cd ccc
make build-linux                       # pure Go, CGO_ENABLED=0, no libc dependency
scp ccc-linux-amd64 vps:~/bin/ccc
```

### 2. Install Claude Code on the VM

```bash
ssh vps
curl -fsSL https://claude.com/install.sh | bash    # or your usual install method
claude --version
```

### 3. Create the Telegram bot and the group

On your phone:

1. Talk to [@BotFather](https://t.me/BotFather) → `/newbot` → copy the token.
2. Create a **group**, open its settings and enable **Topics**.
3. Add your bot to the group and make it an **admin** (it must be able to
   create and close topics).
4. Get your own numeric Telegram user id from [@userinfobot](https://t.me/userinfobot).

### 4. Configure ccc (no interaction needed)

```bash
ccc config set bot_token 123456:ABC-YourTokenHere
ccc config set chat_id   11111111          # YOUR telegram user id — this is the owner
ccc config set data_dir  ~/.local/share/ccc
ccc config set model     sonnet            # optional; omit for claude's default
ccc config set env_passthrough GH_TOKEN,LINEAR_API_KEY   # optional secrets for bots
ccc config                                  # check it
```

`chat_id` is the access-control root: only that user id can administer the
instance, and until it is set **nobody** can talk to ccc at all.

**`env_passthrough` and secrets.** Those names are the only channel by which a
secret reaches a bot. The service is started by `systemctl --user`, which never
sources `~/.profile` or `~/.zshrc`, so ccc snapshots the VALUES into
`~/.config/ccc/env` (mode 0600) and the unit reads that file — the unit itself
holds no secrets. `ccc config set env_passthrough …` writes it, and so does
`ccc install`. **Run them from a login shell**, or the values will not be
visible:

```bash
bash -lc 'ccc env sync'      # re-snapshot after exporting a new secret
```

Each of those commands prints which names it found and which are missing — names
only, never values — and `/status` shows the same thing from Telegram. Change a
token? Export it and run `ccc env sync` again, then restart the service.

### 5. Install the service

```bash
bash -lc 'ccc install'            # login shell: it snapshots env_passthrough into ~/.config/ccc/env
                                  # writes ~/.config/systemd/user/ccc.service and starts it
loginctl enable-linger $USER      # so it keeps running after you log out
systemctl --user status ccc
journalctl --user -u ccc -f
```

> **`systemctl --user` over ssh:** a plain non-login ssh session often has no
> `XDG_RUNTIME_DIR`, and every `systemctl --user` call then fails with
> *"Failed to connect to bus"*. Fix it per command or in your shell profile:
>
> ```bash
> export XDG_RUNTIME_DIR=/run/user/$(id -u)
> ```
>
> With `enable-linger` on, that directory exists even when you are not logged
> in. If it does not, `loginctl enable-linger $USER` and log in again.

### 6. Bind the group and log an account in — from your phone

In the forum group, send:

```
/setgroup
```

ccc records the group id. Then, anywhere:

```
/account add you@example.com claude
```

(`/account add you@example.com` still works: omitted engine is Claude.)

An account is its identity plus its engine. The same email can be registered
on more than one engine (a Claude profile and a Codex profile may share
`you@example.com`). Claude identities are emails; Grok and Antigravity
accept a short name or email. ccc derives an isolated home
behind it (you never see or type that path). For Claude it runs
`claude auth login` on a pseudo-terminal and posts the login URL. Open it on a
device where you are signed in to the right account, and send the code it gives
you back into the same chat. ccc feeds it to the CLI, confirms with
`claude auth status`, then accepts the bypass-permissions disclaimer for you.

Grok and Antigravity use the same `/account` flow with their own CLI login
(`grok login --device-auth`, `agy auth login`) and isolated homes.

If the device turned out to be signed in as somebody else, ccc says so and
stores the account under the address it actually logged in as. The account
already on the machine (`~/.claude`) needs no `add`: it shows up under its own
email as soon as ccc has asked `claude auth status` once.

> ccc **never opens a browser** on the machine it runs on: the login URL is for
> Telegram only. The login runs with a directory of no-op `open` / `xdg-open`
> shims first on `PATH` and `$BROWSER` pointed at one of them.

Add more accounts the same way. Mix engines in one instance:

```
/account add other@example.com claude
/account add work grok
/account add lab agy
```

Turns pick a healthy account whose engine matches the bot. Failover stays
Claude↔Claude or Grok↔Grok — ccc does not jump Claude→Grok mid-conversation.

### 7. Make your first bot

Send a message in the group's **General** topic:

```
keep an eye on the fecha deploy and tell me if anything breaks
```

ccc creates a topic named after the first line, with a bot behind it, and
dispatches your message. From then on, talk in that topic.

A brand new bot has no role yet, so it introduces itself and asks what it should
be responsible for. Answer in the topic and it stores the answer as its role and
picks a fitting name and topic icon for itself — or set them yourself with
`/role` and `/name`.

---

## Using it

### Conversation

| Where | What happens |
|---|---|
| Text in **General** | Creates a new bot named after the first line, and sends it your message. |
| Text in a **bot's topic** | An input for that bot. |
| A photo or document | Saved into the bot's `inbox/`, with the path passed in the message. |
| A voice note | Transcribed if the `voice` build is installed, else the file path is passed. |
| A reply to a question | Answers it. Any text while a bot is waiting counts as the answer too. |

While a turn runs, one progress message in the topic is edited in place with
what the bot is doing (no Telegram notification). The answer is posted when
the turn finishes — that is the ping you get — and your message gets a ✅.

### Commands

**In a bot's topic**

| Command | Effect |
|---|---|
| `/role [text]` | Show or set the bot's role. Setting it starts a fresh conversation. |
| `/name [name] [emoji]` | Show or set the bot's name. It renames the topic, sets the topic icon and starts a fresh conversation (the name is in the system prompt). Names are unique; renaming the topic in Telegram renames the bot too. |
| `/new` | Fresh conversation. Memories are kept. |
| `/stop` | Kill the running turn and drop the queue. |
| `/cwd [path]` | Show or set the bot's working directory. |
| `/engine [name]` | Show this bot's engine pool, or assign it to another (`claude`, `grok`, `antigravity`). Secondary: engine is set when you add the account. Switching pools starts a fresh conversation. |
| `/memory [query]` | List or search the memories this bot can see. |
| `/memory stats` | Per scope: how many entries, how many bytes, whether it is due for compaction, and when it was last compacted. |
| `/memory restore <id>` | Undo one memory compaction (owner only). The id is in the compaction message and in `/memory stats`. |
| `/forget <scope> <key>` | Delete one memory. |
| `/watches` / `/watches cancel <name>` | List or cancel its watches. |
| `/schedules` / `/schedules cancel <id>` | List or cancel its wakeups. |

**Anywhere**

| Command | Effect |
|---|---|
| `/bots` | Every bot, its status and when it last ran. |
| `/status` | Queue, running turns, accounts, watches, schedules, passthrough secrets (names only) and doctor findings. |
| `/usage` | Tokens in/out, cache hit ratio, turns, average duration and cost — per bot and in total, for today and the last 7 days. |

**Owner only**

| Command | Effect |
|---|---|
| `/account` | Status card per account (engine + health), with buttons. |
| `/account add <identity> <engine>` | Register an account for that engine and start its login. |
| `/account login\|remove\|default <identity> [engine]` | Relogin, remove, or make default (new bots inherit that account's engine). If the same email exists on several engines, pass `email/codex` or `email codex`. |
| `/access` | Who may talk to ccc (see below). |
| `/model [name]` | Show or set the model every bot runs on. `/model default` clears it. |
| `/setgroup` | Bind ccc to the forum group the command was sent in. |

### What a bot can do for itself

Every bot has these tools, and uses them without being told:

- `remember` / `recall` / `forget` — durable memory in the three scopes.
- `list_bots` / `send_to_bot` — message a teammate; the exchange is mirrored
  into both topics as 🤝, and the recipient wakes up with it. Grok and
  Antigravity bots do the same with `ccc tell <Name> <text>` (`--no-wake` for FYI).
- `notify_owner` / `ask_owner` — reach you; `ask_owner` renders inline buttons
  and the bot's turn ends until you answer.
- `watch` / `unwatch` / `list_watches` — a command re-run on an interval that
  wakes the bot only when its output changes.
- `schedule_wakeup` / `cancel_schedule` — one-off (or unnamed cron) wakeups.
- `set_routine` / `list_routines` / `cancel_routine` — named recurring work,
  timezone-aware (default `Europe/Madrid`), ⏰ in the topic when it fires.
  Grok/agy: `ccc routine add <name> --cron "0 9 * * 1-5" <prompt>`.
- `run_background` / `list_background` / `get_background` / `cancel_background`
  — start a long shell command without blocking the turn (builds, installs,
  waits). The bot is woken with the result when it finishes. Only the owner
  creates bots (a message in General); bots cannot spawn teammates.
- `archive_bot` — retire a bot (usually itself) and close its topic.
- `get_project` / `set_project` — the team's shared notes about a code base.
- `update_instructions` — rewrite its own role.
- `set_name` — rename itself and set its topic icon. The icon must be one of the
  emoji Telegram allows for forum topics; the tool lists them, and an emoji
  outside the set leaves the icon unchanged.
- `send_file` — send a file into its topic (refuses credential paths).

### What it costs, and what keeps it small

**One turn per burst, not per message.** When you send three lines in a row, an
idle bot waits `debounce_ms` (default 2500) for you to stop typing and answers
all of them in ONE `claude -p` run. Messages that arrive while a turn is running
already queue and are delivered together on the next one. A watch, a schedule,
a background job or another bot is never delayed. `ccc config set debounce_ms 0`
turns the wait off.

**Resumed turns are mostly cache reads.** The system prompt of a session is
byte-stable from turn to turn (the roster and icon list are sorted, nothing that
changes per turn is in it), so the API's prompt cache covers the conversation
and only the new message is charged as fresh input. `/usage` reports the cache
hit ratio per bot — if it drops, something started varying the prompt.

**Bots are told not to wake each other for nothing.** `send_to_bot(wake=true)`
starts a turn on the recipient; the system prompt tells them to use
`wake=false` for anything the other bot only needs to know, and to send one
message instead of five.

### Maintenance (it cleans up after itself)

Once a day at `maintenance_hour` (default 04:00 local) — or on demand with
`ccc maintain` — ccc keeps its own database small:

- **Turns**: it keeps 30 days OR the last 200 per bot, whichever keeps more, and
  after a week it replaces a turn's text with the first 500 characters. The
  status and the token usage are kept, so `/usage` still works on old turns.
- **Memories**: when one scope (your `user` memories, a project's, a bot's)
  passes 120 entries or 48 KB, ONE turn on a cheap model (`compaction_model`,
  default `haiku`) merges the duplicates and drops what a newer entry
  contradicts. It runs outside every bot, with no tools and no access to
  anything. You get a message in **General**:

  ```
  🧹 Compacted user memories: 143 → 61 (/memory restore 4 to undo)
  ```

  The originals are archived, so `/memory restore 4` puts them back exactly.
  If the result does not parse, or if it dropped more than 60% of the entries,
  **nothing is applied** and you are told why instead.
- **Cleanup**: delivered bot-to-bot messages and answered questions older than
  30 days, the private notes of bots archived over a month ago, and memory
  archives older than 90 days.

Tuning knobs. These live in `config.json` and have no Telegram command: the
defaults are meant to be right, and `ccc config` prints what is in force.

| Setting | Default | What it does |
|---|---|---|
| `debounce_ms` | 2500 | How long an idle bot waits for more messages before starting a turn. 0 disables it. |
| `compaction_model` | `haiku` | The cheap model the memory compaction runs on. An unknown name falls back to the instance model. |
| `maintenance_hour` | 4 | Local hour the daily job runs at. A machine that was off catches up when it wakes. |

```bash
ccc config                              # every key, with the defaults in force
ccc config set debounce_ms 0
ccc config set compaction_model sonnet
ccc config set maintenance_hour 22
```

### Access control

ccc is **default-deny by Telegram user id**. The owner (`chat_id`) is always
allowed; nobody else can do anything until you approve them.

- A stranger's message **in the group** is dropped in silence.
- A stranger's **DM** gets one reply with a 6-hex pairing code (valid an hour;
  at most 3 pending requests, at most 2 replies per person and then silence).
  You also get a DM with **Allow** / **Block** buttons.
- You approve with the button or `/access pair <code>`.
- `/access list`, `/access add <id>`, `/access remove <id>`, `/access block <id>`.

An approved user can talk to the bots. They cannot use `/account`, `/access`,
`/model` or `/setgroup` — those stay yours.

> Bots run with bypassed permissions on your machine. **The chat is the trust
> boundary**, which is why this is not optional. Secrets reach bots only through
> `env_passthrough`; ccc never posts environment values or credential files, and
> `send_file` refuses anything inside a config or credentials directory.

---

## Engines and accounts

Engine is a property of an **account**, set when you add it. One ccc process
can mix several Claude emails, several Grok logins, several Antigravity
logins and several Codex logins — including the **same email on different
engines**. A bot's turns pick a healthy account from
the pool that matches what that bot runs on. New bots inherit the default
account's engine (or `default_engine` if you set one). `/engine` only assigns
a bot onto another already-registered pool — it is not the way you introduce
an engine.

The **model** is not a property of the account. `/model <slug>` in a bot's
topic overrides that bot; `/model <engine> <slug>` in General (or a DM) sets
the instance default for that engine. Empty means the CLI's own default.

| Engine | Add account | Isolated home | Binary | Session | MCP |
|---|---|---|---|---|---|
| **Claude Code** | `/account add you@x.com claude` | `CLAUDE_CONFIG_DIR` under `<data_dir>/profiles/` | `claude` | ccc mints a UUID; `--session-id` then `--resume` | ccc MCP (`remember`, `send_to_bot`, …) |
| **Grok Build** | `/account add work grok` | `GROK_HOME` = `<data_dir>/accounts/grok/<id>` (`auth.json`) | `grok` (`~/.grok/bin/grok`) | ccc mints a UUID; `--session-id` then `--resume` | not wired — teammates via `ccc tell` (🤝 in both topics) |
| **Antigravity** | `/account add lab agy` | isolated `HOME` + `GEMINI_HOME` + `GEMINI_FORCE_FILE_STORAGE` under `<data_dir>/accounts/antigravity/<id>` | `agy` (`~/.local/bin/agy`) | first turn lets `agy` mint a `conversation_id`; later turns pass `--conversation` | not wired — teammates via `ccc tell` (🤝 in both topics) |
| **Codex** | `/account add openai codex` | `CODEX_HOME` = `<data_dir>/accounts/codex/<id>` (`auth.json`) | `codex` (PATH / `~/.local/bin/codex`) | first turn lets Codex mint a `thread_id`; later turns `codex exec resume <id>` | not wired — teammates via `ccc tell` (🤝 in both topics) |

```
/account add you@example.com claude
/account add you@example.com codex   # same email, different engine
/account add work grok
/account add lab antigravity
/account add openai codex
/account                         # mixed list: identity, engine, health
/account login you@example.com/codex # required when that email is on several engines

/engine                          # which pool this bot uses
/engine grok                     # assign this bot to the Grok pool (secondary)
/model grok grok-4               # instance default for that engine
/model gpt-5.4                   # this bot (in its topic)

ccc config set default_engine grok    # optional override for new bots
ccc config get default_engine
```

**Claude** is unchanged: profile pool, `CLAUDE_CONFIG_DIR`, same-engine
failover, MCP, `--output-format stream-json`.

**Grok Build.** `/account add work grok` creates an isolated `GROK_HOME` and
drives `grok login --device-auth` on a PTY (the VM has no browser). Turns set
`GROK_HOME` so `auth.json` is this account, not `~/.grok`. Failover is
Grok↔Grok. Turns run `grok --always-approve --output-format
streaming-messages-json --single <envelope>`. Guess labeled: the `grok`
binary was not on the machine that built this; flags were checked against
Grok Build's published headless docs and Hairok's Mac-verified set.

**Antigravity.** `/account add lab agy` creates an isolated HOME so
`~/.gemini` cannot collide with Claude profiles or the real user home, and
forces file-store tokens (`GEMINI_FORCE_FILE_STORAGE=true`) instead of the OS
keyring. Login is `agy auth login` as the CLI requires. A headless run that
is not already logged in exits with `authentication required`. Turns run
`agy --print <prompt> --output-format stream-json --dangerously-skip-permissions`
and `--conversation` on resume. `--print-timeout 120m` is a ccc choice (agy's
default is 5 minutes). Guess labeled: `agy` was not on the build machine;
flags match the official headless docs. Isolation is HOME/XDG/`GEMINI_HOME`
because agy has no official profile selector.

Progress streaming works when the CLI emits a usable NDJSON stream. If an
engine only prints the final answer, Telegram still gets that text (the
progress line stays at "thinking").

`/model` is still instance-wide and is passed through as `--model` on every
engine when set. Use a slug that engine accepts, or `/model default` for the
CLI's own default. An unknown agy model fails the turn.

`ccc doctor` reports `grok` and `agy` as optional.

---

## Where things live

| What | Where |
|---|---|
| Bootstrap config (token, owner, group, accounts) | `~/.config/ccc/config.json` (mode 0600) |
| Passthrough secrets for the service | `~/.config/ccc/env` (mode 0600, written by `ccc env sync`) |
| Everything runtime (bots, turns, memories, watches, schedules, access) | `<data_dir>/ccc.db` (SQLite, WAL) |
| A bot's default working directory | `<data_dir>/bots/<name>/workspace` |
| Files you send a bot | `<its cwd>/inbox/` |
| Claude accounts | `<data_dir>/profiles/<name>` (one `CLAUDE_CONFIG_DIR` each) |
| Grok / Antigravity accounts | `<data_dir>/accounts/grok/<id>` (`GROK_HOME`) and `<data_dir>/accounts/antigravity/<id>` (isolated HOME) |
| Shared conversation transcripts | `<data_dir>/projects`, symlinked into every Claude account |
| Logs | `~/Library/Caches/ccc/ccc.log` (macOS) or `journalctl --user -u ccc` |

`data_dir` defaults to `~/.local/share/ccc`.

Claude accounts point their `projects/` at the **same** directory on purpose:
that is what lets a turn refused by one Claude account resume the very same
conversation on another. Grok and Antigravity keep isolated homes and fail
over only inside their own pool.

---

## Fixing a command you mistyped

Edit the message. An edited message whose text starts with `/` runs as a
command, once per edit — so turning `/account add` into
`/account add you@example.com` does what you meant without sending a second
message. Editing plain text still does nothing: correcting a typo must not
re-run a turn.

---

## Troubleshooting

**A bot says an account needs a new login.** You will also get a DM with a
**Relogin** button. Tap it, or send `/account login <email>`, and follow the URL
+ code flow. The account is skipped for selection until it is fixed.

**`organization has disabled Claude subscription access`.** This reads like an
administrator blocked you, but in practice it is a stale OAuth token. It is
classified as `auth_stale`: the turn is retried on another account and the
profile is marked. Fix it with `/account login <email>`.

**Every account is rate limited.** The turn reports it and the accounts go on
cooldown until their cached reset time. `/status` shows the cooldowns.

**A bot cannot see `GH_TOKEN` (or any other secret).** `/status` lists the
`env_passthrough` names it can and cannot see. A missing one almost always means
`ccc install` / `ccc env sync` was run from a non-login shell, so the value was
never snapshotted into `~/.config/ccc/env`. Fix it with
`bash -lc 'ccc env sync'` and restart the service. The file is 0600 and the
systemd unit contains no secrets, only `EnvironmentFile=-%h/.config/ccc/env`.

**A compaction dropped something I wanted.** `/memory stats` shows the last
compaction id per scope; `/memory restore <id>` puts the originals back and
undoes it. Raise the thresholds by keeping fewer memories, or set
`ccc config set compaction_model` to a stronger model if the cheap one
consolidates badly.

**`systemctl --user` fails with "Failed to connect to bus".** Export
`XDG_RUNTIME_DIR=/run/user/$(id -u)` and make sure `loginctl enable-linger
$USER` is on. See step 5.

**Nothing responds in the group.** Check `/status` in the owner's DM. The usual
causes are a `group_id` that does not match the group (`/setgroup` fixes it),
the bot not being an admin, or Topics not enabled.

**A message got no reply and no error.** You are probably not the owner and not
approved — access control drops group messages from unknown users silently. Ask
the owner for `/access add <your id>`.

**A watch never fires.** Its first run is a baseline, not a change. Check it
with `/watches`; the interval floor is 60 s.

**Run the diagnostics.** `ccc doctor` checks the claude binary, every account's
login and disclaimer state, the configuration and the service. `ccc doctor
--fix` also records the bypass-permissions disclaimer for any account missing
it (the same write as `ccc profile accept-disclaimer <email>`).

---

## CLI

```
ccc listen                    Run the instance (the service does this)
ccc setup <bot_token>         Interactive bootstrap (owner, group, service)
ccc config [get|set] …        Non-interactive bootstrap
ccc setgroup                  Record the group from your next message in it
ccc install                   Install the service (launchd / systemd --user)
ccc env sync                  Snapshot env_passthrough secrets into ~/.config/ccc/env
                              (run from a login shell: bash -lc 'ccc env sync')
ccc maintain                  Run the daily growth-control job once, now
ccc doctor [--fix]            Check dependencies and configuration; --fix records
                              the bypass disclaimer for accounts missing it
ccc profile <cmd>             Manage accounts from a shell
                              (list/add/remove/default/login/accept-disclaimer)
ccc send <file>               Send a file into the topic of the bot owning this directory
ccc relay [port]              Relay server for files over 50 MB
ccc mcp --bot <id>            MCP server for one turn (spawned by Claude Code)
```

## Building

```bash
make build          # this machine
make build-linux    # ccc-linux-amd64 for the VPS
make test           # go build ./... && go vet ./... && go test ./...
make build-voice    # with whisper.cpp for voice transcription (needs cmake)
make install        # build + install to ~/bin/ccc
```

Tests that spend real tokens are behind a build tag:

```bash
go test -tags live -run TestLive -v -timeout 900s .
```

## Design

The specification ccc is built to is [`docs/DESIGN.md`](docs/DESIGN.md). When
the code and that document disagree, one of them is a bug.

## License

MIT — see [LICENSE](LICENSE).
