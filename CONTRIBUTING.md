# Contributing to PatchMan API

Thanks for helping out. Bug reports, fixes, new connection types and tests are all welcome.

## Setup

You need Go 1.26 (use the latest 1.26.x patch release). The web UI lives in [patchman-ui](https://github.com/PttemDev/patchman-ui). Follow its **Quick start** to run both together, or just run the backend:

```bash
cp .env.example .env     # then edit the values
go run ./cmd/server
```

For a no-dependency try-out, run on SQLite: `DB_DRIVER=sqlite DB_PATH=./data/patchman.db`. To use PostgreSQL, start the compose file in `db/`.

## Before you open a pull request

```bash
gofmt -w <files you changed>
go vet ./...
go build ./...
go test ./...
```

- Format the files you touch with `gofmt`. Some existing files are not formatted yet; please keep unrelated reformatting out of your pull request.
- Add or update tests for behavior you change.
- New MCP tools follow the existing pattern in `internal/mcp`: register the tool, add it to a permission group, and update the tool-count assertion in `TestListTools`.
- Keep changes focused. One pull request per fix or feature makes review faster.
- Never commit secrets, `.env` files, tokens or internal hostnames. Use placeholders such as `example.com` in code, tests and docs.

## Pull request flow

1. Fork the repository and create a branch from `master`.
2. Make your change and run the checks above.
3. Open a pull request that explains what changed and why. Link the issue if there is one.

## Reporting bugs and security issues

Open an issue for bugs and feature requests. Please report security problems privately, as described in [SECURITY.md](SECURITY.md).

## License

By contributing you agree that your contributions are licensed under the [MIT License](LICENSE).
