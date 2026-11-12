package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/notecards"
	"patchman-be/internal/notesearch"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
)

// Card splitting: with an AI provider chosen in the person's memory settings,
// remember() text becomes 1–5 atomic cards with summaries, flagged duplicates
// and proposed links. The model only proposes — a duplicate becomes a merge
// suggestion, a "supersedes" link a supersede suggestion, and a link below
// autoLinkConfidence a link suggestion; any failure falls back to one card.

const (
	atomizeTimeout     = 60 * time.Second
	autoLinkConfidence = 0.95
	atomizeMaxCards    = 5
	atomizeContextMax  = 30
)

// SetLLM lets the remember worker use the person's AI provider; cipher
// decrypts its key (nil works only for a keyless local runtime).
func (h *NoteHandler) SetLLM(cipher *services.TokenCipher) {
	h.cipher = cipher
	h.llmHTTP = &http.Client{Timeout: atomizeTimeout}
}

type atomCard struct {
	Title       string     `json:"title"`
	Summary     string     `json:"summary"`
	Body        string     `json:"body"`
	DuplicateOf string     `json:"duplicate_of"`
	Links       []atomLink `json:"links"`
}

type atomLink struct {
	To         string  `json:"to"` // "#abcd1234" (a listed card) or "card:N" (N-th card of the answer)
	Type       string  `json:"type"`
	Confidence float64 `json:"confidence"`
}

// atomize asks the provider to split item into cards. known maps the short
// ids shown to the model back to note ids — links may only point there.
func (h *NoteHandler) atomize(item models.NoteInbox, folder models.Note) ([]atomCard, map[string]string, error) {
	if h.llmHTTP == nil {
		return nil, nil, errString("no AI provider")
	}
	s := h.memorySettings(item.UserID)
	if s.LLMProviderID == "" {
		return nil, nil, errString("no AI provider")
	}
	var prov models.LLMProvider
	// The provider may live only on the other backend (dual-write): then
	// there is simply nothing to call here.
	if err := h.db.Where("id = ? AND user_id = ?", s.LLMProviderID, item.UserID).First(&prov).Error; err != nil {
		return nil, nil, err
	}
	model := firstNonEmpty(s.LLMModel, prov.Model)
	if model == "" {
		return nil, nil, errString("no model")
	}
	key := ""
	if prov.APIKeyEnc != "" {
		if h.cipher == nil {
			return nil, nil, errString("no cipher")
		}
		k, err := h.cipher.Decrypt(prov.APIKeyEnc)
		if err != nil {
			return nil, nil, err
		}
		key = k
	}
	lines, known := h.atomizeContext(item, folder)
	system := atomizeSystemPrompt(lines)
	ctx, cancel := context.WithTimeout(context.Background(), atomizeTimeout)
	defer cancel()
	msgs := []map[string]string{{"role": "user", "content": item.Text}}
	for attempt := 0; attempt < 2; attempt++ {
		text, err := llmComplete(ctx, h.llmHTTP, &prov, key, model, system, msgs, 2500)
		if err != nil {
			return nil, nil, err
		}
		cards, perr := parseAtomCards(text)
		if perr == nil {
			return cards, known, nil
		}
		msgs = append(msgs,
			map[string]string{"role": "assistant", "content": text},
			map[string]string{"role": "user", "content": "That wasn't valid: " + perr.Error() + ". Reply with ONLY the JSON object."})
	}
	return nil, nil, errString("model reply not usable")
}

// atomizeContext lists existing cards the model may link to or flag as
// duplicates: the best search hits for the text's title and the folder's
// latest cards — titles and summaries only, and none that look like they
// hold a secret (they leave this server).
func (h *NoteHandler) atomizeContext(item models.NoteInbox, folder models.Note) ([]string, map[string]string) {
	title, _, _ := notecards.DeriveCard(item.Text)
	var ids []string
	if c, err := notesearch.Candidates(h.db, title, 20); err == nil {
		ids = append(ids, c...)
	}
	var recent []string
	h.db.Model(&models.Note{}).Where("parent_id = ? AND type = ?", folder.ID, "card").Order("updated_at DESC").Limit(atomizeContextMax).Pluck("id", &recent)
	ids = append(ids, recent...)
	known := map[string]string{}
	var lines []string
	for _, id := range dedupe(ids) {
		if len(lines) >= atomizeContextMax {
			break
		}
		var n models.Note
		if h.db.Select("id", "type", "title", "summary").Where("id = ?", id).First(&n).Error != nil || n.Type == "folder" || !canReadNote(h.db, item.UserID, id) {
			continue
		}
		line := "#" + notecards.ShortID(n.ID) + " " + n.Title
		if n.Summary != "" {
			line += " | " + n.Summary
		}
		if notecards.SecretKind(line) != "" {
			continue
		}
		known[notecards.ShortID(n.ID)] = n.ID
		lines = append(lines, line)
	}
	return lines, known
}

func atomizeSystemPrompt(existing []string) string {
	var b strings.Builder
	b.WriteString(`You turn a note someone wants remembered into knowledge cards for a team wiki.
Reply with ONLY a JSON object, no prose, no code fence:
{"cards":[{"title":"","summary":"","body":"","duplicate_of":"","links":[{"to":"","type":"","confidence":0}]}]}
Rules:
- 1 to 5 cards, one fact, decision, root cause or convention each. Usually one card is right.
- title: a short noun phrase, at most 80 characters.
- summary: one sentence that adds to the title (at most 200 characters); never just the title again.
- body: markdown with the details from the text. Keep code, names and numbers exactly. Add nothing that isn't in the text.
- duplicate_of: the #id of an existing card below that already says the same thing, else "".
- links: only to existing cards below ("#id") or to another card of your answer ("card:2"). type is one of: depends_on, part_of, contradicts, supersedes, example_of, related. confidence from 0 to 1.
- Write in the language of the text.
Existing cards:
`)
	if len(existing) == 0 {
		b.WriteString("(none)\n")
	}
	for _, l := range existing {
		b.WriteString(l + "\n")
	}
	return b.String()
}

// parseAtomCards reads the model's JSON (tolerating a code fence, a think
// block or prose around it) and checks it.
func parseAtomCards(text string) ([]atomCard, error) {
	text = stripThink(text)
	i, j := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if i < 0 || j <= i {
		return nil, fmt.Errorf("no JSON object")
	}
	var out struct {
		Cards []atomCard `json:"cards"`
	}
	if err := json.Unmarshal([]byte(text[i:j+1]), &out); err != nil {
		return nil, fmt.Errorf("JSON: %v", err)
	}
	if len(out.Cards) == 0 || len(out.Cards) > atomizeMaxCards {
		return nil, fmt.Errorf("cards must hold 1 to %d cards, got %d", atomizeMaxCards, len(out.Cards))
	}
	for k := range out.Cards {
		c := &out.Cards[k]
		c.Title = strings.TrimSpace(strings.TrimLeft(c.Title, "# "))
		c.Summary = strings.Join(strings.Fields(c.Summary), " ")
		c.Body = strings.TrimSpace(c.Body)
		if c.Title == "" {
			return nil, fmt.Errorf("card %d has no title", k+1)
		}
		if len([]rune(c.Title)) > 120 {
			c.Title = string([]rune(c.Title)[:119]) + "…"
		}
		if len([]rune(c.Summary)) > 200 {
			c.Summary = notecards.FirstSentence(c.Summary)
			if len([]rune(c.Summary)) > 200 {
				c.Summary = string([]rune(c.Summary)[:199]) + "…"
			}
		}
		if strings.EqualFold(c.Summary, c.Title) {
			c.Summary = ""
		}
		if c.Body == "" {
			c.Body = c.Summary
		}
	}
	return out.Cards, nil
}

// fileCards writes the model's cards: duplicates become merge suggestions,
// the rest new cards; then their links — sure ones applied, the rest (and
// every "supersedes") suggested.
func (h *NoteHandler) fileCards(item models.NoteInbox, folder models.Note, cards []atomCard, known map[string]string) (string, error) {
	ids := make([]string, len(cards))
	created := make([]bool, len(cards))
	for i, c := range cards {
		if notecards.SecretKind(c.Title+" "+c.Summary+" "+c.Body) != "" {
			continue // the model echoed something secret-looking; drop that card
		}
		if dup := known[strings.TrimPrefix(strings.TrimSpace(c.DuplicateOf), "#")]; dup != "" {
			var n models.Note
			if h.db.Where("id = ?", dup).First(&n).Error == nil {
				h.suggest(n, "merge", item.UserID, item.Agent, gin.H{"text": c.Body, "inboxId": item.ID})
				ids[i] = dup
				continue
			}
		}
		if id, dup := h.duplicateCard(item, folder, c.Title, c.Body); dup != "" {
			ids[i] = id
			continue
		}
		n, err := h.createCard(item, folder, c.Title, c.Summary, c.Body)
		if err != nil {
			return "", err
		}
		ids[i], created[i] = n.ID, true
	}
	for i, c := range cards {
		if !created[i] {
			continue
		}
		var src models.Note
		if h.db.Where("id = ?", ids[i]).First(&src).Error != nil {
			continue
		}
		for _, l := range c.Links {
			dst := ""
			to := strings.TrimSpace(l.To)
			if k, err := strconv.Atoi(strings.TrimPrefix(to, "card:")); err == nil && strings.HasPrefix(to, "card:") && k >= 1 && k <= len(ids) {
				dst = ids[k-1]
			} else {
				dst = known[strings.TrimPrefix(to, "#")]
			}
			if dst == "" || dst == src.ID || !notecards.ValidLinkType(l.Type) || l.Type == "refers_to" {
				continue
			}
			switch {
			case l.Type == "supersedes":
				var target models.Note
				if h.db.Where("id = ?", dst).First(&target).Error == nil {
					h.suggest(target, "supersede", item.UserID, item.Agent, gin.H{"by": src.ID})
				}
			case l.Confidence >= autoLinkConfidence:
				h.addLink(nil, item.UserID, true, item.Agent, src, dst, l.Type, l.Confidence)
			default:
				h.suggest(src, "link", item.UserID, item.Agent, gin.H{"dst": dst, "type": l.Type, "confidence": l.Confidence})
			}
		}
	}
	for _, id := range ids {
		if id != "" {
			return id, nil
		}
	}
	return "", errString("nothing to remember in that text")
}
