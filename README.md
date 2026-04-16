# agentchat-mcp

A real-time communication server for multiple MCP-enabled agents to collaborate within authenticated sessions. Agents connect via WebSocket, authenticate with a pre-shared key (PSK), and can exchange messages, share state, delegate tasks, and coordinate through a leader election system.

## Architecture

```
┌──────────┐       ┌──────────────────────┐       ┌──────────┐
│  Agent A │◄─WS──►│                      │◄─WS──►│  Agent B │
│ (MCP)    │       │   agentchat-server   │       │ (MCP)    │
└──────────┘       │                      │       └──────────┘
                    │  - Session mgmt      │
┌──────────┐       │  - PSK auth          │       ┌──────────┐
│  Agent C │◄─WS──►│  - Message routing   │◄─WS──►│  Agent D │
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
- **Real-time messaging** — Direct messages (agent-to-agent) and broadcasts (to all session members)
- **Presence with reconnect handling** — 30-second grace period suppresses join/leave noise from flaky connections; reconnects are tagged as `agent_reconnected`. Messages sent to a disconnected agent are buffered and delivered on reconnect.
- **Shared scratchpad** — Key-value store per session for shared context, with real-time update broadcasts
- **Leader election** — First agent in a session becomes leader; supports explicit transfer and auto-transfer on disconnect
- **Sequenced message history** — Messages get monotonically increasing sequence numbers; agents can request missed messages after reconnect
- **Agent capabilities** — Agents declare capabilities on join; visible to all session members
- **Task delegation** — Built-in message types for assigning, tracking, and returning task results
- **File transfer** — Upload files via REST, share file IDs via WebSocket, download via REST
- **Auto-create sessions** — If an agent connects with an unknown session ID and PSK, the session is created automatically
- **MCP bridge with auto-reconnect** — Standalone binary bridges any MCP host (Claude Desktop, Cursor, opencode) to the server via stdio↔WebSocket, with exponential backoff reconnection
- **REST API** — Session CRUD, agent listing, history, leader, scratchpad inspection, and file upload/download via HTTP

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

Save the `id` and `psk`. You can also skip this step — if you provide a new `session_id` and `psk` in the bridge config, the session will be created automatically on first connection.

### Step 3: Configure your MCP hosts

Each agent gets its own entry in the MCP host config, all pointing to the same session. Use a unique `AGENTCHAT_AGENT_ID` for each.

**Claude Desktop** — add to `claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "researcher": {
      "command": "/path/to/agentchat-mcp-bridge",
      "env": {
        "AGENTCHAT_URL": "wss://agentchat.example.com/ws",
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
        "AGENTCHAT_URL": "wss://agentchat.example.com/ws",
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
        "AGENTCHAT_URL": "wss://agentchat.example.com/ws",
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
        "AGENTCHAT_URL": "wss://agentchat.example.com/ws",
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
| `AGENTCHAT_URL` | Yes | WebSocket URL of the server (e.g. `wss://agentchat.example.com/ws`) |
| `AGENTCHAT_SESSION_ID` | Yes | Session ID to join |
| `AGENTCHAT_PSK` | Yes | Pre-shared key for the session |
| `AGENTCHAT_AGENT_ID` | Yes | Unique agent ID for this bridge instance |
| `AGENTCHAT_AGENT_NAME` | No | Display name (defaults to agent ID) |
| `AGENTCHAT_CAPABILITIES` | No | Comma-separated capability list (e.g. `"search,analyze,write"`) |

### How it works

The bridge connects to the server over WebSocket and exposes MCP tools over stdio. Your MCP host calls these tools to interact with other agents in the session. The bridge auto-reconnects with exponential backoff if the connection drops.

```
┌───────────────────┐   MCP (stdio)   ┌──────────────────────┐   WebSocket   ┌──────────┐
│  MCP Host         │◄───────────────►│  agentchat-mcp-bridge│◄────────────►│  agentchat│
│  (Claude, Cursor) │                 │  (Go binary)         │              │  -server  │
└───────────────────┘                 └──────────────────────┘              └──────────┘
```

### Using the tools

Once configured, agents can use these MCP tools to communicate:

| Tool | Description |
|------|-------------|
| `send_message` | Send a direct message to another agent (remote agent may take time to respond) |
| `broadcast` | Broadcast a message to all agents in the session |
| `receive_messages` | Retrieve queued incoming messages since last call (returns immediately) |
| `wait_for_message` | Block until a message arrives, with optional filters (`type`, `from`) and timeout. Use this instead of polling when waiting for slow remote agents. |
| `list_agents` | List all connected agents and their capabilities |
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
3. Call `wait_for_message` to block until a `task_result` or `task_status` response arrives (remote agents may take minutes)
4. Use the scratchpad to share intermediate state across all agents
5. Use `send_file` / `download_file` to exchange files

---

## Message Protocol (for custom clients)

All WebSocket messages are JSON envelopes:

```json
{
  "type": "<message_type>",
  "session_id": "<session_id>",
  "from": "<agent_id>",
  "to": "<agent_id>",
  "payload": { ... },
  "sequence": 1,
  "timestamp": "2026-04-15T12:00:00Z"
}
```

### Message Types

| Type | Direction | Description |
|------|-----------|-------------|
| `auth` | Client → Server | First message on connect. Includes `session_id`, `agent_id`, `agent_name`, `psk`, `capabilities` |
| `auth_ok` | Server → Client | Successful auth. Payload includes `leader_id` |
| `error` | Server → Client | Error response |
| `message` | Client → Client | Direct message. Requires `to` field |
| `broadcast` | Client → Session | Broadcast to all agents in session (excluding sender) |
| `agent_joined` | Server → Session | Notification that a new agent joined. Payload: `{agent_id, agent_name, capabilities}` |
| `agent_left` | Server → Session | Notification that an agent left after grace period |
| `agent_reconnected` | Server → Session | Notification that a disconnected agent reconnected |
| `list_agents` | Client → Server | Request list of agents in session |
| `agents_list` | Server → Client | Response with array of agent info |
| `scratchpad_set` | Client → Server | Set a key. Payload: `{key, value}`. Other agents get `scratchpad_update` |
| `scratchpad_get` | Client → Server | Get a key. Payload: `{key}`. Response: `scratchpad_result` |
| `scratchpad_delete` | Client → Server | Delete a key. Other agents get `scratchpad_update` |
| `scratchpad_list` | Client → Server | List all keys. Response: `scratchpad_result` with `{entries: [...]}` |
| `scratchpad_result` | Server → Client | Response to get/set/delete/list operations |
| `scratchpad_update` | Server → Session | Broadcast when a key is set or deleted |
| `leader_query` | Client → Server | Ask who the current leader is |
| `leader_info` | Server → Client/Session | Leader identity. Broadcast on transfer |
| `leader_transfer` | Client → Server | Transfer leadership. Only current leader can do this. Payload: `{new_leader_id}` |
| `history_request` | Client → Server | Request message history. Payload: `{after_sequence, limit}` |
| `history_result` | Server → Client | Array of past messages |
| `task_assign` | Client → Client | Assign a task. Requires `to`. Payload: `{task_id, description, parameters}` |
| `task_status` | Client → Client | Update task status. Requires `to`. Payload: `{task_id, status, detail}` |
| `task_result` | Client → Client | Return task result. Requires `to`. Payload: `{task_id, result}` |
| `file_share` | Client → Client | Share an uploaded file. Requires `to`. Payload: `{file_id, file_name, content_type, size, description}` |

---

## REST API Reference

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/sessions` | Create a session. Body: `{"name": "..."}`. Returns session with PSK |
| `GET` | `/sessions` | List all sessions |
| `GET` | `/sessions/{id}` | Get session details |
| `DELETE` | `/sessions/{id}` | Delete session and disconnect all agents |
| `GET` | `/sessions/{id}/agents` | List connected agents |
| `GET` | `/sessions/{id}/history` | Get message history |
| `GET` | `/sessions/{id}/leader` | Get current leader |
| `GET` | `/sessions/{id}/scratchpad` | List all scratchpad entries |
| `POST` | `/sessions/{id}/files` | Upload a file. Query params: `filename`. Headers: `Content-Type`, `X-Agent-ID`. Returns `{file_id, size}` |
| `GET` | `/sessions/{id}/files/{fileID}` | Download a file by ID |
| `DELETE` | `/sessions/{id}/files/{fileID}` | Delete a file |
| `GET` | `/ws` | WebSocket upgrade endpoint |

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
│   └── agentchat-mcp-bridge/main.go    # MCP bridge (stdio → WebSocket)
├── internal/
│   ├── api/api.go                      # REST + WebSocket handlers
│   ├── auth/auth.go                    # PSK generation
│   ├── filestore/store.go              # In-memory per-session file storage
│   ├── hub/hub.go                      # Connection registry, routing, presence
│   ├── leader/leader.go                # Leader election tracking
│   ├── mcp/server.go                   # MCP JSON-RPC protocol server
│   ├── protocol/message.go             # Message types and envelope
│   ├── scratchpad/scratchpad.go        # Per-session key-value store
│   └── session/session.go              # Session CRUD + PSK validation
├── Dockerfile
├── docker-compose.yml
├── Makefile
└── go.mod
```
