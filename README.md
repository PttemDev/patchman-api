# PatchMan API

Self-hosted API development & testing platform backend. Built with Go, Gin, and PostgreSQL.

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
  patchman-api
```

No `.env` files are baked into the image. Pass configuration at runtime with `-e` flags or `--env-file`:

```bash
docker run -p 8085:8085 --env-file .env patchman-api
```

Always set a strong, unique `JWT_SECRET` and change the default database credentials before deploying. See `.env.example` for all supported variables.

## Security

Checks run before the initial public release (November 2026):

- [x] `go build ./...`, `go vet ./...` and `go test ./...` pass
- [x] [`govulncheck`](https://go.dev/doc/security/vuln/) reports no known vulnerabilities reachable from this code (Go 1.26.11)
- [x] Source tree scanned for committed secrets, `.env` files and internal hostnames; none found

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
