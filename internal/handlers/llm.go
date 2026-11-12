package handlers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// LLMProviderHandler manages saved LLM endpoints and proxies chat requests to
// them. It mirrors the DB-connection storage model: an FE-supplied UUID PK
// (dual-writable to the local sidecar + remote BE), a single AES-GCM-encrypted
// secret (the API key) via the shared dbCipher, and owner-only access.
//
// Chat requests are proxied + streamed SERVER-SIDE: the key is decrypted here,
// never sent to the browser, and the provider's SSE (OpenAI or Anthropic
// shapes) is normalized into ONE PatchMan frame format the FE reads
// provider-agnostically: `event: token {"text"}`, `event: done {}`,
// `event: error {"message"}`.
type LLMProviderHandler struct {
	db     *gorm.DB
	cipher *services.TokenCipher
	stream *http.Client // Timeout 0 — chat streams may run for minutes
	probe  *http.Client // short timeout — connectivity test only
}

func NewLLMProviderHandler(db *gorm.DB, cipher *services.TokenCipher) *LLMProviderHandler {
	return &LLMProviderHandler{
		db:     db,
		cipher: cipher,
		stream: &http.Client{Timeout: 0},
		probe:  &http.Client{Timeout: 15 * time.Second},
	}
}

// ── wire shapes ─────────────────────────────────────────────────────

type llmProviderInput struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	BaseURL string `json:"baseUrl"`
	Model   string `json:"model"`
	// APIKey is a pointer so Update can tell "not provided" (nil, keep the
	// stored key) from "clear it" (""). Create treats nil/"" as no key.
	APIKey       *string `json:"apiKey"`
	PrimaryScope string  `json:"primaryScope"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	ProviderID  string        `json:"providerId"`
	Model       string        `json:"model"`
	System      string        `json:"system"`
	Messages    []chatMessage `json:"messages"`
	Temperature *float64      `json:"temperature"`
	MaxTokens   *int          `json:"maxTokens"`
}

func llmToDTO(p models.LLMProvider) gin.H {
	return gin.H{
		"id": p.ID, "userId": p.UserID, "name": p.Name, "kind": p.Kind,
		"baseUrl": p.BaseURL, "model": p.Model, "hasApiKey": p.APIKeyEnc != "",
		"primaryScope": p.PrimaryScope, "createdAt": p.CreatedAt, "updatedAt": p.UpdatedAt,
	}
}

func validLLMKind(k string) bool { return k == "openai" || k == "anthropic" }

func (h *LLMProviderHandler) loadOwned(id, userID string) (*models.LLMProvider, error) {
	var p models.LLMProvider
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&p).Error; err != nil {
		return nil, err
	}
	return &p, nil
}

// ── CRUD ────────────────────────────────────────────────────────────

func (h *LLMProviderHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")
	var in llmProviderInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.BaseURL) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name and baseUrl are required"})
		return
	}
	if !validLLMKind(in.Kind) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "kind must be 'openai' or 'anthropic'"})
		return
	}
	enc := ""
	if in.APIKey != nil && *in.APIKey != "" {
		e, err := h.cipher.Encrypt(*in.APIKey)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt key: " + err.Error()})
			return
		}
		enc = e
	}
	id := strings.TrimSpace(in.ID)
	if id == "" {
		id = uuid.New().String()
	}
	row := models.LLMProvider{
		ID: id, UserID: userID, Name: strings.TrimSpace(in.Name), Kind: in.Kind,
		BaseURL: strings.TrimRight(strings.TrimSpace(in.BaseURL), "/"),
		Model:   strings.TrimSpace(in.Model), APIKeyEnc: enc,
		PrimaryScope: in.PrimaryScope,
	}
	if err := h.db.Create(&row).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": llmToDTO(row)})
}

func (h *LLMProviderHandler) List(c *gin.Context) {
	userID := c.GetString("userID")
	var rows []models.LLMProvider
	if err := h.db.Where("user_id = ?", userID).Order("created_at desc").Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		out = append(out, llmToDTO(r))
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func (h *LLMProviderHandler) Get(c *gin.Context) {
	userID := c.GetString("userID")
	p, err := h.loadOwned(c.Param("id"), userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Provider not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": llmToDTO(*p)})
}

func (h *LLMProviderHandler) Update(c *gin.Context) {
	userID := c.GetString("userID")
	p, err := h.loadOwned(c.Param("id"), userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Provider not found"})
		return
	}
	var in llmProviderInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// Mutate the loaded row + Save (rather than a column-name map) so GORM's
	// initialism naming for APIKeyEnc can't bite us.
	if s := strings.TrimSpace(in.Name); s != "" {
		p.Name = s
	}
	if in.Kind != "" {
		if !validLLMKind(in.Kind) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "kind must be 'openai' or 'anthropic'"})
			return
		}
		p.Kind = in.Kind
	}
	if s := strings.TrimSpace(in.BaseURL); s != "" {
		p.BaseURL = strings.TrimRight(s, "/")
	}
	p.Model = strings.TrimSpace(in.Model)
	if in.PrimaryScope != "" {
		p.PrimaryScope = in.PrimaryScope
	}
	// Re-encrypt only when a key was explicitly provided (nil = keep stored;
	// "" = clear it).
	if in.APIKey != nil {
		if *in.APIKey == "" {
			p.APIKeyEnc = ""
		} else {
			e, err := h.cipher.Encrypt(*in.APIKey)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt key: " + err.Error()})
				return
			}
			p.APIKeyEnc = e
		}
	}
	if err := h.db.Save(p).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": llmToDTO(*p)})
}

func (h *LLMProviderHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	if err := h.db.Where("id = ? AND user_id = ?", c.Param("id"), userID).Delete(&models.LLMProvider{}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"deleted": true}})
}

// RevealAPIKey returns the decrypted key to the OWNER only (for editing). This
// is the single plaintext path besides the server-side proxy.
func (h *LLMProviderHandler) RevealAPIKey(c *gin.Context) {
	userID := c.GetString("userID")
	p, err := h.loadOwned(c.Param("id"), userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Provider not found"})
		return
	}
	key := ""
	if p.APIKeyEnc != "" {
		k, err := h.cipher.Decrypt(p.APIKeyEnc)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "decrypt key: " + err.Error()})
			return
		}
		key = k
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"apiKey": key}})
}

// Test probes provider connectivity. With :id it uses the saved (decrypted)
// key; without :id it tests an inline pre-save payload. Always 200 — the body's
// {ok,error} carries the verdict so the FE shows green/red without a throw.
func (h *LLMProviderHandler) Test(c *gin.Context) {
	userID := c.GetString("userID")
	var kind, base, key string
	if id := c.Param("id"); id != "" {
		p, err := h.loadOwned(id, userID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Provider not found"})
			return
		}
		kind, base = p.Kind, p.BaseURL
		if p.APIKeyEnc != "" {
			key, _ = h.cipher.Decrypt(p.APIKeyEnc)
		}
	} else {
		var in llmProviderInput
		if err := c.ShouldBindJSON(&in); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		kind, base = in.Kind, in.BaseURL
		if in.APIKey != nil {
			key = *in.APIKey
		}
	}
	if !validLLMKind(kind) {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": false, "error": "kind must be 'openai' or 'anthropic'"}})
		return
	}
	ok, msg := h.probeProvider(c.Request.Context(), kind, base, key)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": ok, "error": msg}})
}

func (h *LLMProviderHandler) probeProvider(ctx context.Context, kind, base, key string) (bool, string) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return false, "baseUrl is required"
	}
	var req *http.Request
	switch kind {
	case "openai":
		req, _ = http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
	case "anthropic":
		req, _ = http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
		if key != "" {
			req.Header.Set("x-api-key", key)
		}
		req.Header.Set("anthropic-version", "2023-06-01")
	default:
		return false, "kind must be 'openai' or 'anthropic'"
	}
	resp, err := h.probe.Do(req)
	if err != nil {
		return false, err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		return true, ""
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return false, fmt.Sprintf("provider %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
}

// ── streaming chat proxy ────────────────────────────────────────────

// Chat proxies a single-turn (optionally system-prompted) completion to the
// provider and streams normalized SSE frames back. Pre-stream failures return
// JSON (so the FE can read res.ok); once the 200 stream starts, errors arrive
// as an `error` frame.
func (h *LLMProviderHandler) Chat(c *gin.Context) {
	userID := c.GetString("userID")
	var req chatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(req.Messages) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "at least one message is required"})
		return
	}
	prov, err := h.loadOwned(req.ProviderID, userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Provider not found"})
		return
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = prov.Model
	}
	if model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "a model is required (set one on the provider or in the request)"})
		return
	}
	key := ""
	if prov.APIKeyEnc != "" {
		k, decErr := h.cipher.Decrypt(prov.APIKeyEnc)
		if decErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "decrypt key: " + decErr.Error()})
			return
		}
		key = k
	}
	upReq, err := h.buildUpstream(c.Request.Context(), prov, key, model, req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	resp, err := h.stream.Do(upReq)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "connect to provider: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		c.JSON(http.StatusBadGateway, gin.H{
			"error":  fmt.Sprintf("provider %d", resp.StatusCode),
			"status": resp.StatusCode,
			"body":   strings.TrimSpace(string(b)),
		})
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	b, _ := json.Marshal(gin.H{"model": model, "kind": prov.Kind})
	writeSSEEvent(c.Writer, "open", string(b))
	c.Writer.Flush()

	h.pump(c, resp.Body, prov.Kind)
}

func (h *LLMProviderHandler) buildUpstream(ctx context.Context, prov *models.LLMProvider, key, model string, req chatRequest) (*http.Request, error) {
	base := strings.TrimRight(prov.BaseURL, "/")
	switch prov.Kind {
	case "openai":
		body := map[string]any{"model": model, "stream": true, "messages": oaiMessages(req)}
		if req.Temperature != nil {
			body["temperature"] = *req.Temperature
		}
		if req.MaxTokens != nil && *req.MaxTokens > 0 {
			body["max_tokens"] = *req.MaxTokens
		}
		payload, _ := json.Marshal(body)
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/chat/completions", bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "text/event-stream")
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		return r, nil
	case "anthropic":
		maxTok := 1024
		if req.MaxTokens != nil && *req.MaxTokens > 0 {
			maxTok = *req.MaxTokens
		}
		body := map[string]any{"model": model, "stream": true, "max_tokens": maxTok, "messages": anthropicMessages(req)}
		if s := strings.TrimSpace(req.System); s != "" {
			body["system"] = s
		}
		if req.Temperature != nil {
			body["temperature"] = *req.Temperature
		}
		payload, _ := json.Marshal(body)
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/messages", bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "text/event-stream")
		r.Header.Set("anthropic-version", "2023-06-01")
		if key != "" {
			r.Header.Set("x-api-key", key)
		}
		return r, nil
	}
	return nil, fmt.Errorf("unsupported provider kind %q", prov.Kind)
}

func oaiMessages(req chatRequest) []map[string]string {
	out := make([]map[string]string, 0, len(req.Messages)+1)
	if s := strings.TrimSpace(req.System); s != "" {
		out = append(out, map[string]string{"role": "system", "content": s})
	}
	for _, m := range req.Messages {
		out = append(out, map[string]string{"role": m.Role, "content": m.Content})
	}
	return out
}

func anthropicMessages(req chatRequest) []map[string]string {
	out := make([]map[string]string, 0, len(req.Messages))
	for _, m := range req.Messages {
		role := m.Role
		if role != "user" && role != "assistant" {
			role = "user"
		}
		out = append(out, map[string]string{"role": role, "content": m.Content})
	}
	return out
}

// pump reads the provider's SSE body, buffers each event's data lines, and
// hands each complete payload to the kind-specific normalizer, flushing after
// every frame so tokens reach the FE incrementally. Honors client disconnect.
func (h *LLMProviderHandler) pump(c *gin.Context, body io.Reader, kind string) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	clientGone := c.Request.Context().Done()
	var dataLines []string
	flush := func() {
		if len(dataLines) == 0 {
			return
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		if normalizeLLMFrame(c.Writer, kind, payload) {
			c.Writer.Flush()
		}
	}
	for scanner.Scan() {
		select {
		case <-clientGone:
			return
		default:
		}
		line := scanner.Text()
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, "data:") {
			d := strings.TrimPrefix(line, "data:")
			d = strings.TrimPrefix(d, " ")
			dataLines = append(dataLines, d)
		}
		// event:/id:/comment lines are ignored — we key off each JSON payload's
		// own "type"/"choices" so the two provider shapes share this loop.
	}
	flush()
	if err := scanner.Err(); err != nil {
		emitLLMError(c.Writer, "stream error: "+err.Error())
		c.Writer.Flush()
	}
	writeSSEEvent(c.Writer, "done", "{}")
	c.Writer.Flush()
}

// normalizeLLMFrame parses one provider SSE data payload and, if it carries a
// token or error, writes the corresponding PatchMan frame. Returns whether it
// wrote anything (so pump only flushes on real output).
func normalizeLLMFrame(w io.Writer, kind, payload string) bool {
	switch kind {
	case "openai":
		if payload == "[DONE]" {
			return false
		}
		var ev struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(payload), &ev) != nil {
			return false
		}
		if ev.Error != nil {
			emitLLMError(w, ev.Error.Message)
			return true
		}
		wrote := false
		for _, ch := range ev.Choices {
			if ch.Delta.Content != "" {
				emitLLMToken(w, ch.Delta.Content)
				wrote = true
			}
		}
		return wrote
	case "anthropic":
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				Text string `json:"text"`
			} `json:"delta"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(payload), &ev) != nil {
			return false
		}
		switch ev.Type {
		case "content_block_delta":
			if ev.Delta.Text != "" {
				emitLLMToken(w, ev.Delta.Text)
				return true
			}
		case "error":
			msg := "provider error"
			if ev.Error != nil && ev.Error.Message != "" {
				msg = ev.Error.Message
			}
			emitLLMError(w, msg)
			return true
		}
		return false
	}
	return false
}

func emitLLMToken(w io.Writer, text string) {
	b, _ := json.Marshal(map[string]string{"text": text})
	writeSSEEvent(w, "token", string(b))
}

func emitLLMError(w io.Writer, msg string) {
	b, _ := json.Marshal(map[string]string{"message": msg})
	writeSSEEvent(w, "error", string(b))
}
