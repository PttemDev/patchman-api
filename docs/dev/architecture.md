# Architecture (backend)

## Run modes

The same binary (`cmd/server`) runs in two modes, chosen by `APP_MODE` (`internal/config/config.go`):

| Mode | Where | Database | Users |
|---|---|---|---|
| `server` (default) | the team's hosted backend | PostgreSQL | many; JWT sessions, GitLab SSO, teams, grants |
| `embedded` | inside the desktop app (Tauri sidecar) | SQLite next to the app data | the person at the keyboard |

The desktop app can talk to **both** at once: the embedded backend ("local") and the team's backend
("remote"). Many features therefore exist on two backends. See "Two backends" below.

In embedded mode, every request needs the per-launch token (`X-Local-Token` header or `pm_lt` query)
and a loopback Host. `/healthz`, `/api/mcp` and `/mock/*` are exempt. The desktop shell owns the
lock / step-up state and pushes it to `/api/_local/security`; the backend answers 423 (locked) or 428
(step-up needed).

## Packages (`internal/`)

| Area | Packages |
|---|---|
| HTTP layer | `handlers` (one file per feature), `middleware`, `apicontract` |
| Data | `models` (gorm models), `database` (connection + `AutoMigrate` in `database.go`) |
| Config | `config` (all env; tenant values live here) |
| Connection tools | `dbquery`, `esquery`, `redisquery`, `kafkaquery`, `rabbitquery`, `s3client`, `k8sclient`, `grafanaquery`, `graylogquery`, `gcp`, `apm` (observability providers) |
| Service map | `servicemap` (graph builder, merges sources), `apm` (Coroot, Prometheus, New Relic, Jaeger, SigNoz, Kiali, Datadog) |
| AI | `mcp` (MCP server and tools), `mcpscope` (per-token tool groups), `mcpinstr`, `mcpdisco`, `notecards` / `notesearch` (agent memory) |
| Other | `realtime` (websocket collab), `authtoken`, `sessions`, `cloudsession`, `errorreport`, `webaudit`, `plugins`, `cleanup`, `forwarder` |

Routes are registered in one place: `cmd/server/main.go`. Grep it for a path before you add one.

## Request flow

```
FE (axios, src/services/api.ts)
  → gin route (cmd/server/main.go)
  → middleware (auth, local token, CORS)
  → handlers.<Feature>Handler
  → models / connection-tool package
  → c.JSON(status, gin.H{"data": …})   (the FE interceptor unwraps "data")
```

- **Success:** `{"data": …}`. **Errors:** `{"error": "human-readable message"}`, optionally with a
  `"code"`. The frontend shows `error` to the user, so write it for a person.
- **MCP tools** call the same HTTP API in-process (`internal/mcp/server.go` `caller`), with the header
  `X-PatchMan-Client: mcp`. Handlers that must act differently for agents check
  `noteSource(c) == noteSourceMCP`; secret masking is one example.

## Connections, grants, audit

Every connection tool (DB, Kafka, Redis, ES, S3, K8s, Grafana, RabbitMQ, Graylog, observability, Git,
GCP) follows the same shape:

- A model with encrypted secrets (`SecretsEnc`, AES with the backend's own key `GITLAB_TOKEN_ENC_KEY`).
  A row encrypted on one backend can't be decrypted on another.
- A handler whose `loadAccessible(id, userID)` returns the row and the caller's role: owner, or a grant
  (read / write, to a user or a team). **Every endpoint that takes a connection id goes through it.**
- Grants live on the remote (team) backend. Sharing means nothing on an embedded backend.
- Reads and writes against the external system are audited (`h.audit(...)`).

Roles for workspace-owned things (collections, environments) are in `handlers/access.go`
(`OwnerRole > EditorRole > ViewerRole`). A team admin cascades to editor and a team member to viewer.

## Two backends

The frontend decides which backend a call goes to (`DBScope = 'local' | 'remote'`, `dbCfg(scope)` in
`src/services/api.ts` in patchman-ui). The backend side of that contract:

- **Connections are dual-written.** The frontend creates the same row, with the same id, on every
  reachable backend. A handler must accept a client-supplied id and must not assume the row exists
  only here.
- **The hosted backend can't reach a local-only connection.** MCP replies point agents to the
  desktop's `patchman-local` MCP server for those (`internal/mcp/nextstep.go`).
