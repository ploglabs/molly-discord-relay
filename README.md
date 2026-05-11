# Molly Discord Relay

Realtime Discord relay backend for terminal-native developer collaboration.

Molly Relay connects Discord to terminal clients using WebSockets and REST APIs.
It powers realtime chat, developer presence, activity tracking, and message synchronization for terminal-first collaboration platforms.

---

# Features

* Realtime Discord Gateway integration
* WebSocket event broadcasting
* Discord message relay
* Terminal client support
* Developer presence system
* Live activity/status updates
* SQLite persistence
* Message history APIs
* Typing indicators
* Structured realtime event system
* Automatic reconnect handling
* Concurrent websocket sessions
* Production-ready Go architecture

---

# Architecture

```text
Terminal Client
      │
      ▼
WebSocket / REST API
      │
      ▼
Molly Relay Server
      │
      ▼
Discord Gateway + Discord API
      │
      ▼
Discord Channels
```

---

# Tech Stack

* Go
* DiscordGo
* Gorilla WebSocket
* SQLite
* Chi Router
* godotenv

---

# Project Structure

```text
cmd/
internal/
  api/
  discord/
  websocket/
  presence/
  storage/
  models/
pkg/
```

---

# Installation

## Clone Repository

```bash
git clone https://github.com/yourusername/molly-relay.git
cd molly-relay
```

---

## Install Dependencies

```bash
go mod tidy
```

---

# Discord Bot Setup

## Step 1 — Create Discord Application

Open:

[Discord Developer Portal](https://discord.com/developers/applications?utm_source=chatgpt.com)

Create a new application.

---

## Step 2 — Create Bot

* Open the application
* Navigate to:

  * Bot
* Click:

  * Add Bot

---

## Step 3 — Enable Privileged Intents

Enable:

* Message Content Intent
* Server Members Intent
* Presence Intent (optional)

These are required for:

* reading messages
* presence tracking
* activity updates

---

## Step 4 — Invite Bot

Go to:

* OAuth2
* URL Generator

Select:

* bot

Permissions:

* Read Messages/View Channels
* Send Messages
* Read Message History
* Add Reactions

Open generated URL and invite bot to your server.

---

# Environment Variables

Create a `.env` file:

```env
DISCORD_TOKEN=your_discord_bot_token
PORT=8080
DATABASE_PATH=./molly.db
API_KEY=optional_secret_key
```

---

# Running Molly Relay

```bash
go run cmd/main.go
```

Server starts on:

```text
http://localhost:8080
```

---

# WebSocket Endpoint

```text
/ws
```

Realtime events are pushed to connected clients.

---

# Example WebSocket Event

```json
{
  "type": "message_create",
  "channel": "general",
  "username": "arnav",
  "content": "hello world",
  "timestamp": "2026-05-11T10:00:00Z"
}
```

---

# REST APIs

## Send Message

```http
POST /message
```

Example:

```json
{
  "channel": "general",
  "username": "arnav",
  "content": "hello from terminal"
}
```

---

## Fetch History

```http
GET /history?channel=general&limit=100
```

---

## Update Status

```http
POST /status
```

Example:

```json
{
  "username": "arnav",
  "status": "Building websocket relay"
}
```

---

# Presence System

Molly includes a realtime developer presence system.

Users can set statuses like:

```text
Building relay server
Fixing websocket auth
Training ML model
Debugging CUDA kernel
```

Connected clients receive updates instantly.

---

# SQLite Persistence

SQLite stores:

* cached messages
* users
* statuses
* channels
* presence state

Database file:

```text
molly.db
```

---

# Example Schema

```sql
users(
  id TEXT PRIMARY KEY,
  username TEXT,
  online BOOLEAN,
  last_seen DATETIME
);

messages(
  id TEXT PRIMARY KEY,
  channel_id TEXT,
  author TEXT,
  content TEXT,
  timestamp DATETIME
);

statuses(
  user_id TEXT,
  status TEXT,
  updated_at DATETIME
);
```

---

# Realtime Event Types

Supported events:

* message_create
* message_update
* message_delete
* typing_start
* user_join
* user_leave
* status_update

---

# Goals

Molly is designed to feel like:

* IRC
* Discord
* tmux collaboration
* developer coworking
* terminal-native social infrastructure

The platform focuses on:

* realtime communication
* developer presence
* collaboration
* low-latency terminal workflows

---

# Future Plans

* Multi-provider support
* Matrix integration
* Slack bridge
* AI summaries
* Semantic search
* Git integration
* SSH mode
* Plugin system
* End-to-end encryption

---

# Troubleshooting

## Bot Not Receiving Messages

Ensure:

* Message Content Intent is enabled
* Bot has proper permissions
* Bot is invited correctly

---

## WebSocket Disconnects

Check:

* firewall rules
* reverse proxy configuration
* heartbeat/ping intervals

---

## SQLite Locked Errors

Use:

* WAL mode
* proper connection pooling
* mutex protection around writes

---

# License

MIT License
