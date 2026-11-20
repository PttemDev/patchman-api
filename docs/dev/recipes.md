# Recipes (backend)

Each recipe is the minimum set of places to touch. Copy the closest existing feature rather than
starting from scratch.

## Add an HTTP endpoint

1. **Handler method** on the feature's handler in `internal/handlers/<feature>.go`.
   - Read `userID := c.GetString("userID")`.
   - Load what the caller may touch: `loadAccessible` for connections, the `access.go` role helpers
     for workspace-owned things.
   - Answer `c.JSON(status, gin.H{"data": …})`, or on error `gin.H{"error": "…"}`. Return `[]`, not
     `null`, for lists.
2. **Route** in `cmd/server/main.go`, next to the feature's other routes. Grep for the path first, so
   you don't shadow an existing one.
3. **Frontend call** in `src/services/api.ts` in patchman-ui, with `dbCfg(scope, …)` when the call can go to the
   local or the remote backend.
4. **Test** in `internal/handlers/*_test.go` (httptest plus SQLite, as the existing tests do).
5. If agents should use it, add an **MCP tool**: see [mcp.md](mcp.md).

## Add a connection tool (a new external system)

Mirror an existing one; the Kafka, S3 and RabbitMQ tools are the cleanest templates.

- **Model** in `internal/models/`. Keep secrets in `SecretsEnc`, encrypted with the backend's cipher,
  and add the model to `AutoMigrate` in `internal/database/database.go`. Changes are additive only.
- **Client id.** Accept an `id` from the client on create: the frontend writes the same row with the
  same id to the local and the remote backend (dual-write).
- **Handler** with `loadAccessible(id, userID)` → (row, role). CRUD is owner-only. Grants (user / team,
  read / write) and `reveal-secret` exist on the hosted backend only.
- **Writes** to the external system check write access *and* the connection's read-write flag. Audit
  them with `h.audit(...)`.
- **Package** under `internal/<tool>query/` holding the client code, kept free of gin.
- **MCP tools**: a `list_<tool>_connections` tool plus read tools, in their own `mcpscope` group if they
  are a new area.
- **Frontend**: store, sidebar, dialog, share dialog, i18n in 4 locales ([patchman-ui `docs/dev/recipes.md`](https://github.com/PttemDev/patchman-ui/blob/master/docs/dev/recipes.md)).

## Add an observability provider (service map / service detail)

Code: `internal/apm/`.

1. **Implement `apm.Provider`** (`provider.go`): `Name`, `Title`, `AuthKinds`, `Test`, `Scopes`, `Map`.
   Embed `base` for the defaults, and register it in `init()` with `register(...)`.
2. **In `Map`**, add nodes and edges to the `servicemap.Builder`:
   - Name services through `b.ServiceNode(ns, name, source)`. It merges them into Kubernetes workloads
     and records a `SourceRef` for the connection, which the handler sets with `b.SetOrigin`.
   - Call `b.AddRef(id, source, name, ns, key)` when the backend has its own id (New Relic entity guid,
     Coroot app id).
   - Put unknown rates in `Stats` with `HasRED` false, never as zeros.
3. **Optional service detail**: implement `apm.ServiceDetailer` (`detail.go`).
   - Run each panel through `panels.run(name, f)`, so a failing query only marks its own panel.
   - Escape interpolated names (`nrqlString`, `promLabel`).
   - The handler calls `ServiceDetail.Normalize()` before replying, so lists are never `null`.
4. **Tests** with a fake upstream (`newrelic_test.go`, `detail_test.go`). A fake shows only what you
   assumed: mark unverified field names in comments.
5. **Frontend**: add the provider to `src/components/observability/providers.ts` in patchman-ui (auth kinds, scope
   label, help text) and to the i18n files.
