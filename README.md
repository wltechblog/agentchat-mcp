# agentchat-mcp

A real-time communication server for multiple MCP-enabled agents to collaborate within authenticated sessions. Agents communicate via a REST API with server-side mailboxes, authenticate with a pre-shared key (PSK), and can exchange messages, share state, delegate tasks, and coordinate through a leader election system.

## Architecture

```
┌──────────┐   REST + SSE   ┌──────────────────────┐   REST + SSE   ┌──────────┐
│  Agent A │◄──────────────►│                      │◄──────────────►│  Agent B │
│ (MCP)    │                │  agentchat-server    │                │ (MCP)    │
└──────────┘                │  - Session mgmt      │                └──────────┘
                            │  - PSK auth          │
┌──────────┐   REST + SSE   │  - Mailbox routing   │   REST + SSE   ┌──────────┐
│  Agent C │◄──────────────►│  - Event fan-out     │◄──────────────►│  Agent D │
│ (MCP)    │                │  - Leader election   │                │  (CLI)   │
└──────────┘                └──────────────────────┘                └──────────┘
        ▲                                                    ▲
        │ signal (local Unix socket)                         │ SSE stream
        │                                                    │
┌───────┴─────────┐                                   ┌──────┴───────┐
│ picobot agent   │                                   │ human        │
│ (woken on       │                                   │ (watchLoop)  │
│ incoming mail)  │                                   └──────────────┘
└─────────────────┘
```

Every event (direct messages, broadcasts, scratchpad updates, leader
changes, joins/leaves) is emitted once by the hub and fanned out to both
server-side mailboxes and the SSE watch stream, carrying a monotonically
increasing per-session sequence number.

## Features

- **Session-based isolation** — Agents join named sessions, each with a unique PSK
- **Server-side mailboxes** — Every message for an agent is queued in their mailbox regardless of connection state, including while the agent is offline. Agents poll to drain messages (destructive read).
- **Reliable event stream** — The server's SSE `/watch` endpoint carries every session event (direct messages, broadcasts, scratchpad updates, leader changes, agent joins/leaves), each with a monotonically increasing sequence number. Keepalive pings keep idle streams alive through proxies; clients reconnect with backoff.
- **Retried wake-up signals** — When a message arrives for a hosted agent (joist / gino / picobot), the bridge signals it via the local Unix socket; failed signal sends retry with capped exponential backoff and the agent is re-signalled on every stream reconnect, so a missed wake-up is never final.
- **Self-declared signals** — The bridge declares its `check_messages` signal in the MCP `initialize` result (`signals.actions`), so joist/gino hosts auto-register the action with no config. The signal source is the host-injected MCP config key (`JOIST_MCP_ID` / `GINO_MCP_ID`), satisfying the registry's source-bound enforcement.
- **Chat-session routing** — Agents can be members of more than one chat. The bridge captures the calling chat session from `tools/call` `_meta` origin (stamped by joist/gino per-turn) and stamps it into every wake-up signal (`channel` + `chat_id`), so a signal wakes the agent in the exact chat session that invoked the bridge — not a global default.
- **Multiple process tolerant** — Multiple MCP bridge instances for the same agent work correctly. First poll wins (competing consumer semantics). No duplicate delivery.
- **Offline-tolerant presence** — Any authenticated request refreshes agent presence. Agents that go idle past the TTL (60s) stay listed as `online: false` and keep receiving mail, so nothing is lost while they're away.
- **Real-time messaging** — Direct messages (agent-to-agent) and broadcasts (to all session members)
- **Shared scratchpad** — Key-value store per session for shared context, with real-time update broadcasts to mailboxes
- **Leader election** — First agent in a session becomes leader; supports explicit transfer and auto-transfer on expiry
- **Sequenced message history** — Messages get monotonically increasing sequence numbers; agents can request missed messages
- **Crash-safe persistence (opt-in)** — Set `AGENTCHAT_DATA` to a directory and the server snapshots sessions, mailboxes, history, sequence counters, and the scratchpad every few seconds (plus on clean shutdown). A restart becomes a bump: agents re-register, queued mail survives, and no message is delivered twice. Files are not persisted.
- **Agent capabilities** — Agents declare capabilities on registration; visible to all session members
- **Task delegation** — Built-in message types for assigning, tracking, and returning task results
- **File transfer** — Upload files via REST, share file IDs via messages, download via REST
- **Auto-create sessions** — If an agent registers with an unknown session ID and PSK, the session is created automatically
- **MCP bridge** — Standalone binary bridges any MCP host (Claude Desktop, Cursor, opencode) to the server via stdio↔REST
- **Fully REST API** — All operations use HTTP. Session CRUD, messaging, mailbox polling, scratchpad, leader, history, and files

---

## Server Deployment

### Quick Start (Binary)

```bash
# Build
make build

# Run (listens on :8080 by default)
./bin/agentchat-server

# Custom port
PORT=3000 ./bin/agentchat-server
```

### Docker

```bash
# Build image
make docker

# Run with docker compose
make docker-up

# Stop
make docker-down
```

The container listens on port 8080 internally. The `PORT` environment variable in `docker-compose.yml` can be changed to suit your environment.

### Caddy Reverse Proxy

The server is designed to run behind Caddy for TLS termination. Example `Caddyfile`:

```caddyfile
agentchat.example.com {
    reverse_proxy agentchat:8080
}
```

For a full `docker-compose.yml` with Caddy:

```yaml
services:
  agentchat:
    build: .
    environment:
      - PORT=8080
    restart: unless-stopped
    networks:
      - internal

  caddy:
    image: caddy:2
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile
      - caddy_data:/data
      - caddy_config:/config
    depends_on:
      - agentchat
    networks:
      - internal

networks:
  internal:

volumes:
  caddy_data:
  caddy_config:
```

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8080` | HTTP listen port |
| `AGENTCHAT_DEBUG` | `false` | Set to `true` or `1` to enable debug logging |
| `AGENTCHAT_DATA` | *(unset)* | Directory for the state snapshot. When set, sessions, mailboxes, history, sequence counters, and the scratchpad survive restarts; when unset, the server is purely in-memory |

---

## Quick Start: Connect Your Agents

Each agent runs as an MCP tool server via the `agentchat-mcp-bridge` binary. You configure it in your MCP host's config file (Claude Desktop, Cursor, opencode, etc.) with a session ID and PSK.

### Step 1: Build the bridge

```bash
go build -o bin/agentchat-mcp-bridge ./cmd/agentchat-mcp-bridge
```

Or just `make build-bridge`.

### Step 2: Create a session

Create a session on your server to get a session ID and PSK:

```bash
curl -X POST https://agentchat.example.com/sessions \
  -H "Content-Type: application/json" \
  -d '{"name": "my-project"}'
```

This returns:

```json
{
  "id": "ae83c8880de8ed8178e6a2820e41f170",
  "name": "my-project",
  "psk": "d642faa407191ab4e74ae31b044846e704c5d5cc59e18de2ed1917b98294463d",
  "created_at": "2026-04-15T19:20:15Z"
}
```

Save the `id` and `psk`. You can also skip this step — if you provide a new `session_id` and `psk` in the bridge config, the session will be created automatically on first tool use.

### Step 3: Configure your MCP hosts

Each agent gets its own entry in the MCP host config, all pointing to the same session. Use a unique `AGENTCHAT_AGENT_ID` for each.

**Claude Desktop** — add to `claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "researcher": {
      "command": "/path/to/agentchat-mcp-bridge",
      "env": {
        "AGENTCHAT_URL": "https://agentchat.example.com",
        "AGENTCHAT_SESSION_ID": "ae83c8880de8ed8178e6a2820e41f170",
        "AGENTCHAT_PSK": "d642faa407191ab4e74ae31b044846e704c5d5cc59e18de2ed1917b98294463d",
        "AGENTCHAT_AGENT_ID": "researcher",
        "AGENTCHAT_AGENT_NAME": "Research Agent",
        "AGENTCHAT_CAPABILITIES": "web_search,summarize"
      }
    },
    "writer": {
      "command": "/path/to/agentchat-mcp-bridge",
      "env": {
        "AGENTCHAT_URL": "https://agentchat.example.com",
        "AGENTCHAT_SESSION_ID": "ae83c8880de8ed8178e6a2820e41f170",
        "AGENTCHAT_PSK": "d642faa407191ab4e74ae31b044846e704c5d5cc59e18de2ed1917b98294463d",
        "AGENTCHAT_AGENT_ID": "writer",
        "AGENTCHAT_AGENT_NAME": "Writer Agent",
        "AGENTCHAT_CAPABILITIES": "write,edit"
      }
    }
  }
}
```

**Cursor** — add to `.cursor/mcp.json`:

```json
{
  "mcpServers": {
    "agentchat": {
      "command": "/path/to/agentchat-mcp-bridge",
      "env": {
        "AGENTCHAT_URL": "https://agentchat.example.com",
        "AGENTCHAT_SESSION_ID": "ae83c8880de8ed8178e6a2820e41f170",
        "AGENTCHAT_PSK": "d642faa407191ab4e74ae31b044846e704c5d5cc59e18de2ed1917b98294463d",
        "AGENTCHAT_AGENT_ID": "cursor-agent",
        "AGENTCHAT_CAPABILITIES": "code,debug"
      }
    }
  }
}
```

**opencode** — add to `.opencode/mcp.json`:

```json
{
  "mcpServers": {
    "agentchat": {
      "command": "/path/to/agentchat-mcp-bridge",
      "env": {
        "AGENTCHAT_URL": "https://agentchat.example.com",
        "AGENTCHAT_SESSION_ID": "ae83c8880de8ed8178e6a2820e41f170",
        "AGENTCHAT_PSK": "d642faa407191ab4e74ae31b044846e704c5d5cc59e18de2ed1917b98294463d",
        "AGENTCHAT_AGENT_ID": "opencode-agent",
        "AGENTCHAT_CAPABILITIES": "code,debug,review"
      }
    }
  }
}
```

### Bridge Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `AGENTCHAT_URL` | Yes | Server URL (e.g. `https://agentchat.example.com`). Legacy `wss://` URLs are auto-converted to `https://`. |
| `AGENTCHAT_SESSION_ID` | Yes | Session ID to join |
| `AGENTCHAT_PSK` | Yes | Pre-shared key for the session |
| `AGENTCHAT_AGENT_ID` | Yes | Unique agent ID for this bridge instance |
| `AGENTCHAT_AGENT_NAME` | No | Display name (defaults to agent ID) |
| `AGENTCHAT_CAPABILITIES` | No | Comma-separated capability list (e.g. `"search,analyze,write"`) |
| `AGENTCHAT_DEBUG` | No | Set to `true` or `1` to enable debug logging |

### How it works

The bridge exposes MCP tools over stdio and communicates with the server via REST. On startup it registers with the server and keeps its presence alive with a heartbeat every 30s, so the agent is visible and message-able even before its first tool call.

```
┌───────────────────┐   MCP (stdio)   ┌──────────────────────┐    HTTP/REST    ┌──────────┐
│  MCP Host         │◄───────────────►│  agentchat-mcp-bridge│◄───────────────►│  agentchat│
│  (Claude, Cursor) │                 │  (Go binary)         │                 │  -server  │
└───────────────────┘                 └──────────────────────┘                 └──────────┘
```

Messages destined for the agent are queued in a server-side mailbox, including while the agent is offline. `receive_messages` and `wait_for_message` drain the mailbox via `GET /sessions/{id}/mailbox`, optionally as a server-side filtered long-poll (`?wait=25&from=X`) that holds the request until a matching message arrives and never destroys non-matching mail.

When the bridge is spawned by a host agent, it also holds an SSE connection to `/watch` (authenticated with a short-lived token issued by register — PSKs never appear in URLs) and sends a `check_messages` signal to the host's local Unix socket whenever relevant mail arrives. Failed signals retry with capped exponential backoff, the agent is re-signalled on every reconnect, and the stream has keepalive pings plus an idle watchdog, so a silent network drop becomes a reconnect instead of a dead agent.

### Delivery guarantees

- **Mailboxes are at-least-once, exactly-once per drain.** Drains are destructive; multiple bridge instances for the same agent get competing-consumer semantics (first poll wins, no duplicates).
- **The watch stream is best-effort, the mailbox is authoritative.** Anything missed while a stream is down is picked up by the reconnect drain and wake-up signal.
- **One sequence counter per session.** Every event carries it; clients dedupe by sequence and hold a single high-water mark.
- **History is catch-up, not a store.** The last ~100 message-like events per session are available via `request_history`; system events (joins, scratchpad updates) are sequenced but not replayed in history.
- **Presence is ambient.** Joins, offline lapses, and departures appear only on the watch stream and in `list_agents` — they never enter mailboxes and never trigger wake-up signals, so agents aren't woken to report transitions nobody needs to act on. `agent_left` is announced only when an agent is forgotten entirely (24h offline), not when it merely goes idle.
- **Persistence (opt-in via `AGENTCHAT_DATA`)** loses at most one flush interval (5s) on a crash; a clean shutdown loses nothing. Files are not persisted.

### Using the tools

Once configured, agents can use these MCP tools to communicate:

| Tool | Description |
|------|-------------|
| `send_message` | Send a direct message to another agent (remote agent may take time to respond) |
| `broadcast` | Broadcast a message to all agents in the session |
| `receive_messages` | Drain mailbox — retrieve all queued incoming messages (returns immediately) |
| `wait_for_message` | Poll mailbox until a matching message arrives, with optional filters (`type`, `from`) and timeout. Messages that don't match are retained for the next drain, never discarded |
| `send_and_wait` | Send a message and poll until a reply arrives from the target agent |
| `list_agents` | List all active agents and their capabilities |
| `get_leader` | Get the current session leader |
| `transfer_leadership` | Transfer leadership to another agent |
| `scratchpad_set` | Set a key in the shared scratchpad |
| `scratchpad_get` | Get a value from the scratchpad |
| `scratchpad_delete` | Delete a key from the scratchpad |
| `scratchpad_list` | List all scratchpad entries |
| `task_assign` | Assign a task to another agent (remote agent may take minutes to complete) |
| `task_status` | Update a task's status |
| `task_result` | Return a completed task's result |
| `request_history` | Request message history (optionally after a sequence number) |
| `send_file` | Upload a file (base64 content) and share it with another agent |
| `download_file` | Download a file by ID, returns base64-encoded content |

**Typical workflow:**

1. Call `list_agents` to discover peers and their capabilities
2. Call `task_assign` to delegate work to a specific agent
3. Call `wait_for_message` to poll until a `task_result` or `task_status` response arrives (remote agents may take minutes)
4. Use the scratchpad to share intermediate state across all agents
5. Use `send_file` / `download_file` to exchange files

---

## REST API Reference

### Public endpoints

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/sessions` | Create a session. Body: `{"name": "..."}`. Returns session with PSK |
| `GET` | `/sessions` | List all sessions, including each session's agent roster |
| `GET` | `/sessions/{id}` | Get session details |
| `DELETE` | `/sessions/{id}` | Delete session and disconnect all agents |
| `GET` | `/healthz` | Unauthenticated liveness probe |

### Agent-authenticated endpoints

All require `Authorization: Bearer <psk>` and `X-Agent-ID: <agent_id>` headers.

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/sessions/{id}/register` | Register agent presence / heartbeat. Body: `{"capabilities": [...]}`. Auto-creates the session if unknown (besides `POST /sessions`, the only way to create one). Returns a short-lived `watch_token` for `/watch`, renewed on every register |
| `POST` | `/sessions/{id}/messages` | Send a direct message. Body: `{"to": "...", "type": "message", "payload": {...}}` |
| `POST` | `/sessions/{id}/broadcast` | Broadcast to session. Body: `{"type": "broadcast", "payload": {...}}` |
| `GET` | `/sessions/{id}/mailbox` | Drain mailbox — destructive read of all queued messages. Query params: `wait` (seconds, ≤30) holds the request until a match arrives; `from`/`type` filter the drain — non-matching messages stay queued |
| `GET` | `/sessions/{id}/agents` | List active agents |
| `GET` | `/sessions/{id}/leader` | Get current leader |
| `POST` | `/sessions/{id}/leader/transfer` | Transfer leadership. Body: `{"new_leader_id": "..."}` |
| `GET` | `/sessions/{id}/history` | Get message history. Query params: `after_sequence`, `limit` |
| `GET` | `/sessions/{id}/scratchpad` | List all scratchpad entries |
| `POST` | `/sessions/{id}/scratchpad/set` | Set a key. Body: `{"key": "...", "value": ...}` |
| `POST` | `/sessions/{id}/scratchpad/get` | Get a key. Body: `{"key": "..."}` |
| `POST` | `/sessions/{id}/scratchpad/delete` | Delete a key. Body: `{"key": "..."}` |
| `POST` | `/sessions/{id}/files` | Upload a file. Query params: `filename`. Headers: `Content-Type`. Returns `{file_id, size}` |
| `GET` | `/sessions/{id}/files` | List files |
| `GET` | `/sessions/{id}/files/{fileID}` | Download a file by ID |
| `DELETE` | `/sessions/{id}/files/{fileID}` | Delete a file |

## Development

```bash
# Build
make build

# Run locally
make run

# Lint (gofmt + vet) and run tests (with race detector)
make check

# Docker build
make docker
```

CI runs `make lint`, `go build ./...`, and `go test -race ./...` on every push and pull request (see `.github/workflows/ci.yml`).

## Project Structure

```
agentchat-mcp/
├── cmd/
│   ├── server/main.go                  # Server entrypoint (persistence wiring, graceful shutdown)
│   ├── agentchat-mcp-bridge/           # MCP bridge (stdio ↔ REST + SSE, picobot signals)
│   └── agentchat-cli/                  # Human chat client (SSE watch + mailbox drain)
├── internal/
│   ├── api/                            # REST handlers, auth middleware, SSE watch endpoint
│   │   ├── api.go                      #   routes, auth, mailbox long-poll
│   │   ├── watcher.go                  #   per-session SSE subscriber sets
│   │   └── watchtoken.go               #   short-lived watch tokens (PSKs stay out of URLs)
│   ├── auth/auth.go                    # PSK generation
│   ├── filestore/store.go              # In-memory per-session file storage
│   ├── hub/hub.go                      # Business logic: routing, history, single event fan-out
│   ├── leader/leader.go                # Leader election tracking
│   ├── mailbox/mailbox.go              # Per-agent queues: destructive drains, filtered long-poll
│   ├── mcp/server.go                   # MCP JSON-RPC server (concurrent request dispatch)
│   ├── persist/persist.go              # Opt-in crash-safe state snapshots (AGENTCHAT_DATA)
│   ├── presence/presence.go            # Activity-based presence with TTL; offline ≠ unreachable
│   ├── protocol/message.go             # Message types and envelope
│   ├── scratchpad/scratchpad.go        # Per-session key-value store
│   ├── session/session.go              # Session CRUD + PSK validation
│   └── signal/signal.go                # Action-based signals to picobot's Unix socket
├── Dockerfile
├── docker-compose.yml
├── Makefile
└── go.mod
```
