package errorreport

import (
	"fmt"
	"strings"
)

// buildSlackMessage turns a Payload into a Slack Block Kit JSON body.
// Format goal: dense and copy-pastable. The maintainer reading the
// channel should see env, user, where, what, and a short stack without
// expanding anything.
//
// Block Kit accepts a max of 50 blocks per message and 3000 chars per
// section's text — we stay well under both by truncating the stack to
// the top frames.
func buildSlackMessage(p *Payload) map[string]any {
	header := fmt.Sprintf("🚨 %s crash · v%s · %s / %s",
		fallback(p.AppName, "patchman-ui"),
		fallback(p.AppVersion, "?"),
		fallback(p.Platform, "?"),
		fallback(p.BuildEnv, "?"),
	)

	context := strings.Join([]string{
		"*Source* `" + string(p.Source) + "`",
		"*Build* " + fallback(p.BuildCommit, "?") + (func() string {
			if p.OSInfo != "" {
				return " · " + truncate(p.OSInfo, 80)
			}
			return ""
		})(),
		"*When* " + p.ServerAt.UTC().Format("2006-01-02 15:04:05 MST"),
	}, "\n")

	whoWhere := []string{
		"*User* " + fallback(p.UserEmail, p.UserID),
	}
	if p.WorkspaceName != "" || p.WorkspaceID != "" {
		whoWhere = append(whoWhere, "*Workspace* "+fallback(p.WorkspaceName, p.WorkspaceID))
	}
	if p.Route != "" {
		whoWhere = append(whoWhere, "*Route* `"+truncate(p.Route, 200)+"`")
	}
	if p.TabName != "" {
		whoWhere = append(whoWhere, "*Tab* `"+truncate(p.TabName, 120)+"`")
	}

	errorText := "*" + fallback(p.Message, "(no message)") + "*"

	stack := truncateStack(p.Stack, 8)
	stackText := ""
	if stack != "" {
		stackText = "```" + stack + "```"
	}

	componentStackText := ""
	if p.ComponentStack != "" {
		componentStackText = "*Component stack*\n```" + truncate(p.ComponentStack, 1500) + "```"
	}

	blocks := []map[string]any{
		{
			"type": "header",
			"text": map[string]any{"type": "plain_text", "text": truncate(header, 150), "emoji": true},
		},
		{
			"type": "section",
			"text": map[string]any{"type": "mrkdwn", "text": truncate(context, 2900)},
		},
		{
			"type": "section",
			"text": map[string]any{"type": "mrkdwn", "text": truncate(strings.Join(whoWhere, "\n"), 2900)},
		},
		{
			"type": "divider",
		},
		{
			"type": "section",
			"text": map[string]any{"type": "mrkdwn", "text": truncate(errorText, 2900)},
		},
	}
	if stackText != "" {
		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]any{"type": "mrkdwn", "text": stackText},
		})
	}
	if componentStackText != "" {
		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]any{"type": "mrkdwn", "text": componentStackText},
		})
	}

	return map[string]any{
		// Plain text fallback for notifications + clients that don't render Block Kit.
		"text":   header + " — " + truncate(p.Message, 200),
		"blocks": blocks,
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max < 4 {
		return s[:max]
	}
	return s[:max-1] + "…"
}

// truncateStack keeps the top N frames so the Slack message stays within
// the section character limit. Stack format from the FE is one frame per
// line; we slice on '\n'.
func truncateStack(stack string, frames int) string {
	if stack == "" {
		return ""
	}
	count := 0
	out := strings.Builder{}
	for i := 0; i < len(stack); i++ {
		if stack[i] == '\n' {
			count++
			if count >= frames {
				out.WriteString(stack[:i])
				out.WriteString("\n…")
				return out.String()
			}
		}
	}
	return stack
}

func fallback(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
