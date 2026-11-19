package mcp

// Agents should know there is more than their own Agent Memory: team
// memories, memories colleagues shared, the "everyone" ones. recall already
// searches all of them, but an agent that doesn't know they exist never asks
// for them by name (from:, scope:) and can't tell the user where an answer
// came from. So:
//   - the instructions sent on initialize list the memories this user can
//     read (name, whose / which team, card count, biggest projects);
//   - every recall answer ends with a one-line "memories searched" summary.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type memoryFolder struct {
	Title     string `json:"title"`
	OwnerName string `json:"ownerName"`
	IsOwner   bool   `json:"isOwner"`
	Cards     int64  `json:"cards"`
	Scope     struct {
		Kind  string   `json:"kind"`
		Teams []string `json:"teams"`
		Owner string   `json:"owner"`
	} `json:"scope"`
	Projects []struct {
		Title string `json:"title"`
		Cards int64  `json:"cards"`
	} `json:"projects"`
}

type memoryDigestEntry struct {
	folders []memoryFolder
	at      time.Time
}

var memoryDigestCache sync.Map // userID → memoryDigestEntry

const memoryDigestTTL = 2 * time.Minute

// memoryFolders lists the memories the caller can read (cached briefly).
func (c *caller) memoryFolders(ctx context.Context) []memoryFolder {
	if v, ok := memoryDigestCache.Load(c.userID); ok {
		if e := v.(memoryDigestEntry); time.Since(e.at) < memoryDigestTTL {
			return e.folders
		}
	}
	code, body := c.cloudOrLocalCtx(ctx, "GET", "/api/notes/memory", nil)
	if code != http.StatusOK {
		return nil
	}
	var out struct {
		Data struct {
			Folders []memoryFolder `json:"folders"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(body), &out) != nil {
		return nil
	}
	memoryDigestCache.Store(c.userID, memoryDigestEntry{folders: out.Data.Folders, at: time.Now()})
	return out.Data.Folders
}

// whose names a memory the way recall tags its hits.
func (f memoryFolder) whose() string {
	switch f.Scope.Kind {
	case "personal":
		if f.IsOwner {
			return "yours"
		}
	case "team":
		if len(f.Scope.Teams) > 0 {
			return "team: " + strings.Join(f.Scope.Teams, ", ")
		}
		return "team"
	case "everyone":
		return "everyone"
	case "shared":
		if f.Scope.Owner != "" {
			return "shared by " + f.Scope.Owner
		}
	}
	if f.OwnerName != "" && !f.IsOwner {
		return "shared by " + f.OwnerName
	}
	return f.Scope.Kind
}

// memoryInstructionsDigest is the "memories you can read" block for the
// initialize instructions ("" when there is only the caller's own, or none).
func memoryInstructionsDigest(folders []memoryFolder) string {
	if len(folders) == 0 {
		return ""
	}
	others := 0
	for _, f := range folders {
		if !(f.IsOwner && f.Scope.Kind == "personal") {
			others++
		}
	}
	var b strings.Builder
	b.WriteString("Memories you can read right now (recall searches all of them; scope 'personal' / 'team' or from: '<person>' narrows it):\n")
	for i, f := range folders {
		if i == 8 {
			fmt.Fprintf(&b, "- … and %d more (memory_folders lists them all)\n", len(folders)-8)
			break
		}
		fmt.Fprintf(&b, "- %s — %s, %s", f.Title, f.whose(), cardsN(f.Cards))
		if len(f.Projects) > 0 {
			ps := append(f.Projects[:0:0], f.Projects...)
			sort.SliceStable(ps, func(i, j int) bool { return ps[i].Cards > ps[j].Cards })
			names := []string{}
			for _, p := range ps {
				if len(names) == 4 {
					break
				}
				names = append(names, fmt.Sprintf("%s %d", p.Title, p.Cards))
			}
			fmt.Fprintf(&b, " (projects: %s)", strings.Join(names, ", "))
		}
		b.WriteString("\n")
	}
	if others > 0 {
		b.WriteString("When an answer comes from someone else's or a team's memory, say so (recall tags those hits).")
	}
	return strings.TrimRight(b.String(), "\n")
}

// recallFooter is the one-line "memories searched" summary under a recall
// answer, so the agent (and the user) see that more than their own memory
// was consulted — or could be, with from:.
func recallFooter(folders []memoryFolder, scope, from string) string {
	if len(folders) < 2 || from != "" {
		return ""
	}
	parts := []string{}
	for _, f := range folders {
		if scope == "personal" && f.Scope.Kind != "personal" {
			continue
		}
		if scope == "team" && f.Scope.Kind != "team" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s (%s, %d)", f.Title, f.whose(), f.Cards))
		if len(parts) == 6 {
			break
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "memories searched: " + strings.Join(parts, " · ")
}

func cardsN(n int64) string {
	if n == 1 {
		return "1 card"
	}
	return fmt.Sprintf("%d cards", n)
}
