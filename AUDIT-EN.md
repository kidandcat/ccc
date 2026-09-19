# ccc — product & engineering audit (English summary)

Full report (Spanish, with evidence per finding): [`AUDIT.md`](AUDIT.md).
Target: `kidandcat/ccc` @ `main` (`45de516`, 2026-09-19). Method: full read of the Go code, `go build`/`vet`/`test`/`deadcode` on Linux, docs cross-check, skim of `kidandcat/ccc-app`. No deploy, no secrets reproduced.

## Verdict

**GO for single-owner daily use, with three conditions. NO-GO for multi-user (`/access add`) until F-18 is addressed.**
**Health score: 6.5 / 10.**

Conditions: (1) fix hub pairing verification or disable the hub (`ccc config set hub_url -`); (2) add a worker turn cap and a way to stop/kill a worker from Telegram; (3) accept that `main` is DM-only — the forum-group "Mac" model from the operator brief no longer exists.

What is genuinely good: stateless CLI runner with a byte-stable system prompt (cache-friendly), SQLite as the single runtime store, detached background jobs that survive restarts, blind secret injection with output redaction, per-account isolation across four engines, a 68 KB design spec that matches the code, 355 tests at 59.5% coverage, and unusually good code comments.

What pulls the score down: operability under failure (hung workers, Telegram I/O without timeouts on the update goroutine, silent failure on a bad token), a default-on phone hub that trusts the relay for pairing, two unbounded token leaks, and no CI.

## Operator brief vs. code

| Brief | Reality on `main` |
|---|---|
| Forum group + topic per session, group "Mac" | Gone. DM-only; group messages dropped (`listenv3.go:456-458`, DESIGN §14.31). |
| Engine chosen at `/account add`; accounts unique by identity+engine | Confirmed (`account.go:304-327,367-371`). |
| `spawn_bot` removed | Confirmed; `sendToBot`/`listBots`/`updateInstructions` remain as dead code (`mcp.go:293,317,482`); `ccc tell` CLI re-enables worker messaging (`botmsg.go:56`). |
| Jobs > 1 min non-blocking, restart-safe | Confirmed for `run_background` and General's 60 s cap + auto-spawn. Worker turns are requeued after restart but have **no timeout** (F-01). |
| Token/config corruption incidents | Atomic + mutexed writes, but no `getMe`/`getChat` anywhere (F-04). |

## Findings by severity

**Critical**
- **F-17** Hub pairing does not verify the code locally: the delete's `RowsAffected` is discarded (`hub_client.go:220`), so a malicious/compromised relay can pair a rogue device and gain full RPC (`send` to General = shell with bypass permissions). Hub is on by default (`hub_proto.go:18,30`). No test covers `handlePair`. Fix: require `RowsAffected == 1`.

**High**
- **F-01** Worker turns have no timeout (`chief.go:49-53`, `runner.go:1160`); `/stop` only hits General (`listenv3.go:917`); `archiveBotRow` kills jobs but not the turn (`store.go:805`). A hung `claude -p` in a worker can only be cleared by restarting the service.
- **F-02** All Telegram sends/edits/downloads use the default `http` client with no timeout (`telegram.go:45,158,482`) and run synchronously in the `getUpdates` loop (`listenv3.go:284-296`).
- **F-04** Invalid token, wrong `chat_id`, and 409 Conflict (two instances) only appear as a log line every 5 s (`listenv3.go:276`); `ccc doctor` never calls Telegram (`commands.go:225-227`); README's "run `/status`" advice is unreachable for a locked-out owner.
- **F-13** Every idle worker wakes General every 10 min forever until archived (`scheduler.go:251-306,355-366`, `chief.go:46`); each wake is a full General turn.
- **F-14** General's conversation is never rotated (`runner.go:1022`, `scheduler.go:231`); it grows unbounded and is re-charged whole after any > 1 h gap.
- **F-18** `/access add` is effectively remote shell: approved users prompt General, which runs with bypass permissions; replies go to the owner's DM (`listenv3.go:450-500`, `access.go:59-78`).
- **F-26** No CI runs tests; only tag-release and Pages workflows exist.

**Medium**
- **F-03** No `429`/`retry_after` handling; a failed final `sendMessage` loses the reply (`telegram.go:213-235`, `progress.go:172-179`).
- **F-05** No `recover()`; a panic crashes the process, launchd restarts it, the unconfirmed update is reprocessed, and "🔁 ccc is back" spams (`listenv3.go:314`, `service.go:57`).
- **F-06** Single-threaded scheduler: watches (≤ 2 min each) and doctor probes block schedules and background-job reaping (`scheduler.go:62-93`).
- **F-07** Attachments, Whisper transcription and the 466 MB model download run inside the update loop (`listenv3.go:742,769`, `whisper.go:40`).
- **F-08** ccc refreshes Claude/Grok/Codex OAuth tokens out-of-band with the CLI's client id and writes them back; a persist failure logs the CLI out (`usagefetch.go:35,182-200,291-330`).
- **F-15** `ccc mcp` runs `AutoMigrate` + FTS DDL on every turn (`mcp.go:70`, `store.go:326-335`).
- **F-19** `ccc setup` takes the first sender as owner, no private-chat filter, no confirmation (`commands.go:100-117`).
- **F-20** `ccc tell` and `ccc send` bypass the MCP rules (`botmsg.go:56-106`, `relay.go:21-66` vs `mcp.go:566`).
- **F-21** `secrets_delete` is exposed to every session (`mcp.go:857-860`).
- **F-24** Three log sinks, `listenLog` lines written twice under launchd, no rotation, no levels (`commands.go:19-38`, `helpers.go:50-57`, `service.go:59-62`).
- **F-25** No metrics, no `ccc logs`, no owner alert when the Telegram loop or hub is down.
- **F-27** `make test` is red on Linux: PTY fixture uses `printf '\x…'` which `dash` does not support (`ptyflow_test.go:88`).
- **F-28** 17 unreachable functions incl. v2 leftovers `updateCCC`, `setBotCommands` (with `/update`, `/cleanup`, `/c`), `sendToBot`, `listBots`, `updateInstructions`.
- **F-01b** Worker watches/schedules/jobs are invisible and uncancellable from Telegram (`/status` shows counts only).

**Low**
- **F-09** `foldQueue` merges heterogeneous sources under one `source` label (`runner.go:520-544`).
- **F-10** A pending login swallows the owner's next DM as the OAuth code (`account.go:544-570`).
- **F-11** No `/start`/`/help` (both → "Unknown command."); `/session` missing from the Telegram menu; `/name` registered but always refuses (`listenv3.go:411-430,906-910,1012`).
- **F-12 / F-30** Stale copy ("Talk to the sessions in the group.", `access.go:261,355`; `ccc pair` promises a QR, `commands.go:321`); comment says hub default `hub.getccc.dev` vs code `hub.mentasystems.com` (`main.go:23`, `hub_proto.go:18`); brand inconsistency (ccc vs "Crew Command Center") across repos.
- **F-16** `sessionPanel.sync` runs three queries on every tool event; `hookLog` opens the file per line.
- **F-22** Hub metadata leak by default; 24-bit pair codes with no rate limit on the hub; `SSH_AUTH_SOCK` passed to sessions.
- **F-23** Prompt-injection posture is prompt-only (expected with bypass); the "data from the world" sentence also covers `source="user"`.
- **F-29** CI `go 1.24` vs `go.mod 1.25`; `modernc.org/sqlite v1.23.1` (2023); otel/grpc pulled in via go-whisper even for non-voice builds; 405 baselined lint issues.
- **F-31 – F-34** Cross-engine failover rotates a worker's conversation silently; `pickSpawnEngine` may choose an unexpected engine; up to 8 "needs login" pings per turn; `classifyFailure` matches loose substrings (`"429"`, `"network"`).

## Prioritized backlog

**P0**
1. F-17 — verify pair code in `handlePair`; add a test with a forged `pair` frame.
2. F-01 — worker turn timeout (config key) + `/stop <session>` and `/kill <session>`; make `archive` interrupt the active turn.
3. F-02 — shared `http.Client{Timeout}` for `telegram.go`; move sends/downloads off the update goroutine.
4. F-04 — `getMe` at boot and in `doctor`; `getChat(chat_id)` in `doctor`; detect 401/409 loudly; fix README troubleshooting.
5. F-26 / F-27 — `test.yml` on push/PR (build, vet, `test -race`, deadcode); make the PTY fixture portable.

**P1**
6. F-13 — idle-reminder backoff + cap; auto-archive idle workers after N h.
7. F-14 — rotate General by accumulated tokens or hours; show its size in `/status`.
8. F-18 — restrict `roleUser` to read-only + answering `ask_owner`; document the real meaning of `/access add`.
9. F-03 — handle `429`/`retry_after`; persist an undelivered reply and retry.
10. F-06 — run watches/doctor concurrently with a limit; keep the job ticker unblocked.
11. F-24 / F-25 — one `slog` log, no double append, rotation, `ccc logs -f`, owner alert on a dead loop.
12. F-08 — default off for out-of-band OAuth refresh (show `?` instead of risking a logout).
13. F-20 / F-21 — apply MCP rules to `ccc tell`/`ccc send`; remove `secrets_delete` from MCP.
14. F-05 — `recover()` in handlers/goroutines; `ThrottleInterval` in the plist; debounce "ccc is back".
15. F-07 — attachments/transcription off the update loop; timeout on model download.
16. F-19 — `ccc setup` accepts private chats only and asks for confirmation.

**P2**
17. F-11 / F-12 / F-30 — `/start`, `/help`, menu cleanup, stale strings, brand alignment, rename root `DESIGN.md`.
18. F-28 — delete dead code and `Role`/`ParentBotID`.
19. F-15 — no `AutoMigrate` in `ccc mcp`.
20. F-16 — coalesce panel redraws; keep the debug log file open.
21. F-01b — per-session `/watches`, `/schedules`, `/jobs`.
22. F-09, F-10, F-29, F-31–F-34 as listed in `AUDIT.md` §10.
23. `ccc backup` (config + env + secrets + db).
24. Split the 32k-line `main` into internal packages, starting with `telegram` and `hub`.

## ccc-app appendix

Flutter client (`lib/`: `hub.dart`, `crew.dart`, `sessions.dart`, `decisions.dart`, `md.dart`, `notify.dart`, `main.dart`; NaCl via `pinenacl`, keys in `flutter_secure_storage`). It speaks only the hub RPC defined in `hub_proto.go`/`hub_client.go` (`bots`, `history`, `send`, `answer`, `archive`, `rename`, file chunks) and receives `post`/`progress`/`question`/`session` events. It is the owner's second surface, equivalent to the DM, plus a per-worker log Telegram does not show. It inherits F-17 (pairing) and F-01 (no stop/kill in the RPC), and depends on the third-party `hub.mentasystems.com` unless the owner runs `ccc hub`. Dart code was not audited.

## Not reviewed

ccc-app Dart code; real behaviour of `grok`/`agy`/`codex` CLIs; `relay.go`/`Dockerfile.relay`/`fly.relay.toml` beyond a skim; `ptyflow.go`, `enginelogin.go`, `markdown.go`, `usage.go`, `profilecmd.go`, `maintenance.go` line by line; `live`-tagged tests; measured latency/token costs (this audit is static); the operator's actual Mac configuration.
