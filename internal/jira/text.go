package jira

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// FieldText renders a field value as plain text. It handles:
//   - a string (v2 wiki markup / plain text, kept as is);
//   - an Atlassian Document Format document (v3 rich text: description and
//     textarea custom fields on Cloud — doğrulanmadı for every node type);
//   - an option {value} / {name}, a user {displayName};
//   - an array of any of these (one line each);
//   - numbers and booleans.
//
// Anything else falls back to compact JSON so nothing is silently lost.
func FieldText(raw json.RawMessage) string {
	if isNull(raw) {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		lines := make([]string, 0, len(arr))
		for _, it := range arr {
			if t := FieldText(it); t != "" {
				lines = append(lines, "- "+t)
			}
		}
		return strings.Join(lines, "\n")
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		if strField(obj["type"]) == "doc" {
			return strings.TrimSpace(collapseBlank(adfText(raw)))
		}
		for _, k := range []string{"value", "displayName", "name"} {
			if v := strField(obj[k]); v != "" {
				if child, ok := obj["child"]; ok && !isNull(child) {
					if c := FieldText(child); c != "" {
						return v + " / " + c
					}
				}
				return v
			}
		}
		b, _ := json.Marshal(obj)
		return string(b)
	}
	var num json.Number
	if json.Unmarshal(raw, &num) == nil {
		return num.String()
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return fmt.Sprint(b)
	}
	return string(raw)
}

type adfNode struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Attrs   json.RawMessage `json:"attrs"`
	Content []adfNode       `json:"content"`
}

func adfText(raw json.RawMessage) string {
	var n adfNode
	if json.Unmarshal(raw, &n) != nil {
		return ""
	}
	var b strings.Builder
	renderADF(&b, n, "")
	return b.String()
}

func attr(n adfNode, key string) string {
	var m map[string]any
	if json.Unmarshal(n.Attrs, &m) != nil {
		return ""
	}
	if v, ok := m[key]; ok && v != nil {
		return fmt.Sprint(v)
	}
	return ""
}

// renderADF writes a node as text. Block nodes end with a newline; list
// items get "- " / "1. " with indentation for nesting.
func renderADF(b *strings.Builder, n adfNode, indent string) {
	switch n.Type {
	case "text":
		b.WriteString(n.Text)
	case "hardBreak":
		b.WriteString("\n" + indent)
	case "mention":
		if t := attr(n, "text"); t != "" {
			b.WriteString(t)
		}
	case "emoji":
		if t := attr(n, "text"); t != "" {
			b.WriteString(t)
		} else {
			b.WriteString(attr(n, "shortName"))
		}
	case "inlineCard", "blockCard", "embedCard":
		b.WriteString(attr(n, "url"))
	case "date":
		b.WriteString(attr(n, "timestamp"))
	case "status":
		b.WriteString("[" + attr(n, "text") + "]")
	case "rule":
		b.WriteString("\n---\n")
	case "paragraph", "heading":
		b.WriteString(indent)
		for _, c := range n.Content {
			renderADF(b, c, indent)
		}
		b.WriteString("\n")
	case "bulletList", "orderedList":
		for i, item := range n.Content {
			marker := "- "
			if n.Type == "orderedList" {
				marker = fmt.Sprintf("%d. ", i+1)
			}
			renderListItem(b, item, indent, marker)
		}
	case "taskList":
		for _, item := range n.Content {
			marker := "[ ] "
			if attr(item, "state") == "DONE" {
				marker = "[x] "
			}
			renderListItem(b, item, indent, marker)
		}
	case "codeBlock":
		b.WriteString(indent + "```\n")
		for _, c := range n.Content {
			renderADF(b, c, indent)
		}
		b.WriteString("\n" + indent + "```\n")
	case "table":
		for _, row := range n.Content {
			cells := make([]string, 0, len(row.Content))
			for _, cell := range row.Content {
				var cb strings.Builder
				for _, c := range cell.Content {
					renderADF(&cb, c, "")
				}
				cells = append(cells, strings.Join(strings.Fields(cb.String()), " "))
			}
			b.WriteString(indent + "| " + strings.Join(cells, " | ") + " |\n")
		}
	default:
		// doc, blockquote, panel, expand, mediaGroup… render their children.
		for _, c := range n.Content {
			renderADF(b, c, indent)
		}
	}
}

func renderListItem(b *strings.Builder, item adfNode, indent, marker string) {
	if item.Type == "taskItem" {
		// taskItem holds inline content directly.
		b.WriteString(indent + marker)
		for _, c := range item.Content {
			renderADF(b, c, indent+"  ")
		}
		b.WriteString("\n")
		return
	}
	first := true
	for _, c := range item.Content {
		if first && (c.Type == "paragraph" || c.Type == "heading") {
			b.WriteString(indent + marker)
			for _, cc := range c.Content {
				renderADF(b, cc, indent+"  ")
			}
			b.WriteString("\n")
			first = false
			continue
		}
		first = false
		renderADF(b, c, indent+"  ")
	}
}

// collapseBlank squeezes runs of blank lines to one.
func collapseBlank(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := false
	for _, l := range lines {
		l = strings.TrimRight(l, " \t")
		if l == "" {
			if blank {
				continue
			}
			blank = true
		} else {
			blank = false
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

var (
	wikiHeading = regexp.MustCompile(`^h[1-6]\.\s+`)
	wikiList    = regexp.MustCompile(`^([*#-]+)\s+`)
	wikiBold    = regexp.MustCompile(`\*([^*\n]+)\*`)
	wikiItalic  = regexp.MustCompile(`(^|\s)_([^_\n]+)_`)
	wikiMono    = regexp.MustCompile(`\{\{([^}\n]+)\}\}`)
	wikiLink    = regexp.MustCompile(`\[([^|\]\n]+)\|([^\]\n]+)\]`)
	wikiBlock   = regexp.MustCompile(`\{(code|noformat|quote|panel)(:[^}]*)?\}`)
)

// WikiText turns Jira wiki markup (v2 rich-text fields on Data Center) into
// plain text: headings lose "h3. ", "*" / "#" bullets become "- " / "1. ",
// *bold*, _italic_ and {{mono}} lose their marks, [text|url] becomes
// "text (url)". Rarer markup (tables, colors, macros) stays as written —
// doğrulanmadı against every DC macro.
func WikiText(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	counters := map[int]int{}
	for i, l := range lines {
		l = wikiHeading.ReplaceAllString(l, "")
		if m := wikiList.FindStringSubmatch(l); m != nil && !strings.HasPrefix(l, "----") {
			depth := len(m[1])
			indent := strings.Repeat("  ", depth-1)
			marker := "- "
			if strings.HasSuffix(m[1], "#") {
				counters[depth]++
				marker = fmt.Sprintf("%d. ", counters[depth])
			}
			l = indent + marker + l[len(m[0]):]
		} else {
			counters = map[int]int{}
		}
		l = wikiBold.ReplaceAllString(l, "$1")
		l = wikiItalic.ReplaceAllString(l, "$1$2")
		l = wikiMono.ReplaceAllString(l, "$1")
		l = wikiLink.ReplaceAllString(l, "$1 ($2)")
		l = wikiBlock.ReplaceAllString(l, "```")
		lines[i] = l
	}
	return strings.TrimSpace(collapseBlank(strings.Join(lines, "\n")))
}
