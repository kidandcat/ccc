# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Stack

Static HTML and CSS in `docs/`, deployed by `.github/workflows/pages.yml` on push to `main` (path `docs/**`) to GitHub Pages at https://kidandcat.github.io/ccc/. No framework, no Go server, not Fly. Inferred from the landing brief (2026-09-18) and the existing workflow; labeled inferred where the brief was the source.

## Users

Primary: someone who already has (or will have) Claude, Grok, Codex, or Antigravity subscriptions, and who wants **one Telegram assistant** instead of four separate chats. They run those CLIs on a machine they own. They are often away from a desk (phone, couch) and do not want a web dashboard, a Slack app, or a forum of session topics. Coding is a common use, not the pitch.

Secondary: someone evaluating the public OSS repo (`kidandcat/ccc`) before cloning. They need an accurate picture, not a sales fiction.

## Product Purpose

**ccc** is a personal assistant in Telegram. One DM; every subscription you already pay for (Claude, Grok, Codex, Antigravity). The bot's 1:1 DM is the **orchestrator**: you talk to it, it sees live sessions, and it can start a backend worker (`spawn_session`) or message one (`tell_session`). Sessions live in the backend — no Telegram topic. The owner never writes into a session chat. Workers report only to the orchestrator (`report_to_general`); the owner does not see the transcript. The orchestrator posts the owner-facing result in the DM — a structured digest stays readable; it is not crushed into one paragraph.

Success for this landing: a first-time visitor understands they get one assistant for every AI subscription they already pay for, in Telegram; believes the product is self-hosted OSS (not a hosted chat SaaS); and goes to https://github.com/kidandcat/ccc.

## Positioning

Visitor-facing pitch: **one personal assistant, every subscription, in Telegram.** Coding is a use, not the category. The mechanism a neighboring product could not copy without becoming ccc: **one Telegram DM is the orchestrator; sessions are backend workers with no chat of their own.** Quiet owner UX (`ask_owner` buttons, vault secrets the model never reads, watches that cost nothing until output changes, routines that fire as fresh workers). Engines are interchangeable runners (Claude Code default; Grok Build, Antigravity, Codex) behind the same envelope — the subscriptions you already pay for, one place.

Not: a Claude Code plugin, a Slack bot, a web agent dashboard, or Claude background agents / `claude attach` (explicitly dropped in v3).

## Operating Context

- One `ccc listen` process on one machine you own (laptop or desktop). A VPS is optional, not required. Bound to one Telegram bot token. Owner `chat_id` is the access-control root.
- Default-deny by Telegram user id. Owner is `chat_id`; extra ids are `allowed_user_ids` in config (loaded at start). Allowed users can talk in the DM; `/account`, `/access`, `/model`, `/secret` stay owner-only. Unknown DMs are dropped in silence.
- Bootstrap is headless: BotFather token, `ccc config` / `ccc install` (launchd / systemd --user). Login URLs go to Telegram; ccc never opens a browser on the machine.
- Implementation spec (not visual): [`docs/DESIGN.md`](docs/DESIGN.md). **Do not overwrite that file with a visual design system.** Visual DESIGN.md, if written, lives at the repo root.

## Capabilities and Constraints

Confirmed (README / `docs/DESIGN.md`):

- Orchestrator 60s cap; longer work goes to a session. If the cap fires and the orchestrator does not spawn, ccc starts the session itself. `/session <prompt>` starts a worker without the orchestrator.
- Live session card in the Telegram DM (one block per working session), pinned while a worker is running, waiting, or on a background job. `/sessions` is the full list.
- Idle sessions waiting on the owner (no pending `ask_owner`) wake the orchestrator **once per idle spell** after 10 minutes (inbox, not a chat ping). The orchestrator conversation is rotated after `idle_compact_s` like any other session. Parked questions stay in the DM; the next free-text message lists them. Tapping one answers similar pending questions. Every `ask_owner` keyboard ends with **Omitir** (skip without choosing, unblocks the session).
- `/stop` kills the active turn (orchestrator or a named worker). Hung CLIs get SIGTERM, then SIGKILL. Workers have a 30m cap (`worker_turn_timeout_s`).
- `/access add` does not grant anyone. Extra users are config whitelist only (`allowed_user_ids`). There is no phone pairing hub.
- Tools sessions actually have: `remember` / `recall` / `forget`; `notify_owner` / `ask_owner`; `watch` / `schedule_wakeup` / `set_routine`; `run_background` / `run` with vault inject; `secrets_list` / `secrets_delete` (no `secrets_get`); orchestrator-only `spawn_session` / `tell_session`; workers-only `report_to_general`; `send_file`; `get_project` / `set_project`; `set_name`; `archive_bot`.
- Engines: Claude Code, Grok Build, Antigravity, Codex. Failover stays inside the same engine.
- Owner vault (`/secret add`); values never shown; sessions inject via env/stdin.
- MIT license. Go 1.25+.

Landing constraints (brief, 2026-09-18):

- Copy in English. Follow README facts; **do not invent features.**
- Link to https://github.com/kidandcat/ccc. No secrets, no personal phone numbers.
- The incumbent `docs/index.html` is a v2-era landing (forum topics, `claude attach`, “background agents”, invented “interactive approvals”). It is **anti-reference** for product truth. Replace it; do not polish those claims.

Undecided / do not fabricate: user counts, testimonials, pricing (there is none — self-hosted), benchmarks, screenshots of a real production DM.

## Brand Commitments

- Product name is **ccc** (lowercase in running text). No expansion (not “Crew Command Center”).
- Public copy calls the 1:1 DM the **orchestrator** (English pages: that word, not Spanish). The implementation session is still named General (`report_to_general`, `topic_id` 0). Do not rename listen, the dispatcher, or the DM in code.
- Voice in the README: precise, second-person, short. No hype, no “join developers who…”. Landing copy should stay in that register even when it is friendlier than the README.
- Telegram is the interface, not the brand color by default.
- MIT. Public repo. Public story is Telegram (+ CLI / `ccc listen`).
- **Landing visual world (standing preference, 2026-09-18):** category canon — a typical 2026 product site, sitting alongside Cursor, Claude, and Codex in craft. Played straight at their craft level. No radio / ATC / sewing (or any other governing metaphor). No neo-terminal costume. No irony or smuggled quirk. Pitch is assistant + subscriptions, not “coding sessions”.

## Evidence on Hand

- [`README.md`](README.md) — public product facts.
- [`docs/DESIGN.md`](docs/DESIGN.md) — implementation specification (v3).
- [`docs/index.html`](docs/index.html) + [`docs/style.css`](docs/style.css) — incumbent Pages landing; visually a dark neo-terminal with Outfit/IBM Plex; factually stale. Use as anti-reference for claims.
- Live URL already: https://kidandcat.github.io/ccc/ (workflow from `docs/`).
- No photography, no logo lockup, no testimonials. Do not invent them.

## Product Principles

1. **The DM is the orchestrator.** If a sentence implies the owner chats with a worker, it is wrong.
2. **Facts over atmosphere.** Friendliness is tone and craft, not extra capabilities.
3. **Self-hosted is the product.** Telegram is the interface. There is no public phone hub.
4. **Quiet by default.** Progress is silent; the ping is the answer; `ask_owner` is how decisions happen.
5. **One instance, one bot.** Do not draw a mesh of bots talking to each other.

## Accessibility & Inclusion

No product-specific legal standard was set. The landing must remain usable: real headings, keyboard-focusable controls, contrast that holds on the chosen ground, no information in color alone. English-only copy is a brief constraint, not a claim that other languages are unsupported in the binary.
