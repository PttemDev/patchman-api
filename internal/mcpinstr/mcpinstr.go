// Package mcpinstr holds the MCP server instructions shared by the embedded
// backend (internal/mcp) and the patchman-mcp stdio bridge, which answers
// initialize itself while the desktop app is not running yet.
package mcpinstr

// MemoryInstructions is the server instructions text for agent memory.
const MemoryInstructions = `PatchMan notes hold the user's long-term memory and their teams', shared by every AI agent they use. What you save is signed with your name as an agent (the client name you connected with).
- Before answering project-specific questions: recall(q). One call is usually enough; never get_note a note recall already showed in full.
- You can have several memories: your personal one plus team, shared and "everyone" memories other people gave you access to. recall searches all of them; personal hits have no tag, others are tagged [team: …], [shared by …] or [everyone]. memory_folders lists every memory you can read.
- Write recall's q as keywords (identifiers, names, versions) in the memories' language — usually English — plus synonyms; any of the words may match.
- Save durable learnings with remember(text): decisions, root causes, conventions. One concept per call. It answers with the card's #id; [[Card title]] in the text links an existing card.
- Memories are grouped by project: by default the one you work in (your working directory). Pass project when the learning belongs to another one.
- When the user asks what a colleague knows ("check Alex's notes"), recall(q, from: "Alex") — it searches only what that person shared with the user.
- Don't save transient state, secrets, or what recall already returned.
- Memories form a graph: link cards with a TYPED link instead of repeating them — link_notes(src, dst, type), or in remember's text [title](note:<id> "type"). Pick the type that says how they relate:
  depends_on (needs the other to work / be true), part_of (a piece of a larger topic, system or flow), example_of (a concrete instance of a rule or pattern), supersedes (replaces an older card — the old one goes stale), contradicts (conflicts; say which is right), refers_to (mentions an entity, e.g. a patchman:// URI), related (only when none of the others fits).
- After remember, link the new card to the cards recall showed that it depends on, belongs to or illustrates.
- Write to your personal memory unless the user asks to share with the team (scope: team); memory_folders lists the team ones.
- Name what a memory is about in remember's context as patchman:// URIs: patchman://request/<requestId>, patchman://k8s/<connectionId>/<namespace or _>/<Kind>/<name>.
- Follow any hint: line in tool results.`
