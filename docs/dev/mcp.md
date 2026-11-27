# MCP server

PatchMan is an MCP server, so AI agents can use its tools. Code: `internal/mcp/`. The hosted server
serves `/api/mcp` per user and token, and the desktop app serves the same tools as `patchman-local`.

## How a tool works

- Tools are registered in `internal/mcp/server.go` with `reg` / `regCtx`, grouped by `register*`
  functions. A group is the unit of per-token permission (`internal/mcpscope`). `BuildServer` skips the
  groups a token doesn't allow, so the tool is neither listed nor callable.
- A tool usually calls the backend's own HTTP API in-process through `caller.callCtx`, which adds
  `X-PatchMan-Client: mcp`. So access control, audit and secret masking stay in the handlers, written
  once.
- Replies go through `compactReply` (`compact.go`). It caps a reply at **80 KB**
  (`maxReplyBytes`) by trimming the largest list, and `withNextStep` (`nextstep.go`) adds a `hint:`
  line for agents on common failures.

## Adding a tool (checklist)

1. **Input struct** with `json` and `jsonschema` tags. The schema text is what the agent reads, so say
   what each field is and where to get its value ("see list_x_connections").
2. **Register it** in the right `register*` group, with a description that says what it returns and
   whether it only reads. Append `connNote` for connection tools.
3. **Annotate it** in `internal/mcp/annotations.go` (`readRemote`, destructive, …).
   `TestEveryToolAnnotated` fails otherwise.
4. **Update the tool count** in `server_test.go` (currently 158) and add the name to the expected-tools
   list there.
5. **Keep the reply small.** Return what an agent needs, not the raw upstream payload:
   - `compactServiceMap` turns a graph into lines.
   - `leanCollections` returns a tree with filters.
   - `dropDetailCharts` leaves out time series unless asked.

   A reply that hits the 80 KB cap loses data silently.
6. **Filters belong in the input.** If a list can be large, take name / id / depth filters. An agent
   can't "scroll".
7. **Secrets:** go through handlers that mask for MCP. Never add a tool that returns a stored secret.
8. **Test the reply shaping**: the compactor, filters and the hint text (examples:
   `collections_lean_test.go`, `service_detail_test.go`).
9. If the tool belongs in a workflow prompt (`step2.go`, for example incident investigation), mention
   it there.

## Agent memory (notes / recall / remember)

- Cards are notes with a card type in memory folders (`internal/notecards`, `handlers/note_cards.go`).
- `remember` takes `evidence` (verified / stated / inferred / hearsay) and `importance`
  (core / normal / detail). Confidence comes from the evidence unless stated. Unrated cards keep 0.8
  and rank neutrally (`notecards/evidence.go`).
- Don't change ranking weights without a test. Old cards must not sink.
