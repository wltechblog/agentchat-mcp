# agentchat-mcp

A real-time communication server for multiple MCP-enabled agents to collaborate within authenticated sessions. Agents communicate via a REST API with server-side mailboxes, authenticate with a pre-shared key (PSK), and can exchange messages, share state, delegate tasks, and coordinate through a leader election system.

## Architecture

```
┌──────────┐       ┌──────────────────────┐       ┌──────────┐
│  Agent A │◄─REST─►│                      │◄─REST─►│  Agent B │
│ (MCP)    │       │   agentchat-server   │       │ (MCP)    │
└──────────┘       │                      │       └──────────┘
                    │  - Session mgmt      │
┌──────────┐       │  - PSK auth          │       ┌──────────┐
│  Agent C │◄─REST─►│  - Mailbox routing   │◄─REST─►│  Agent D │
│ (MCP)    │       │  - Shared scratchpad │       │ (MCP)    │
└──────────┘       │  - Leader election   │       └──────────┘
                    └──────────────────────┘
                               ▲
                               │ HTTP (Caddy reverse proxy)
                               │
                      ┌────────────────┐
                      │  Caddy Server  │
                      │  (TLS, routing)│
                      └────────────────┘
```

## Features

- **Session-based isolation** — Agents join named sessions, each with a unique PSK
- **Server-side mailboxes** — Every message for an agent is queued in their mailbox regardless of connection state. Agents poll to drain messages (destructive read).
- **Lazy connection** — The MCP bridge doesn't contact the server until a tool is actually invoked. Spawned-but-unused processes consume zero server resources.
- **Multiple process tolerant** — Multiple MCP bridge instances for the same agent work correctly. First poll wins (competing consumer semantics). No duplicate delivery.
- **Activity-based presence** — Any authenticated request refreshes agent presence. Agents expire after 60s of inactivity, with configurable TTL.
- **Real-time messaging** — Direct messages (agent-to-agent) and broadcasts (to all session members)
- **Shared scratchpad** — Key-value store per session for shared context, with real-time update broadcasts to mailboxes
- **Leader election** — First agent in a session becomes leader; supports explicit transfer and auto-transfer on expiry
- **Sequenced message history** — Messages get monotonically increasing sequence numbers; agents can request missed messages
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

The bridge exposes MCP tools over stdio and communicates with the server via REST. It does **not** connect to the server on startup — the first tool call triggers a registration request. Spawned-but-unused bridge processes consume zero server resources.

```
┌───────────────────┐   MCP (stdio)   ┌──────────────────────┐    HTTP/REST    ┌──────────┐
│  MCP Host         │◄───────────────►│  agentchat-mcp-bridge│◄───────────────►│  agentchat│
│  (Claude, Cursor) │                 │  (Go binary)         │                 │  -server  │
└───────────────────┘                 └──────────────────────┘                 └──────────┘
```

Messages destined for the agent are queued in a server-side mailbox. `receive_messages` and `wait_for_message` drain the mailbox via `GET /sessions/{id}/mailbox`.

### Using the tools

Once configured, agents can use these MCP tools to communicate:

| Tool | Description |
|------|-------------|
| `send_message` | Send a direct message to another agent (remote agent may take time to respond) |
| `broadcast` | Broadcast a message to all agents in the session |
| `receive_messages` | Drain mailbox — retrieve all queued incoming messages (returns immediately) |
| `wait_for_message` | Poll mailbox until a matching message arrives, with optional filters (`type`, `from`) and timeout |
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
| `GET` | `/sessions` | List all sessions |
| `GET` | `/sessions/{id}` | Get session details |
| `DELETE` | `/sessions/{id}` | Delete session and disconnect all agents |

### Agent-authenticated endpoints

All require `Authorization: Bearer <psk>` and `X-Agent-ID: <agent_id>` headers.

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/sessions/{id}/register` | Register agent presence / heartbeat. Body: `{"agent_name": "...", "capabilities": [...]}` |
| `POST` | `/sessions/{id}/messages` | Send a direct message. Body: `{"to": "...", "type": "message", "payload": {...}}` |
| `POST` | `/sessions/{id}/broadcast` | Broadcast to session. Body: `{"type": "broadcast", "payload": {...}}` |
| `GET` | `/sessions/{id}/mailbox` | Drain mailbox — destructive read of all queued messages |
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

# Run tests (with race detector)
go test -race ./...

# Docker build
make docker
```

## Project Structure

```
agentchat-mcp/
├── cmd/
│   ├── server/main.go                  # Server entrypoint
│   └── agentchat-mcp-bridge/main.go    # MCP bridge (stdio → REST)
├── internal/
│   ├── api/api.go                      # REST handlers + auth middleware
│   ├── auth/auth.go                    # PSK generation
│   ├── filestore/store.go              # In-memory per-session file storage
│   ├── hub/hub.go                      # Business logic, mailbox routing, presence
│   ├── leader/leader.go                # Leader election tracking
│   ├── mailbox/mailbox.go              # Per-agent message queue (destructive reads)
│   ├── mcp/server.go                   # MCP JSON-RPC protocol server
│   ├── presence/presence.go            # Activity-based agent presence with TTL
│   ├── protocol/message.go             # Message types and envelope
│   ├── scratchpad/scratchpad.go        # Per-session key-value store
│   └── session/session.go              # Session CRUD + PSK validation
├── Dockerfile
├── docker-compose.yml
├── Makefile
└── go.mod
```
