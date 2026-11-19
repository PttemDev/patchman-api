package notecards

import "testing"

func TestEvidenceRating(t *testing.T) {
	if _, err := NormalizeEvidence("Verified "); err != nil {
		t.Fatal(err)
	}
	if _, err := NormalizeEvidence("maybe"); err == nil {
		t.Fatal("unknown evidence must be refused")
	}
	if _, err := NormalizeImportance("huge"); err == nil {
		t.Fatal("unknown importance must be refused")
	}
	if c := ConfidenceFor(EvidenceInferred, nil); c != 0.6 {
		t.Errorf("inferred default = %v", c)
	}
	two := 2.0
	if c := ConfidenceFor("", &two); c != 1 {
		t.Errorf("clamp = %v", c)
	}
	if c := ConfidenceFor("", nil); c != UnratedConfidence {
		t.Errorf("unrated = %v", c)
	}
	// Legacy agent cards (0.8, no evidence) and a person's (1) are unrated:
	// no tag, neutral weight.
	if Rated("", UnratedConfidence) || Rated("", 1) || RatingTag("", UnratedConfidence) != "" {
		t.Error("unrated card shows a rating")
	}
	if RankWeight("", UnratedConfidence, "") != 1 {
		t.Error("unrated must rank neutrally")
	}
	if tag := RatingTag(EvidenceHearsay, 0.4); tag != "hearsay · 40%" {
		t.Errorf("tag = %q", tag)
	}
	// Verified ranks above hearsay; core above detail.
	if !(RankWeight(EvidenceVerified, 0.95, "") > RankWeight(EvidenceHearsay, 0.4, "")) {
		t.Error("verified must outrank hearsay")
	}
	if !(RankWeight(EvidenceStated, 0.85, ImportanceCore) > RankWeight(EvidenceStated, 0.85, ImportanceDetail)) {
		t.Error("core must outrank detail")
	}
	// Mild: never more than ±20% from neutral.
	for _, w := range []float64{RankWeight(EvidenceHearsay, 0, ImportanceDetail), RankWeight(EvidenceVerified, 1, ImportanceCore)} {
		if w < 0.7 || w > 1.2 {
			t.Errorf("weight out of range: %v", w)
		}
	}
}
