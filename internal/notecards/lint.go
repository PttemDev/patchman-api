package notecards

// Card lint: what makes a memory findable, from the 2026-11-18 recall
// benchmarks (.agent-memory-trial): one topic per card, a one-sentence lead
// that says what the card is about, a name people search for, links to the
// cards it relies on, and an anchor to the thing it is about. remember()
// returns these as hints; the card is saved either way.

import (
	"regexp"
	"unicode/utf8"
)

// LintGoodRunes is the size a card reads best at.
const LintGoodRunes = 4000

var (
	headingRE = regexp.MustCompile(`(?m)^(#{1,4} \S|\*\*[^*\n]{2,60}:?\*\*\s*$)`)
	bulletRE  = regexp.MustCompile(`(?m)^\s*[-*] \*\*[^*\n]{2,60}\*\*`)
	idLikeRE  = regexp.MustCompile("[A-Za-z][A-Za-z0-9]*[._/-][A-Za-z0-9]|[a-z][A-Z]|`[^`]+`|\\d+\\.\\d+")
)

// Lint returns the hints for a memory about to be saved. hasContext: the call
// passed patchman:// URIs; hasLinks: the text names other cards ([[…]] or a
// note: link).
func Lint(text string, hasContext bool) []string {
	title, summary, body := DeriveCard(text)
	var out []string
	if summary == "" || summary == body {
		out = append(out, "Start with a title line, then one sentence that says what this is about — recall shows that sentence and ranks by it.")
	}
	if n := utf8.RuneCountInString(text); n > LintGoodRunes {
		out = append(out, "Long for one card: keep one topic per card; split the rest into their own remember calls and link them with [[Title]].")
	} else if topics := len(headingRE.FindAllString(body, -1)) + len(bulletRE.FindAllString(body, -1)); topics >= 4 && n > LintGoodRunes/2 {
		out = append(out, "This reads like several topics; one card per topic is found more reliably.")
	}
	if !idLikeRE.MatchString(title + " " + summary) {
		out = append(out, "Name the thing in the title the way people search for it (service, table, flag, version).")
	}
	if !hasContext {
		out = append(out, "If this is about a specific table, deployment, file or topic, pass it as context (patchman://…) so it can be found from there.")
	}
	return out
}
