# PatchMan backend — developer guide

Read this before changing the backend, whether you are a person or an AI agent. It lists what the code
relies on but does not enforce, so a careless change breaks it quietly.

PatchMan is two repositories that ship together:

| Repo | What | Guide |
|---|---|---|
| `patchman-api` (this one) | Go API: gin + gorm, PostgreSQL (server) or SQLite (embedded in the desktop app), MCP server | `docs/dev/` |
| `patchman-ui` | React + TypeScript web app, Tauri desktop shell, release pipeline | [`docs/dev/`](https://github.com/PttemDev/patchman-ui/blob/master/docs/dev/README.md) |

## Contents

- [architecture.md](architecture.md): run modes, packages, request flow, data model.
- [rules.md](rules.md): invariants. Breaking one ships a bug or a leak. **Read it first.**
- [mcp.md](mcp.md): the MCP server, how to add a tool, and the tests that guard it.
- [recipes.md](recipes.md): an endpoint, a connection tool, an observability provider.
- [testing.md](testing.md): what to run before you say "done", and how to run a throwaway backend.

## The five rules people break most

1. **Never run the server from the repo root.** `config.Load` reads `.env` from the working directory,
   and a `.env` there may point at real infrastructure. Build into a scratch directory instead (see
   [testing.md](testing.md)).
2. **A list in JSON is `[]`, never `null`.** A nil Go slice marshals to `null`, and the frontend calls
   `.map` on it. This caused the 1.71.2 crash. See [rules.md](rules.md#json).
3. **Secrets never leave the backend unasked.** No secrets in logs, audit targets, error messages or
   MCP replies. See [rules.md](rules.md#secrets).
4. **Every MCP tool has an annotation, and the tool count test is updated.** See [mcp.md](mcp.md).
5. **Per-client values are config, not code.** No literal company domains or hosts. See
   [rules.md](rules.md#tenant-config).

Releases (version bump, changelog, signed builds, tags) are made by the maintainers from the frontend
repo: [patchman-ui `docs/dev/release.md`](https://github.com/PttemDev/patchman-ui/blob/master/docs/dev/release.md).
