package mcp

import (
	"encoding/json"
	"net/url"
	"patchman-be/internal/mcpinstr"
	"strconv"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---- Notes (knowledge base) --------------------------------------------------

type searchNotesIn struct {
	Query string `json:"query" jsonschema:"words to search for in note titles and bodies"`
	Limit int    `json:"limit,omitempty" jsonschema:"max results (default 20, max 50)"`
}

type createNoteIn struct {
	Title    string `json:"title" jsonschema:"page (or folder) title"`
	Body     string `json:"body,omitempty" jsonschema:"markdown content of the page"`
	ParentID string `json:"parentId,omitempty" jsonschema:"parent folder/page id; omit to create a new top-level space"`
	Type     string `json:"type,omitempty" jsonschema:"'page' (default), 'folder' or 'card' (one fact with a summary — prefer remember for cards)"`
	Summary  string `json:"summary,omitempty" jsonschema:"cards: one sentence (max 200 chars) that says more than the title"`
}

type recallIn struct {
	Q       string `json:"q" jsonschema:"keywords for what you want to know — names, identifiers, versions; add synonyms and the words the memories are written in (usually English): any of them may match"`
	Budget  int    `json:"budget,omitempty" jsonschema:"max tokens in the answer, 100-2000 (default 800)"`
	Scope   string `json:"scope,omitempty" jsonschema:"'all' (default), 'personal' or 'team' — which memories to search"`
	Project string `json:"project,omitempty" jsonschema:"the project you work in; its cards rank first (default: your working directory's name)"`
	From    string `json:"from,omitempty" jsonschema:"a person's name or email: answer only from their notes and memories — those they shared with you (or your own). Use it when the user asks what someone knows, e.g. 'check Ekrem's notes'"`
}

type rememberIn struct {
	Text    string `json:"text" jsonschema:"one durable fact, decision, root cause or convention (aim for under 4000 characters, 10000 at most; longer → split it, or write a page with create_note and link it). First line = title when followed by more text; the next sentence becomes the summary. [[Card title]] links to an existing card. Never secrets"`
	Context string `json:"context,omitempty" jsonschema:"patchman:// URIs of what this is about (e.g. patchman://request/<id>, patchman://k8s/<conn>/<ns>/<kind>/<name>); linked automatically"`
	Scope   string `json:"scope,omitempty" jsonschema:"'personal' (default) or 'team' (the team memory shared with you; your default one when there are several)"`
	Folder  string `json:"folderId,omitempty" jsonschema:"a memory folder id from memory_folders — overrides scope"`
	Project string `json:"project,omitempty" jsonschema:"the project this is about; cards are grouped by project (default: your working directory's name)"`
}

type linkNotesIn struct {
	Src  string `json:"src" jsonschema:"the note the link starts from: id or short id from recall (#abcd1234)"`
	Dst  string `json:"dst" jsonschema:"the target: a note id / short id, or a patchman:// URI"`
	Type string `json:"type" jsonschema:"one of: depends_on, part_of, contradicts, supersedes, example_of, related, refers_to"`
}

type updateNoteIn struct {
	ID string `json:"id" jsonschema:"note id to update"`
	// Mode "append" adds Body to the end atomically (safe alongside human
	// edits — use it for logs/journals). "replace" overwrites title/body and
	// requires baseVersion (from get_note) so a concurrent edit isn't lost.
	Mode        string  `json:"mode,omitempty" jsonschema:"'replace' (default) or 'append'"`
	Title       string  `json:"title,omitempty" jsonschema:"new title (replace mode)"`
	Body        string  `json:"body,omitempty" jsonschema:"markdown: the full new body (replace) or the text to add (append)"`
	BaseVersion int64   `json:"baseVersion,omitempty" jsonschema:"the note's version from get_note; required for replace — a newer version on the server returns a conflict"`
	ParentID    *string `json:"parentId,omitempty" jsonschema:"move under this folder/page id; omit to leave unchanged"`
}

const notesNote = " Notes are the team's markdown knowledge base (pages + folders, shared at any level); you see your own and those shared with you."

// registerNotesTools exposes the Notes knowledge base. Routed with
// cloudOrLocal like whiteboards: a desktop agent works on the notes the
// signed-in user actually sees.
func registerNotesTools(s *mcpsdk.Server, c *caller) {
	reg(s, "list_notes", "List notes: your own tree plus everything shared with you (metadata — ids, titles, parentIds, versions; no bodies)."+notesNote,
		func(_ struct{}) (int, string) {
			code, own := c.cloudOrLocal("GET", "/api/notes", nil)
			if code >= 400 {
				return code, own
			}
			code2, shared := c.cloudOrLocal("GET", "/api/notes/shared", nil)
			if code2 >= 400 {
				return code2, shared
			}
			return code, `{"own":` + dataOf(own) + `,"shared":` + dataOf(shared) + `}`
		})
	reg(s, "search_notes", "Full-text search over the notes you can read; returns ids, titles, breadcrumbs and snippets. Use get_note to read one."+notesNote,
		func(in searchNotesIn) (int, string) {
			v := url.Values{}
			v.Set("q", in.Query)
			if in.Limit > 0 {
				v.Set("limit", strconv.Itoa(in.Limit))
			}
			return c.cloudOrLocal("GET", "/api/notes/search?"+v.Encode(), nil)
		})
	reg(s, "get_note", "Read one note: markdown body, title, version (needed to update it), your role and breadcrumb. Links to other notes look like [text](note:<id>)."+notesNote,
		func(in idIn) (int, string) { return c.cloudOrLocal("GET", "/api/notes/"+url.PathEscape(in.ID), nil) })
	reg(s, "create_note", "Create a markdown page (or folder). Link other notes with [text](note:<id>)."+notesNote,
		func(in createNoteIn) (int, string) {
			body := map[string]any{"title": in.Title, "body": in.Body, "type": in.Type, "summary": in.Summary}
			if in.ParentID != "" {
				body["parentId"] = in.ParentID
			}
			return c.cloudOrLocal("POST", "/api/notes", body)
		})
	reg(s, "update_note", "Update a note: mode 'replace' (needs baseVersion from get_note) or 'append' (adds text at the end, safe alongside human edits); optionally move it with parentId."+notesNote,
		func(in updateNoteIn) (int, string) {
			path := "/api/notes/" + url.PathEscape(in.ID)
			if in.Mode == "append" {
				code, out := c.cloudOrLocal("POST", path+"/append", map[string]string{"text": in.Body})
				if code >= 400 || in.ParentID == nil {
					return code, out
				}
				return c.cloudOrLocal("PATCH", path, map[string]any{"parentId": *in.ParentID})
			}
			patch := map[string]any{}
			if in.Title != "" {
				patch["title"] = in.Title
			}
			if in.Body != "" {
				patch["body"] = in.Body
			}
			if len(patch) > 0 {
				patch["baseVersion"] = in.BaseVersion
			}
			if in.ParentID != nil {
				patch["parentId"] = *in.ParentID
			}
			return c.cloudOrLocal("PATCH", path, patch)
		})
	reg(s, "delete_note", "Delete a note and everything under it (soft delete — recoverable by the owner)."+notesNote,
		func(in idIn) (int, string) { return c.cloudOrLocal("DELETE", "/api/notes/"+url.PathEscape(in.ID), nil) })

	// Agent memory.
	reg(s, "recall", "Call before answering anything project-specific. Returns the notes and knowledge cards that match, with their links, within a token budget — one call is usually enough. Lines read '#id title | summary'; follow any 'hint:' line.",
		func(in recallIn) (int, string) {
			v := url.Values{}
			v.Set("q", in.Q)
			if in.Budget > 0 {
				v.Set("budget", strconv.Itoa(in.Budget))
			}
			if in.Scope != "" {
				v.Set("scope", in.Scope)
			}
			if in.Project != "" {
				v.Set("project", in.Project)
			}
			if in.From != "" {
				v.Set("from", in.From)
			}
			code, out := c.cloudOrLocal("GET", "/api/notes/recall?"+v.Encode(), nil)
			if code >= 400 {
				return code, out
			}
			var res struct {
				Text string `json:"text"`
			}
			if json.Unmarshal([]byte(dataOf(out)), &res) == nil && res.Text != "" {
				return code, res.Text
			}
			return code, out
		})
	reg(s, "remember", "Call when you learn something durable: a decision, root cause, convention or fact worth knowing next time. One concept per call, no secrets. It is filed as a knowledge card in your Agent Memory, under the project you work in, signed with your name as an agent; the answer names the card (#id) so you can link it.",
		func(in rememberIn) (int, string) {
			code, out := c.cloudOrLocal("POST", "/api/notes/remember", map[string]string{"text": in.Text, "context": in.Context, "scope": in.Scope, "folderId": in.Folder, "project": in.Project})
			if code >= 400 {
				return code, out
			}
			return code, rememberAnswer(out)
		})
	reg(s, "memory_folders", "List your memories: your personal Agent Memory folder and the team / shared ones you can read, each with its scope, whether you can write to it and its card count. Use an id as remember's folderId when there are several team memories.",
		func(in struct{}) (int, string) {
			return c.cloudOrLocal("GET", "/api/notes/memory", nil)
		})
	reg(s, "link_notes", "Connect two existing notes (or a note and a patchman:// entity) with a typed link. Prefer it over writing the same thing twice. Links from a note a person wrote are suggested to them instead.",
		func(in linkNotesIn) (int, string) {
			return c.cloudOrLocal("POST", "/api/notes/"+url.PathEscape(strings.TrimPrefix(in.Src, "#"))+"/links", map[string]string{"dst": in.Dst, "type": in.Type})
		})
}

// memoryInstructions is the MCP server's guidance, sent when the notes tools
// are on.
// memoryInstructions lives in mcpinstr so the stdio bridge can hand it out
// while the desktop app is still starting.
const memoryInstructions = mcpinstr.MemoryInstructions

// dataOf returns the raw JSON under a REST response's "data" key (or the body
// itself when it isn't wrapped), for stitching two responses into one result.
func dataOf(body string) string {
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal([]byte(body), &env) == nil && len(env.Data) > 0 {
		return string(env.Data)
	}
	if body == "" {
		return "null"
	}
	return body
}

type rememberCard struct {
	Short string `json:"short"`
	Title string `json:"title"`
}

// rememberAnswer turns remember's JSON into one line for the agent: the card
// it became (or matched), where it went, or that it is still queued. An
// older backend's answer comes back unchanged.
func rememberAnswer(out string) string {
	var r struct {
		Hint    string        `json:"hint"`
		Status  string        `json:"status"`
		Project string        `json:"project"`
		Outcome string        `json:"outcome"`
		Queued  bool          `json:"queued"`
		Card    *rememberCard `json:"card"`
	}
	if json.Unmarshal([]byte(dataOf(out)), &r) != nil || r.Status == "" {
		return out
	}
	text := rememberLine(r.Project, r.Outcome, r.Card)
	if r.Hint != "" {
		text += "\nhint: " + r.Hint
	}
	return text
}

func rememberLine(project, outcome string, card *rememberCard) string {
	where := ""
	if project != "" {
		where = " in project " + project
	}
	if card == nil {
		return "Queued" + where + " — the AI provider is splitting it into cards; recall finds them in a few seconds."
	}
	switch outcome {
	case "duplicate":
		return "Already remembered as " + card.Short + " \"" + card.Title + "\"" + where + " — nothing new saved."
	case "merge":
		return "A card titled like this exists: " + card.Short + " \"" + card.Title + "\"" + where + ". Your text was filed as a merge suggestion for its owner; to change the card yourself, update_note it."
	case "split":
		return "Saved" + where + " as cards, the first " + card.Short + " \"" + card.Title + "\"."
	}
	return "Saved " + card.Short + " \"" + card.Title + "\"" + where + "."
}
