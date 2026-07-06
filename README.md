# ccc - Claude Code Companion

> Control [Claude Code](https://claude.ai/claude-code) **background-agent** sessions from Telegram. Start work from your phone, talk to Claude in a topic, tap buttons to answer its questions, and pick the session back up on your PC with `claude attach`.

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

---

## What ccc is (v2)

Each ccc session is a **dedicated Claude Code background agent** — the same thing you see in `claude agents` (the fleet view). One Telegram topic maps to one bg agent with its own clean, isolated conversation. ccc mirrors that agent into Telegram: you send messages, Claude's replies come back to the topic, and when Claude needs a decision it shows the options as **inline buttons**.

This is built for **real, focused work** — one project per topic, isolated context — not fire-and-forget one-liners.

### How it works

```
┌────────────┐   message    ┌──────────┐   claude --bg     ┌──────────────┐
│  Telegram  │─────────────▶│   ccc    │──────────────────▶│  Claude bg   │
│   topic    │◀─────────────│  listen  │◀── agents --json ──│    agent     │
└────────────┘   replies    └──────────┘   + transcript     └──────────────┘
                                  ▲                                │
                                  │   PreToolUse hook (buttons)    │
                                  └────────────────────────────────┘
                                       AskUserQuestion
```

- **Dispatch** — the first message to a topic starts a bg agent (`claude --bg`) with that message as the prompt. It appears in `claude agents`.
- **Mirror (pull)** — `ccc listen` polls `claude agents --json` and the session transcript, delivering Claude's text and status (working / needs input / done) to the topic.
- **Follow-ups** — later messages resume the conversation.
- **Questions** — a scoped PreToolUse hook (installed per-agent via `--settings`, never touching your global config) turns `AskUserQuestion` into Telegram buttons and feeds your tap back to Claude.
- **Handoff** — `claude attach <id>` opens the same session in your terminal.

## Features

- **Background agents** — sessions show up in `claude agents`; no tmux
- **Dedicated context per topic** — one project, one clean conversation
- **Inline question buttons** — tap to answer Claude's `AskUserQuestion`
- **Voice & images** — voice messages are transcribed; images are handed to Claude
- **File transfer** — `ccc send <file>` (direct, or streaming relay for large files)
- **Seamless handoff** — `claude attach` to continue on your PC
- **Self-hosted** — runs entirely on your machine

## Requirements

- macOS or Linux
- Go 1.21+ (to build)
- [Claude Code](https://claude.ai/claude-code) **with background-agent support** (`claude --bg` / `claude agents`)
- A Telegram account + bot

## Install

```bash
git clone https://github.com/kidandcat/ccc.git
cd ccc
make install        # builds, signs (macOS), installs to ~/bin/ccc
ccc --version       # ccc version 2.0.0
```

## Setup

```bash
ccc setup <BOT_TOKEN>
```

This connects to your bot, optionally configures a group with Topics, installs the `ccc-send` skill, and installs + starts the listener service.

For session topics: create a Telegram group with **Topics enabled**, add your bot as **admin**, then run `ccc setgroup` (or send a message in the group during setup).

## Usage

### Terminal

| Command | Description |
|---------|-------------|
| `ccc` | Attach to this directory's bg session (or run `claude`) |
| `ccc -c` | Continue the previous local session |
| `ccc start <name> <dir> <prompt>` | Start a detached session with an initial prompt |
| `ccc send <file>` | Send a file to the current session's topic |
| `ccc doctor` | Check dependencies and configuration |
| `ccc config` | Show / set configuration |
| `ccc setgroup` | Configure the Telegram group for topics |

### Telegram (in your group)

| Command | Description |
|---------|-------------|
| `/new <name>` | Create a new session + topic |
| `/new` | Reset the conversation in this topic (fresh context) |
| `/stop` | Stop this session's agent (conversation is kept) |
| `/delete` | Delete this session + topic |
| `/cleanup` | Delete all sessions + topics |
| `/list` | List sessions and their status |
| `/c <cmd>` | Run a shell command |
| `/stats` | System stats |
| `/update` | Update ccc from GitHub |

Send a plain message in a topic to talk to that session's Claude agent. When Claude asks a question, tap the buttons to answer. Send a voice message or image and it's forwarded to Claude. In a **private chat**, any message runs a one-shot Claude query.

## Configuration

`~/.config/ccc/config.json`:

```json
{
  "bot_token": "…",
  "chat_id": 123456789,
  "group_id": -1001234567890,
  "sessions": {
    "myproject": { "topic_id": 42, "path": "/home/user/myproject" }
  },
  "projects_dir": "/home/user/Projects",
  "transcription_lang": "es"
}
```

`session_id` / `short_id` are added per session at runtime (the current bg agent). `topic_id` is the stable key.

## Permissions

Background agents run unattended with `--dangerously-skip-permissions` (auto-approve), so they can work without blocking. `AskUserQuestion` is the exception — it always asks you, via inline buttons.

## Notes

- Sessions are **lazy**: `/new <name>` registers the topic; the first message starts the agent.
- The bg-agent short id changes on every resume; ccc tracks the current one automatically.
- Live end-to-end validation of the primitives lives in `live_test.go` (gated behind `CCC_LIVE_TEST=1`).

## License

[MIT License](LICENSE)
