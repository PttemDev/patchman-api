package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"patchman-be/internal/models"
)

// llmComplete makes ONE non-streaming completion call to a saved provider
// (OpenAI Chat Completions or Anthropic Messages). Shared by the diagram loop
// and agent memory's card splitter.
func llmComplete(ctx context.Context, client *http.Client, prov *models.LLMProvider, key, model, system string, msgs []map[string]string, maxTokens int) (string, error) {
	base := strings.TrimRight(prov.BaseURL, "/")
	switch prov.Kind {
	case "openai":
		oaMsgs := make([]map[string]string, 0, len(msgs)+1)
		oaMsgs = append(oaMsgs, map[string]string{"role": "system", "content": system})
		oaMsgs = append(oaMsgs, msgs...)
		body, _ := json.Marshal(map[string]any{"model": model, "stream": false, "max_tokens": maxTokens, "messages": oaMsgs})
		var parsed struct {
			Choices []struct {
				Message struct{ Content string } `json:"message"`
			} `json:"choices"`
		}
		if err := llmPostJSON(ctx, client, base+"/chat/completions", key, "openai", body, &parsed); err != nil {
			return "", err
		}
		if len(parsed.Choices) == 0 {
			return "", fmt.Errorf("empty completion")
		}
		return parsed.Choices[0].Message.Content, nil
	case "anthropic":
		body, _ := json.Marshal(map[string]any{"model": model, "system": system, "max_tokens": maxTokens, "stream": false, "messages": msgs})
		var parsed struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := llmPostJSON(ctx, client, base+"/v1/messages", key, "anthropic", body, &parsed); err != nil {
			return "", err
		}
		var sb strings.Builder
		for _, c := range parsed.Content {
			if c.Type == "text" {
				sb.WriteString(c.Text)
			}
		}
		return sb.String(), nil
	}
	return "", fmt.Errorf("unsupported provider kind %q", prov.Kind)
}

func llmPostJSON(ctx context.Context, client *http.Client, url, key, kind string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if kind == "anthropic" {
		req.Header.Set("anthropic-version", "2023-06-01")
		if key != "" {
			req.Header.Set("x-api-key", key)
		}
	} else if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("connect to provider: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("provider %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return json.Unmarshal(raw, out)
}
