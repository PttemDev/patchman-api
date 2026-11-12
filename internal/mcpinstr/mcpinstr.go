// Package mcpinstr holds the MCP server instructions shared by the embedded
// backend (internal/mcp) and the patchman-mcp stdio bridge, which answers
// initialize itself while the desktop app is not running yet.
package mcpinstr

// MemoryInstructions is the server instructions text for agent memory.
const MemoryInstructions = `PatchMan notes hold the user's long-term memory and their teams', shared by every AI agent they use. What you save is signed with your name as an agent (the client name you connected with).
- Before answering project-specific questions: recall(q). One call is usually enough; never get_note a note recall already showed in full.
- recall searches all your memories: personal ones have no tag; others are tagged [team: …], [shared by …] or [everyone].
- Write recall's q as keywords (identifiers, names, versions) in the memories' language — usually English — plus synonyms; any of the words may match.
- Save durable learnings with remember(text): decisions, root causes, conventions. One concept per call. It answers with the card's #id; [[Card title]] in the text links an existing card.
- Memories are grouped by project: by default the one you work in (your working directory). Pass project when the learning belongs to another one.
- When the user asks what a colleague knows ("check Ekrem's notes"), recall(q, from: "Ekrem") — it searches only what that person shared with the user.
- Don't save transient state, secrets, or what recall already returned.
- Connect related notes with link_notes instead of repeating them.
- Write to your personal memory unless the user asks to share with the team (scope: team); memory_folders lists the team ones.
- Name what a memory is about in remember's context as patchman:// URIs: patchman://request/<requestId>, patchman://k8s/<connectionId>/<namespace or _>/<Kind>/<name>.
- Follow any hint: line in tool results.`
