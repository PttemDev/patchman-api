// Package qalink ties code to Jira for the CR + QA Hub (CR_QA_RFC §5.2):
// which Jira issue an MR belongs to.
package qalink

import (
	"regexp"
	"strings"
)

// DefaultKeyPattern is a Jira issue key: PROJECT-123.
const DefaultKeyPattern = `[A-Za-z][A-Za-z0-9_]+-\d+`

var defaultRE = keyRE(DefaultKeyPattern)

// keyRE matches a key not glued to a preceding letter or digit (so the
// "abc" of "xabc-1" is not a key), capturing the key in group 1.
func keyRE(pattern string) *regexp.Regexp {
	return regexp.MustCompile(`(?:^|[^A-Za-z0-9])(` + pattern + `)`)
}

// ExtractJiraKey finds the Jira issue key of an MR: the first key in the
// title, then the source branch, then the description whose project is one
// of projectKeys (any project when projectKeys is empty). Branches are often
// lower case (feature/mer-936-p), so keys match case-insensitively and come
// back upper case. pattern "" means DefaultKeyPattern; an invalid one falls
// back to it.
func ExtractJiraKey(title, branch, description string, projectKeys []string, pattern string) string {
	re := defaultRE
	if p := strings.TrimSpace(pattern); p != "" {
		if _, err := regexp.Compile(p); err == nil {
			re = keyRE(p)
		}
	}
	allowed := map[string]bool{}
	for _, k := range projectKeys {
		if k = strings.ToUpper(strings.TrimSpace(k)); k != "" {
			allowed[k] = true
		}
	}
	for _, text := range []string{title, branch, description} {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			key := strings.ToUpper(m[1])
			dash := strings.LastIndex(key, "-")
			if dash <= 0 {
				continue
			}
			if len(allowed) == 0 || allowed[key[:dash]] {
				return key
			}
		}
	}
	return ""
}
