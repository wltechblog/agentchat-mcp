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
- **Presence with reconnect handling** — 30-second grace period suppresses join/leave noise from flaky connections; reconnects are tagged as `agent_reconnected` instead of join+leave
- **Shared scratchpad** — Key-value store per session for shared context, with real-time update broadcasts
- **Leader election** — First agent in a session becomes leader; supports explicit transfer and auto-transfer on disconnect
- **Sequenced message history** — Messages get monotonically increasing sequence numbers; agents can request missed messages after reconnect
- **Agent capabilities** — Agents declare capabilities on join; visible to all session members
- **Task delegation** — Built-in message types for assigning, tracking, and returning task results
- **REST API** — Session CRUD, agent listing, history, leader, and scratchpad inspection via HTTP

## Message Protocol

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

## Client / Agent Guide

### 1. Create a Session

Any agent (or orchestrator) creates a session via the REST API:

```bash
curl -X POST https://agentchat.example.com/sessions \
  -H "Content-Type: application/json" \
  -d '{"name": "project-alpha"}'
```

Response:

```json
{
  "id": "ae83c8880de8ed8178e6a2820e41f170",
  "name": "project-alpha",
  "psk": "d642faa407191ab4e74ae31b044846e704c5d5cc59e18de2ed1917b98294463d",
  "created_at": "2026-04-15T19:20:15.396758839Z"
}
```

Save the `id` and `psk` — you will need them to connect agents.

### 2. Connect via WebSocket

Open a WebSocket to `/ws` and send an `auth` message as the first message:

```json
{
  "type": "auth",
  "payload": {
    "session_id": "ae83c8880de8ed8178e6a2820e41f170",
    "agent_id": "research-agent",
    "agent_name": "Research Agent",
    "psk": "d642faa407191ab4e74ae31b044846e704c5d5cc59e18de2ed1917b98294463d",
    "capabilities": ["web_search", "summarize"]
  },
  "timestamp": "2026-04-15T19:21:00Z"
}
```

On success you receive:

```json
{
  "type": "auth_ok",
  "session_id": "ae83c8880de8ed8178e6a2820e41f170",
  "from": "server",
  "to": "research-agent",
  "payload": {"leader_id": "research-agent"},
  "timestamp": "2026-04-15T19:21:00Z"
}
```

If auth fails the connection is closed with an `error` message.

### 3. Send Messages

**Direct message** to a specific agent:

```json
{
  "type": "message",
  "to": "writer-agent",
  "payload": {"text": "Please write a summary of the research findings."}
}
```

**Broadcast** to all agents in the session:

```json
{
  "type": "broadcast",
  "payload": {"text": "Starting new research cycle."}
}
```

### 4. Use the Scratchpad (Shared State)

**Set** a key:

```json
{
  "type": "scratchpad_set",
  "payload": {"key": "research_plan", "value": ["step 1: search", "step 2: analyze", "step 3: report"]}
}
```

Other agents in the session receive a `scratchpad_update` automatically.

**Get** a key:

```json
{
  "type": "scratchpad_get",
  "payload": {"key": "research_plan"}
}
```

**Delete** a key:

```json
{
  "type": "scratchpad_delete",
  "payload": {"key": "research_plan"}
}
```

**List** all keys:

```json
{
  "type": "scratchpad_list"
}
```

### 5. Leader Election

The first agent to join a session automatically becomes the leader. The leader can transfer leadership:

```json
{
  "type": "leader_transfer",
  "payload": {"new_leader_id": "writer-agent"}
}
```

Any agent can query the current leader:

```json
{"type": "leader_query"}
```

If the leader disconnects, leadership is automatically transferred to the next available agent.

### 6. Assign and Track Tasks

**Assign a task:**

```json
{
  "type": "task_assign",
  "to": "research-agent",
  "payload": {
    "task_id": "task-001",
    "description": "Search for recent papers on LLM agents",
    "parameters": {"max_results": 10}
  }
}
```

**Update task status:**

```json
{
  "type": "task_status",
  "to": "coordinator",
  "payload": {
    "task_id": "task-001",
    "status": "in_progress",
    "detail": "Found 8 papers so far"
  }
}
```

**Return task result:**

```json
{
  "type": "task_result",
  "to": "coordinator",
  "payload": {
    "task_id": "task-001",
    "result": {"papers": ["..."], "summary": "..."}
  }
}
```

### 7. Reconnect and Catch Up

If an agent disconnects and reconnects within the grace period (30 seconds), it is tagged as `agent_reconnected` — no join/leave noise is broadcast.

On reconnect, request missed messages by sequence number:

```json
{
  "type": "history_request",
  "payload": {"after_sequence": 42, "limit": 50}
}
```

Response:

```json
{
  "type": "history_result",
  "payload": {"messages": [...], "count": 5}
}
```

### Example: Python Client (websockets library)

```python
import json
import asyncio
import websockets

SERVER = "wss://agentchat.example.com/ws"
SESSION_ID = "your-session-id"
PSK = "your-psk"

async def run():
    async with websockets.connect(SERVER) as ws:
        # Auth
        await ws.send(json.dumps({
            "type": "auth",
            "payload": {
                "session_id": SESSION_ID,
                "agent_id": "my-agent",
                "agent_name": "My Agent",
                "psk": PSK,
                "capabilities": ["search", "analyze"]
            },
            "timestamp": "2026-04-15T19:21:00Z"
        }))
        auth_resp = json.loads(await ws.recv())
        print("Auth:", auth_resp["type"])

        # Listen for messages
        async for raw in ws:
            env = json.loads(raw)
            print(f"[{env['type']}] from={env.get('from')} payload={env.get('payload')}")

            if env["type"] == "message" and env["to"] == "my-agent":
                # Handle direct message
                reply = json.dumps({
                    "type": "message",
                    "to": env["from"],
                    "payload": {"text": "Got your message!"}
                })
                await ws.send(reply)

asyncio.run(run())
```

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
├── cmd/server/main.go          # Server entrypoint
├── internal/
│   ├── api/api.go              # REST + WebSocket handlers
│   ├── auth/auth.go            # PSK generation
│   ├── hub/hub.go              # Connection registry, routing, presence
│   ├── leader/leader.go        # Leader election tracking
│   ├── protocol/message.go     # Message types and envelope
│   ├── scratchpad/scratchpad.go # Per-session key-value store
│   └── session/session.go      # Session CRUD + PSK validation
├── Dockerfile
├── docker-compose.yml
├── Makefile
└── go.mod
```
