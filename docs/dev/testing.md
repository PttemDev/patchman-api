# Testing and verification (backend)

## Before you say "done"

```bash
cd patchman-api
go build ./...
go vet ./...
go test ./...
```

All three must be clean. For a change in one area, also run its package verbosely; a race you
suspect is worth `-count=50`.

Report what you actually ran. If something was only tested against a fake server, say so.

## Run a throwaway backend (click-testing)

**Never start the server from the repo root**: it loads the `.env` there, which may point at real
infrastructure. Build
into a scratch directory and run it from there with a clean environment:

```bash
S=/path/to/scratch            # any empty directory outside the repo
cd patchman-api && go build -o "$S/patchman-server" ./cmd/server
[ -f "$S/jwt" ] || openssl rand -hex 32 > "$S/jwt"
[ -f "$S/enc" ] || openssl rand -base64 32 > "$S/enc"
cd "$S"
env -i HOME="$HOME" PATH=/usr/bin:/bin \
  APP_MODE=server DB_DRIVER=sqlite DB_PATH="$S/x.db" PORT=8085 \
  JWT_SECRET="$(cat "$S/jwt")" GITLAB_TOKEN_ENC_KEY="$(cat "$S/enc")" \
  ALLOWED_REDIRECT_ORIGINS=http://localhost:3100 REALTIME_ALLOWED_ORIGINS=http://localhost:3100 \
  ./patchman-server > "$S/be.log" 2>&1 &
```

Then start the frontend against it ([patchman-ui `docs/dev/testing.md`](https://github.com/PttemDev/patchman-ui/blob/master/docs/dev/testing.md)) and register a user through the UI or
`POST /api/auth/register`.

- **Keep the same `GITLAB_TOKEN_ENC_KEY`** across restarts, or saved connections can't be decrypted.
- **Check the port first**: `lsof -ti :8085`. A stale scratch backend from an earlier run keeps
  answering with old code, which looks exactly like "my change didn't work".
- **Never put real credentials in the scratch database.** Use fake upstream servers instead.

## Fake upstream servers

To test an integration (New Relic, Coroot, GitLab, …) without the real system, write a small HTTP
server that answers the calls the code makes.

- **Make it concurrent.** In Python, use `ThreadingHTTPServer`, not `HTTPServer`. The backend runs
  panel queries in parallel, and a single-threaded fake resets connections, which looks like a backend
  bug.
- **A fake shows only what you assumed.** Response shapes that come from guesses stay unverified
  until someone sees a real response. Say so in the code and in the release notes.
