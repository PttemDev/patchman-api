# PatchMan API

[![Slack: join the community](https://img.shields.io/badge/Slack-join_the_community-4A154B?logo=slack&logoColor=white)](https://join.slack.com/t/patchman-workspace/shared_invite/zt-4hjqey19u-~AxNN3qSh51RbOsedKTwdw)

Self-hosted API development & testing platform backend. Built with Go, Gin, and PostgreSQL (or SQLite).

This is the backend. For the product overview, screenshots, the MCP guide for AI agents and a quick start that runs the backend together with the web UI, see the [patchman-ui README](https://github.com/PttemDev/patchman-ui#readme).

## Features

- **Collections & Requests** - Organize and save API requests with folders, editable base URL per collection
- **Collection Sync** - Refresh collections from Swagger/OpenAPI URLs, update from JSON or Postman format
- **Collaboration** - Share collections via code, real-time team collaboration
- **Workspaces** - Multi-workspace support with role-based member management
- **Proxy** - Send HTTP requests through backend (bypass CORS), lightweight CORS proxy mode
- **Mock Servers** - Create mock API endpoints with custom responses, catch-all routing
- **Environments** - Variable substitution with multiple environments, secret variable support
- **Test Management** - Test cases with versioning, test plans, shared steps, execution and reporting
- **Test Runs** - Track test plan execution with run history
- **Load Testing** - Concurrent load tests with live SSE results streaming
- **Import** - Swagger/OpenAPI (v2 & v3), Postman collection v2.1, JUnit XML
- **Code Generation** - Generate code snippets (curl, Python, JS, Go, Java, C#, PHP, Ruby)
- **WebSocket & SSE** - WebSocket and Server-Sent Events client with message history
- **Cookie Management** - Persistent cookie jar
- **Collection Runner** - Sequential and data-driven test execution
- **Script Execution** - Pre-request and test scripts
- **Comments** - Comment on any entity (test cases, defects, etc.)
- **Defect Tracking** - Bug tracking linked to test cases
- **Requirements** - Traceability matrix for requirements coverage
- **Audit & Activity** - Audit logs and activity feed
- **History** - Request history with automatic 30-day retention
- **Search** - Global search across all resources
- **Data & infrastructure connections** - PostgreSQL, MySQL, SQL Server, MongoDB (indexes, explain, aggregation pipelines), BigQuery (dry-run check and scan limits), Redis, Elasticsearch (incl. Kibana data views), Kafka, RabbitMQ, S3, Kubernetes, Grafana, Graylog; secrets encrypted at rest
- **Google Cloud** - Cloud Storage, Pub/Sub, GKE, Cloud Logging and BigQuery via a service account key, gcloud credentials (desktop) or a linked Google account
- **Git (GitLab & GitHub)** - Repositories, files, history, code search, merge/pull request review, GitLab pipelines and GitHub Actions; local git folders and IDE-style branch operations in desktop mode
- **Sign-in providers** - Email/password, GitLab, GitHub (incl. Enterprise Server) and Google (optionally limited to Workspace domains)
- **Sessions & devices** - Sliding sessions with an optional idle timeout, signed-in device list and remote sign-out
- **Plugins** - Sandboxed WebAssembly plugins (Extism) with declared egress, per-connection secrets and signature checks
- **MCP server** - Tools, resources, prompts and progress for AI agents at `/api/mcp`, with per-token tool groups and shared agent memory

## Quick Start

### 1. Database

```bash
cd db
docker compose up -d
```

This starts PostgreSQL 16 on port 5432. Tables are created automatically by GORM AutoMigrate on first run.

### 2. Environment

The server loads environment files based on `APP_ENV`:

```bash
# Default: loads .env
go run cmd/server/main.go

# Production: loads .env.production
APP_ENV=production go run cmd/server/main.go

# Test: loads .env.test
APP_ENV=test go run cmd/server/main.go
```

Create your `.env` file:

```bash
cp .env.example .env
# Edit .env with your database credentials
```

| Variable | Default | Description |
|----------|---------|-------------|
| `APP_ENV` | *(empty)* | Environment name (`production`, `test`). Loads `.env.{APP_ENV}` |
| `DB_HOST` | `localhost` | PostgreSQL host |
| `DB_PORT` | `5432` | PostgreSQL port |
| `DB_NAME` | `getman` | Database name |
| `DB_USER` | `getman` | Database user |
| `DB_PASSWORD` | `getman123` | Database password |
| `DB_SSLMODE` | `disable` | SSL mode |
| `JWT_SECRET` | - | JWT signing secret (required, change in production!) |
| `PORT` | `8085` | API server port |
| `SESSION_TTL` | `720h` | Sliding session lifetime |
| `SESSION_IDLE_TTL` | `0` (off) | End sessions unused for this long |
| `GITHUB_BASE_URL` | `https://github.com` | GitHub or GitHub Enterprise Server root |
| `GITHUB_OAUTH_CLIENT_ID` / `_SECRET` | - | GitHub sign-in; empty = off. Callback: `<origin>/auth/github/callback`, `patchman://auth/github/callback` |
| `GOOGLE_OAUTH_CLIENT_ID` / `_SECRET` | - | Google sign-in; empty = off. Callback: `<origin>/auth/google/callback`, `patchman://auth/google/callback` |
| `GOOGLE_WORKSPACE_DOMAINS` | - | Limit Google sign-in to these Workspace domains |
| `GOOGLE_OAUTH_EXTERNAL_CLIENT_ID` / `_SECRET` | - | Optional second client for Google Cloud connections from outside those domains |
| `BIGQUERY_MAX_BYTES_CEILING` | - | Server-wide BigQuery scan ceiling per query (e.g. `100GB`) |
| `PATCHMAN_PLUGIN_TRUSTED_KEYS` | - | Trusted plugin publisher keys: `Name=base64key,...` |
| `PLUGIN_PRIVATE_CIDRS` | - | Private CIDRs plugins may reach on a hosted server |

See `.env.example` for the full list, including the tenant values (`ALLOWED_EMAIL_DOMAINS`, `ALLOWED_REDIRECT_ORIGINS`, GitLab OAuth, `GITLAB_TOKEN_ENC_KEY`).

### 3. Run

```bash
go run cmd/server/main.go
```

Server starts on `http://localhost:8085`.

### 4. API Docs

Open `http://localhost:8085/api/docs` for interactive Scalar API documentation.

## Docker

```bash
docker build -t patchman-api .
docker run -p 8085:8085 \
  -e DB_HOST=host.docker.internal \
  -e DB_PORT=5432 \
  -e DB_NAME=getman \
  -e DB_USER=getman \
  -e DB_PASSWORD=getman123 \
  -e JWT_SECRET=your-secret-here \
  -e GITLAB_TOKEN_ENC_KEY="$(openssl rand -base64 32)" \
  patchman-api
```

No `.env` files are baked into the image. Pass configuration at runtime with `-e` flags or `--env-file`:

```bash
docker run -p 8085:8085 --env-file .env patchman-api
```

Always set a strong, unique `JWT_SECRET` and change the default database credentials before deploying. See `.env.example` for all supported variables.

Keep the same `GITLAB_TOKEN_ENC_KEY` across restarts, or saved connection secrets can't be decrypted. Without it, connection, LLM provider and Git tool routes stay off, and the log says so.

To use this container from the desktop app, add it under Settings → Remote as `http://localhost:8085/api`. The URL must end in `/api`.

## Security

Checks run before the initial public release (November 2026):

- [x] `go build ./...`, `go vet ./...` and `go test ./...` pass
- [x] [`govulncheck`](https://go.dev/doc/security/vuln/) reports no known vulnerabilities reachable from this code (Go 1.26.11)
- [x] Source tree scanned for committed secrets, `.env` files and internal hostnames; none found

Re-checked for 1.67.1 (November 2026): `go build ./...`, `go vet ./...` and `go test ./...` pass, and the source tree was scanned again for secrets, internal hostnames and personal names. `govulncheck` was not re-run for this version.

Re-checked for 1.68.2 (November 2026): `go build ./...`, `go vet ./...` and `go test ./...` pass, and the changes since 1.67.1 were scanned for secrets, internal hostnames and personal names. `govulncheck` was not re-run for this version.

Re-checked for 1.70.0 (November 2026): `go build ./...`, `go vet ./...` and `go test ./...` pass, and the changes since 1.68.2 (1.69.0 and 1.70.0) were scanned for secrets, internal hostnames and personal names. `govulncheck` was not re-run for this version.

Build with Go 1.26.11 or newer. Older patch releases of the standard library contain known vulnerabilities.

These checks are a point-in-time snapshot, not a security audit. Re-run `govulncheck ./...` regularly. To report a vulnerability, see `SECURITY.md`.

## Project Structure

```
cmd/server/main.go       # Entry point, route registration
internal/
  config/                # Environment config loader (godotenv + APP_ENV)
  database/              # GORM init + auto-migrate
  handlers/              # HTTP handlers (one per domain)
  middleware/            # JWT auth middleware
  models/               # GORM models
  services/             # Business logic (proxy, codegen, script engine)
db/
  docker-compose.yml    # PostgreSQL setup
  init.sql              # Extensions (uuid, trigram, unaccent)
```

## Community

Join the [PatchMan Slack workspace](https://join.slack.com/t/patchman-workspace/shared_invite/zt-4hjqey19u-~AxNN3qSh51RbOsedKTwdw) to ask questions, share feedback and ideas, and hear about new releases. Bug reports and feature requests are also welcome as GitHub issues.
