package notecards

import (
	"fmt"
	"strings"
)

// Evidence says how the writer of a memory knows it — what recall shows next
// to the card and weighs it by. The agent states it when it remembers; a
// person's card is first-hand. "" = not stated (cards written before this
// existed): unrated, ranked neutrally.
const (
	EvidenceVerified = "verified" // checked: code, logs, a run, the docs
	EvidenceStated   = "stated"   // the user said so
	EvidenceInferred = "inferred" // the agent's own conclusion
	EvidenceHearsay  = "hearsay"  // heard / read somewhere, not confirmed
)

// Importance is how widely a memory applies — known when it is written
// (relevance to a question is computed by recall, per question).
const (
	ImportanceCore   = "core"   // a project rule, decision or root cause
	ImportanceNormal = "normal" // the default
	ImportanceDetail = "detail" // a one-off detail
)

// UnratedConfidence is what an agent card gets when the writer stated
// neither evidence nor confidence (and what every agent card got before).
const UnratedConfidence = 0.8

var evidenceDefault = map[string]float64{
	EvidenceVerified: 0.95,
	EvidenceStated:   0.85,
	EvidenceInferred: 0.6,
	EvidenceHearsay:  0.4,
}

// NormalizeEvidence lower-cases and checks an evidence value ("" is fine).
func NormalizeEvidence(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "", nil
	}
	if _, ok := evidenceDefault[s]; ok {
		return s, nil
	}
	return "", fmt.Errorf("evidence must be one of verified, stated, inferred, hearsay (how you know it)")
}

// NormalizeImportance lower-cases and checks an importance value ("" is fine).
func NormalizeImportance(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "", ImportanceCore, ImportanceNormal, ImportanceDetail:
		return s, nil
	}
	return "", fmt.Errorf("importance must be one of core, normal, detail")
}

// ConfidenceFor is a card's confidence: the stated one (clamped to 0–1),
// else the evidence's default, else UnratedConfidence.
func ConfidenceFor(evidence string, stated *float64) float64 {
	if stated != nil {
		c := *stated
		if c < 0 {
			c = 0
		}
		if c > 1 {
			c = 1
		}
		return c
	}
	if d, ok := evidenceDefault[evidence]; ok {
		return d
	}
	return UnratedConfidence
}

// Rated reports whether a card carries a real rating: an evidence value, or a
// confidence other than the unrated default (1 = a person's card, 0.8 =
// nobody said).
func Rated(evidence string, confidence float64) bool {
	return evidence != "" || (confidence > 0 && confidence < 1 && confidence != UnratedConfidence)
}

// RankWeight scales a hit's search score by how much to trust and how widely
// the card applies. Deliberately mild — matching the question decides; this
// only orders near-ties (a verified rule above a rumour). Unrated cards are
// neutral, so old memories don't sink.
func RankWeight(evidence string, confidence float64, importance string) float64 {
	w := 1.0
	if Rated(evidence, confidence) {
		w = 0.8 + 0.25*confidence // 0.4 → 0.9, 0.95 → ~1.04
	}
	switch importance {
	case ImportanceCore:
		w *= 1.1
	case ImportanceDetail:
		w *= 0.92
	}
	return w
}

// RatingTag is the short tag recall prints after a rated card
// ("inferred · 60%"); "" for an unrated one.
func RatingTag(evidence string, confidence float64) string {
	if !Rated(evidence, confidence) {
		return ""
	}
	pct := fmt.Sprintf("%d%%", int(confidence*100+0.5))
	if evidence == "" {
		return pct
	}
	return evidence + " · " + pct
}
