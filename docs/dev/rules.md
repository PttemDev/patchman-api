# Backend rules (invariants)

Nothing enforces these rules mechanically, but each one was learned from a bug. If a change has to
break one, say so in the commit and update this file.

## Running locally

- **Never run `cmd/server` with the repo root as the working directory.** `config.Load` loads `.env`
  from the cwd, and a real `.env` holds live database hosts, keys and webhooks. Build into a scratch directory and
  start it from there under `env -i` (recipe in [testing.md](testing.md)).
- Never print, log or commit values from `.env`, `.env.*` or `.env.tenant`. Only the `*.example`
  files are tracked.

## JSON

- **A list is `[]`, not `null`.** A nil slice (`var xs []T`, or a struct field never assigned)
  marshals to `null`, and the frontend crashes on `.map` (1.71.2: `null is not an object (evaluating
  'ge.points.map')`). Initialise slices you return (`out := make([]T, 0, n)`), or normalise before
  `c.JSON` (as `ServiceDetail.Normalize` does).
- **Unknown is `null`, zero is `0`.** For metrics, use `*float64` when "no data" differs from 0. Never
  fill in an estimate the source didn't give. The UI shows `—` for null.
- **Bodies that contain markup:** `json.Marshal` escapes `<`, `>` and `&` as `\u003c`. To store or
  return a request body or SOAP/XML verbatim, use an encoder with `SetEscapeHTML(false)`
  (`marshalRaw` in `handlers/request_secrets.go`).
- **Envelope:** success is `{"data": …}`; errors are `{"error": "…"}` with a message a person can
  act on. Never put a stack trace or secret in `error`.

## Secrets

- Connection secrets are stored encrypted (`SecretsEnc`) and are **write-only through the API**.
  Reading one back is an explicit, audited endpoint (`reveal-secret`), owner-only.
- **MCP callers get secrets masked as `***`** in requests, collections and environments, unless the
  top-level collection's or the environment's `mcpShowSecrets` is on. Only a person (not MCP) can turn
  that on. When an agent writes `***` back, the stored value is kept: header and param rows are
  restored by key and auth fields by name, and a changed body or URL that still contains `***` gets a
  400 (`handlers/request_secrets.go`). Any new handler that returns request or environment data to MCP
  must go through this masking.
- Audit targets and log lines name *what* (connection, query text the user wrote), never credentials.

## Access control

- Every endpoint that takes a connection id calls the handler's `loadAccessible(id, userID)` and
  checks the role before it acts. A 404 for "not yours" is correct; leaking existence is not.
- Writes against an external system (publish, delete, run pipeline, …) need write access, and many
  also need the connection to be marked read-write (`allowWrite=true` on the call). Keep both checks.
- Grants and sharing exist only on the hosted backend. Don't add grant logic that assumes embedded
  mode.

## Tenant config

Per-client values come from config, never from literals:

- Backend: `internal/config/config.go`, for example `ALLOWED_EMAIL_DOMAINS`, `DEFAULT_ADMIN_EMAIL`,
  `ALLOWED_REDIRECT_ORIGINS` and `GITLAB_BASE_URL`. Redirect checks go through `originAllowed` in
  `handlers/tenant.go`.
- Add a new per-client key to `.env.tenant.example` too.
- Never introduce company-specific domains or hosts into code, tests or defaults.

## Database (gorm)

- Schema changes go through `AutoMigrate` in `internal/database/database.go`, so they must be
  **additive**. Add columns as nullable or with a safe default, and never rename or drop in place;
  both SQLite (embedded) and PostgreSQL run the same models.
- **`default:` tags bite.** With `gorm:"default:1"`, a Go zero value (`0`, `false`, `""`) is treated as
  "not set" and the default is written instead. If 0 / false is a real value, use a pointer, or write
  with `Select(...)` / a map in `Updates`.
- Ordered lists (`sort_order`) must handle ties: rows created without an order all start at 0. Sort
  with a stable tiebreaker (created_at, id), and renumber the whole sibling set on reorder.

## Desktop (embedded mode)

- Nothing may write much to stdout before `net.Listen`. The desktop shell drains the sidecar's
  stdout, but a large debug dump at boot once filled the pipe and killed launches (1.58.0). Embedded
  mode runs gin in release mode; keep it that way.
- New local endpoints are covered by the launch-token middleware automatically. Don't add exemptions
  without a reason recorded in the code.

## External APIs (observability, Git, cloud)

- Field names and result shapes you haven't seen in a real response are assumptions. Mark them in a
  comment as unverified  until checked against a live system. A fake server only
  proves what you assumed.
- Run independent upstream calls in parallel, and give each its own error (see `apm.panels`), so one
  bad query doesn't blank a whole page.
- Escape every user value you interpolate into a query language: NRQL (`nrqlString`), PromQL
  (`promLabel` / `sel`), SQL (parameters only), and so on.
