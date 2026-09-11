# agentchat-mcp — Full Review & Rebuild Plan

*Review date: 2026-09-11. Every defect marked **[confirmed]** was verified by reading the code and, for the concurrency bugs, reproducing them with `go test -race` (repro tests were run and then removed).*

---

## 1. Executive summary

The system is a Go server (REST + server-side mailboxes + an SSE `/watch` event stream) with two clients: an MCP stdio bridge (for Claude Desktop / Cursor / picobot) and a human CLI. The intended design is sound: **REST for data, a persistent SSE connection for wake-up signals, mailboxes for guaranteed delivery.**

The implementation fails that design in four compounding ways:

1. **The server crashes under real traffic.** Two data races — one fatal — reproduce with `-race`. The fatal one (unsynchronized sequence counter) kills the process the moment two agents send concurrently; the other panics an in-flight request every time an SSE client disconnects near a broadcast.
2. **The SSE wake-up path silently dies and never reconnects.** The server sends no keepalives, so idle streams are dropped by NAT/proxies and the client blocks on a dead socket forever without an error — the 5s reconnect loop exists but never fires. When a reconnect does happen, or when the picobot socket is briefly unavailable, messages are missed and never re-signalled.
3. **Presence expiry destroys mail.** A 60s idle TTL deletes the agent's mailbox including unread messages, hard-fails direct sends to that agent, and skips it in broadcasts. Non-picobot agents have no heartbeat at all (it's gated behind the signal-socket config), so any agent that thinks for a minute "drops off the network" and loses whatever was queued for it.
4. **The message-drain path re-delivers stale messages.** `drainAll` merges mailbox + history but never advances its high-water mark, so agents see duplicates of other agents' private traffic on every poll — and `wait_for_message` filters destructively discard non-matching messages.

Net effect: agents appear to "drop their connection and never reconnect," messages vanish, and the server periodically dies. The fixes are tractable — most of Phase 0 is a few days of work — but they should be done in the order below, because later phases depend on the event pipeline being single-sourced.

---

## 2. Architecture as it stands

```
MCP host (Claude/Cursor/picobot)                             human
   │ stdio (JSON-RPC)                                          │
   ▼                                                           ▼
agentchat-mcp-bridge ──REST──► agentchat-server ◄──REST── agentchat-cli
   │  (per-agent mailbox drain,                 ▲
   │   sends, scratchpad, files)                │ SSE /watch?session&psk
   └─SSE /watch ──► on event: signal ──► picobot Unix socket ("Gino" signal path)
```

Server internals: `session.Store` (in-memory PSK map), `presence.Tracker` (TTL 60s, 15s sweep), `mailbox.Store` (per-agent queues, destructive drain), `hub` (routing + history capped at 100/session + sequence numbers), `api.Watcher` (per-session SSE subscriber sets), all in-memory — nothing survives a restart.

Two independent event fan-outs exist and have drifted:

- **Mailbox fan-out** (hub, 9 call sites): agent_joined/left, leader info, direct messages, broadcasts, scratchpad updates, file shares.
- **SSE fan-out** (api layer, 3 call sites): agent_joined, direct messages, broadcasts only.

So scratchpad updates, leader transfers, and agent-expiry (`agent_left`) **never reach SSE watchers** — the bridge can't signal picobot about them and the CLI never sees them.

---

## 3. Root causes of "agents drop and never reconnect"

### 3.1 Server crashes (P0, confirmed)

**[confirmed] Fatal: concurrent map write on sequence numbers** — `internal/hub/hub.go:68-71`
`nextSeq()` increments `h.seqNums[sessionID]` with no lock, and is called from `SendMessage`/`Broadcast`/`ScratchpadSet`/`ScratchpadDelete`/`LeaderTransfer` without holding `h.mu`. Two concurrent sends → `fatal error: concurrent map writes`, which is unrecoverable — the whole process dies. Reproduced under `-race` with 50 goroutines. Under multi-agent load this is the "server just stops working" event. After it dies, every bridge's SSE reconnect loop hammers a dead server, and whichever bridge reconnects first re-creates the session (see 3.4).

**[confirmed] Panic + data race in the SSE watcher** — `internal/api/watcher.go:35-60`
`Notify()` grabs the subscriber set under `RLock`, then iterates and sends **after releasing the lock**; `Unsubscribe()` concurrently deletes and `close()`s the channel. Result: `send on closed channel` panic and a map-iteration race, both reproduced under `-race`. `net/http` recovers the panic, but the sender's in-flight HTTP request dies with a connection reset — a message send that was already committed to mailboxes returns an error, so clients can't know whether it was delivered.

**[confirmed] Data race in history catch-up** — `internal/hub/hub.go:305-327`
`GetHistoryAfter` takes the slice header under `RLock`, releases the lock, then iterates while `addToHistory` may be appending into the same backing array. `GetHistory` (which copies under the lock) is safe; this one is not.

### 3.2 The SSE stream dies silently and stays dead (P0)

- **No server keepalive** — `internal/api/api.go:567-622`: the handler writes nothing between events. Any NAT gateway, load balancer, or proxy idle timeout silently drops the stream. The bridge then blocks in `bufio.Scanner.Scan()` forever — **no error, no reconnect**. The bridge's 5s reconnect loop (`cmd/agentchat-mcp-bridge/watcher.go:55-73`) only helps when the socket actually errors; a half-open connection is indistinguishable from a quiet one.
- **No re-signal after recovery** — missed-during-disconnect messages land in the mailbox but nothing ever tells picobot: `initLastSeq` runs once at startup (`watcher.go:132-174`), and a failed `SendToSocket` (picobot restarting) is logged and dropped with no retry (`watcher.go:322-328`). There is no "unread messages → re-signal" safety net anywhere.
- **Event gap on connect** — the SSE handler fetches history *before* subscribing (`api.go:589-599`); events in between are lost for that connection.
- **CLI reconnect has no backoff** — `cmd/agentchat-cli/main.go:216-220` respawns `watchSSE` immediately on failure; against a down server it spins.

### 3.3 Presence expiry destroys messages (P0)

`presence` TTL is 60s (sweep every 15s). On expiry, `hub.onAgentExpired` (`internal/hub/hub.go:99-126`):

1. **Deletes the agent's mailbox** including unread messages (`hub.go:125`). The README promise — "queued regardless of connection state" — is false.
2. Before that, `SendMessage` refuses expired targets (`hub.go:138` returns "agent not found in session"), and broadcasts skip expired agents (`deliverToSessionMailboxes` iterates *present* agents only). So a message sent to a thinking-for-90-seconds agent fails outright instead of queuing.
3. On its next tool call the agent silently resurrects with **no capabilities** (`RefreshPresence` → `Touch(..., nil)` ignores the `isNew` return — no `agent_joined`, empty caps in `list_agents`).

Worse, **most agents never heartbeat at all**: the 30s heartbeat lives inside `startWatcher`, which returns early when no picobot signal socket is configured (`watcher.go:36-39`). Plain Claude Desktop/Cursor bridges and the CLI register once and expire 60s later.

Also: `presence.NewTracker(ttl)` ignores its argument and always uses 60s (`internal/presence/presence.go:28-37`) — the "configurable TTL" is a lie.

### 3.4 Message-duplication and destructive-filter bugs (P1)

- **`drainAll` never advances `lastSeq`** (`cmd/agentchat-mcp-bridge/main.go:195-263`). It fetches history `after_sequence=lastSeq`, but `lastSeq` is only updated by relevant *SSE* events. History contains **all** direct messages in the session, so every `receive_messages` re-returns other agents' private traffic since the last relevant SSE event. Dedup is per-call only.
- **`wait_for_message` destroys non-matching messages** (`main.go:407`): destructive read, filter locally, log "discarded by destructive read". A message from agent B is permanently lost because the agent was waiting on agent A.
- **History cap is 100/session** — catch-up after a long disconnect is impossible; `drainAll` fetches only 50.

### 3.5 Auth is effectively optional (P1)

`auth` and `authQuery` (`internal/api/api.go:84-98, 121-129`) **auto-create any session with any caller-supplied PSK** when the session doesn't exist. Consequences:

- Any client can claim any session ID that isn't currently in memory — including *immediately after a server restart*, before the legitimate agents reconnect, hijacking the session with a PSK of their choosing.
- `GET /watch/sessions` (list of all sessions and agents) is accessible with garbage credentials.
- PSKs travel in query strings on `/watch` (`?psk=...`), so they end up in proxy/access logs.

### 3.6 Everything is in-memory (P1)

Server restart wipes sessions, mailboxes, history, scratchpad, and files. This converts every crash/restart into: sessions possibly hijacked (3.5), all history gone, bridges stuck with stale `lastSeq`. The bridge's `initialized` flag is also never reset (`main.go:102-126`), so after a server restart the bridge never re-registers with capabilities — it relies on the auth middleware's presence touch.

### 3.7 Smaller defects (P2)

- MCP server (`internal/mcp/server.go`): requests handled serially in the read loop, so `wait_for_message` (up to 600s) blocks everything; no `ping` method (hosts get "method not found"); `Result` has `omitempty` so an empty-string tool result produces an invalid JSON-RPC response.
- Bridge serializes **all** HTTP calls behind `b.mu` held across full round trips (`main.go:128-137`).
- `pendingSignal` metadata is recorded but never surfaced to the agent; `getPendingSignalInfo` is dead code.
- `signal.SendToSocket` does a single 1024-byte read — truncated responses.
- `GetAgents` returns map-random order; `list_agents` output reshuffles every call.
- CLI never heartbeats (human expires in 60s, DMs to them fail) and never drains the mailbox.
- README drift: `AGENTCHAT_AGENT_NAME` no longer accepted; "lazy connection" contradicted by eager registration when a signal socket is configured.
- Repo hygiene: 7-8MB binaries (`cli`, `main`, `agentchat-mcp-bridge`) sitting untracked at repo root; stray `tmp` log file; no CI, no lint target.
- Test coverage: only `api` and `mcp` packages have tests; hub, presence, mailbox, watcher (the racy parts) have none.

---

## 4. The plan

### Phase 0 — Stop the bleeding (crashes + message loss) · ~2-3 days

1. **Guard the sequence counter** (`hub.go`): protect `nextSeq` with `h.mu` (or replace `seqNums` with per-session atomic counters owned under one mutex). Add the concurrency repro test permanently.
2. **Fix the Watcher**: hold `RLock` for the whole `Notify` loop (the non-blocking send is cheap and safe under the read lock), and never `close` subscriber channels — let disconnected channels be garbage-collected; or give each subscriber a `done` channel and select on it. Add the race repro test permanently.
3. **Fix `GetHistoryAfter`**: copy under the lock like `GetHistory`.
4. **Make mailboxes survive presence expiry**: remove `DeleteBox` from `onAgentExpired`, remove the `IsPresent` check from `SendMessage` (mailbox delivery already creates boxes on demand — presence should only affect *visibility*, not *deliverability*). Mark expired agents `online: false` in `list_agents` instead of hiding them.
5. **Heartbeat for every bridge**, not just picobot ones: move `presenceHeartbeat` out of the `signalSocketPath` gate in `startWatcher`. Fix `NewTracker` to honor its `ttl` argument.
6. **SSE keepalives**: server sends `: ping\n\n` every ~15s per open stream (and a `retry: 3000` hint on connect). This alone converts silent stream death into a detectable error.

### Phase 1 — Make the wake-up path actually reliable · ~2-3 days

7. **Bridge SSE hardening**: exponential backoff with jitter (500ms → 30s cap) instead of flat 5s; a watchdog that force-reconnects if no bytes arrive within ~45s (belt-and-braces with server pings); re-run `initLastSeq` on every reconnect; on reconnect, if the mailbox is non-empty, send a `check_messages` signal immediately.
8. **Signal-send retry**: queue failed `SendToSocket` attempts and retry with backoff; add a low-frequency fallback sweep (e.g., every 60s: if mailbox non-empty and no signal sent in the last N seconds, re-signal). A missed signal must never be final.
9. **Close the connect gap**: subscribe to the watcher *before* fetching history in the SSE handler, and tag history events with their sequence so clients can dedupe (or drop history replay entirely once clients have reliable catch-up).
10. **Fix `drainAll` semantics**: single source of truth = the mailbox. Advance `lastSeq` after each successful drain (and dedupe against it across calls). History catch-up (`request_history`) stays as an explicit tool, not an implicit merge.
11. **Stop destructive filtering**: `wait_for_message` holds back non-matching messages in an in-bridge holdback queue and prepends them to the next drain result, instead of discarding them. (Server-side long-poll with server-side filtering is the cleaner long-term fix — see Phase 3.)

### Phase 2 — Unify the event pipeline · ~2-3 days

12. **One fan-out, two sinks.** Move all event emission into `hub`: a single `emit(sessionID, Envelope)` that (a) assigns the sequence, (b) appends history where appropriate, (c) routes to mailboxes, (d) notifies SSE watchers. The API layer stops calling `watcher.Notify` directly. This immediately puts scratchpad updates, leader changes, and agent_left on the SSE stream (so picobot gets signalled about them and the CLI sees joins/leaves).
13. **Sequence every envelope** (including joins/leaves/scratchpad) so clients can hold a single high-water mark and dedupe everywhere.
14. **Bridge/CLI parse hardening**: proper SSE frame parsing (multi-line `data:`, CRLF, comments), handle `id:`/`retry:` fields, and make the CLI drain its mailbox on reconnect.

### Phase 3 — Security + API semantics · ~2-3 days

15. **Kill auth-middleware auto-create.** Sessions are created only by `POST /sessions` or by an explicit `POST /register` with a `create: true` flag; `auth`/`authQuery` validate only. After a server restart, a re-register with the original PSK fails closed (which is correct — see Phase 4 for why restarts stop happening).
16. **Stop putting PSKs in URLs**: issue a short-lived watch token from `register` (random, session-scoped, TTL) and accept it on `/watch`; keep header auth everywhere else.
17. **Server-side mailbox long-poll**: `GET /mailbox?wait=30&from=X&type=Y` — atomically filter + drain matching messages, hold the request until timeout. This gives `wait_for_message` one efficient, loss-free primitive and removes the 2s polling loop.
18. Rate-limit `/watch` connection attempts per session; cap sessions per PSK; add `GET /healthz`.

### Phase 4 — Persistence & operations · ~3-5 days

19. **SQLite (or bbolt) persistence** for sessions, mailboxes, history, scratchpad. This is what makes reconnects meaningful: a restart becomes a bump, not a lobotomy. File store can stay in-memory initially (document the limit), or move blobs to disk under a data dir.
20. **Bridge resilience**: reset `initialized` on 401/403/connection failures and re-register with capabilities; stop holding `b.mu` across HTTP round trips (per-request contexts instead); surface `pendingSignal` info in `receive_messages` results; handle MCP `ping`; run tool handlers in goroutines with a mutex-protected writer so a 600s `wait_for_message` doesn't freeze the bridge; fix `Result` `omitempty`.
21. **CLI**: heartbeat, backoff, mailbox drain on reconnect.

### Phase 5 — Tests, CI, hygiene · ongoing

22. Port the two race repros into permanent tests (`hub` concurrency, `watcher` unsubscribe-under-notify). Add: SSE integration test via `httptest` (connect, receive, kill connection, reconnect, verify no gap via sequences), presence-expiry mailbox-retention test, end-to-end test (two bridges + server in-process: A sends → B's watcher signals → B drains exactly once), long-poll test.
23. CI: `go vet`, `golangci-lint`, `go test -race ./...` on PRs. Add `make lint`.
24. Clean the repo: `.gitignore` the built binaries (`cli`, `main`, `agentchat-mcp-bridge`), delete the stray `tmp` file, refresh README (env vars, TTL behavior, architecture diagram with the SSE/signal path, and an honest description of delivery guarantees).

### Target architecture (unchanged in shape, fixed in wiring)

```
hub.emit(env) ──► [seq + history] ──► mailbox fan-out (all agents, expiry-proof)
                       └────────────► SSE dispatcher (keepalives, seq-tagged, backpressure)
                                            ▲
bridge: supervised SSE (backoff+watchdog) ──┘   on event OR reconnect-with-unread → signal picobot (retried)
bridge tools: mailbox long-poll + holdback queue (no loss, no dupes)
```

### Acceptance criteria

- `go test -race ./...` clean, including the two repro tests under sustained concurrency.
- Kill -9 the server mid-conversation: restart it, and every bridge re-registers, redelivers nothing twice, misses nothing that was queued (SQLite), and picobot gets re-signalled for its backlog.
- Unplug the network for 5 minutes on an active SSE stream: on restore, the bridge reconnects on its own, re-signals for missed messages, and `receive_messages` returns each message exactly once.
- An agent idle for 10 minutes can still be DM'd; the message waits in its mailbox; `list_agents` shows it offline.
- A `wait_for_message(from=A)` during which B sends never loses B's message.

---

## 5. Defect index (file:line)

| # | Severity | Defect | Location |
|---|----------|--------|----------|
| 1 | P0 fatal | Unsynchronized `seqNums` map → process crash | `internal/hub/hub.go:68` |
| 2 | P0 | Send-on-closed-channel + map race in watcher | `internal/api/watcher.go:35-60` |
| 3 | P0 | Mailbox deleted on presence expiry | `internal/hub/hub.go:125` |
| 4 | P0 | Sends to expired agents fail; broadcasts skip them | `internal/hub/hub.go:138,177-184` |
| 5 | P0 | No heartbeat unless picobot socket configured | `cmd/agentchat-mcp-bridge/watcher.go:36-39` |
| 6 | P0 | No SSE keepalive → silent stream death, no reconnect | `internal/api/api.go:567-622` |
| 7 | P0 | No re-signal for messages missed while disconnected / signal-send failures | `watcher.go:132-174,322-328` |
| 8 | P1 | `drainAll` never advances `lastSeq` → stale duplicates | `cmd/agentchat-mcp-bridge/main.go:195-263` |
| 9 | P1 | `wait_for_message` destructively discards non-matches | `main.go:406-408` |
| 10 | P1 | Auth auto-creates sessions with attacker-supplied PSK | `internal/api/api.go:84-98,121-129` |
| 11 | P1 | PSK in query string on `/watch` | `api.go:68,177` (bridge) |
| 12 | P1 | All state in-memory; restart wipes + allows session hijack | server-wide |
| 13 | P1 | `GetHistoryAfter` iterates history slice unlocked | `internal/hub/hub.go:305-312` |
| 14 | P1 | Scratchpad/leader/agent_left never reach SSE (split fan-out) | `api.go:261,307,340` vs `hub.go` 9 sites |
| 15 | P2 | `presence.NewTracker` ignores `ttl` arg | `internal/presence/presence.go:28-37` |
| 16 | P2 | Bridge never re-registers after server restart (`initialized` sticky) | `main.go:102-126` |
| 17 | P2 | MCP: serial request handling; no `ping`; `Result` omitempty | `internal/mcp/server.go` |
| 18 | P2 | Bridge serializes all HTTP behind one mutex | `main.go:128-137` |
| 19 | P2 | CLI: no heartbeat, no SSE backoff, no mailbox drain | `cmd/agentchat-cli/main.go` |
| 20 | P2 | History cap 100/session breaks catch-up | `internal/hub/hub.go:19` |
| 21 | P2 | SSE subscribe-after-history event gap | `internal/api/api.go:589-599` |
| 22 | P2 | Random agent ordering in `list_agents`; dead `pendingSignal` code; 1KB signal read; README/repo hygiene | various |
