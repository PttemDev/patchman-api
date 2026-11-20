# AGENTS.md — PatchMan backend

Instructions for AI coding agents (and a short checklist for people). The full guide is
[docs/dev/](docs/dev/README.md). Read [docs/dev/rules.md](docs/dev/rules.md) before you change code.

- **Never run the server from the repo root.** A `.env` there may point at real infrastructure. Use a scratch dir
  ([docs/dev/testing.md](docs/dev/testing.md)). Never print or commit values from `.env*` files.
- **Before you say "done":** `go build ./... && go vet ./... && go test ./...`. Report what you ran.
- **JSON:** lists are `[]`, never `null`; unknown numbers are `null`, never an estimate;
  `{"data": …}` on success, `{"error": "…"}` on failure.
- **Secrets** never go to logs, errors, audit targets or MCP replies. MCP callers get them masked.
- **Every endpoint taking a connection id** uses `loadAccessible` and checks the role.
- **MCP tool:** annotate it in `internal/mcp/annotations.go`, bump the count in
  `internal/mcp/server_test.go`, keep replies small ([docs/dev/mcp.md](docs/dev/mcp.md)).
- **Schema:** additive `AutoMigrate` only. Beware gorm `default:` turning a zero value into the
  default.
- **No company domains or hosts in code.** They come from `internal/config` (tenant config).
- **Escape user values** you put into a query language (NRQL, PromQL, SQL parameters).
- **Git:** commit or push only when the person you work for asks. No `Co-Authored-By` footer.
  Releases are made by the maintainers from the frontend repo ([patchman-ui `docs/dev/release.md`](https://github.com/PttemDev/patchman-ui/blob/master/docs/dev/release.md)).
