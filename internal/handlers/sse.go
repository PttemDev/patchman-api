package handlers

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type SSEHandler struct{}

func NewSSEHandler() *SSEHandler {
	return &SSEHandler{}
}

type sseConnectRequest struct {
	URL     string            `json:"url" binding:"required"`
	Headers map[string]string `json:"headers"`
}

// Connect opens a connection to the target SSE endpoint and proxies events
// back to the frontend via a streaming response.
// POST /api/sse/connect
func (h *SSEHandler) Connect(c *gin.Context) {
	var req sseConnectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Build the outgoing HTTP request to the target SSE server
	httpReq, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, req.URL, nil)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid URL: " + err.Error()})
		return
	}

	// Set default SSE headers
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Cache-Control", "no-cache")
	httpReq.Header.Set("Connection", "keep-alive")

	// Apply user-provided headers (may override defaults)
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}

	client := &http.Client{
		Timeout: 0, // No timeout for SSE streaming
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Failed to connect to SSE server: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	// If the target didn't return 200, forward the error
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		c.JSON(resp.StatusCode, gin.H{
			"error":      fmt.Sprintf("Target server returned %s", resp.Status),
			"statusCode": resp.StatusCode,
			"body":       string(body),
		})
		return
	}

	// Set streaming response headers
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no") // Disable nginx buffering if behind a proxy

	// Forward target response headers as an initial SSE event so the frontend
	// can inspect them
	var headerParts []string
	for k, v := range resp.Header {
		headerParts = append(headerParts, fmt.Sprintf("%s: %s", k, strings.Join(v, ", ")))
	}
	if len(headerParts) > 0 {
		writeSSEEvent(c.Writer, "headers", strings.Join(headerParts, "\n"))
		c.Writer.Flush()
	}

	// Send a connected event
	writeSSEEvent(c.Writer, "connected", fmt.Sprintf(`{"url":"%s","status":%d}`, req.URL, resp.StatusCode))
	c.Writer.Flush()

	// Stream SSE events from the target to the frontend
	scanner := bufio.NewScanner(resp.Body)
	// Increase scanner buffer for large SSE payloads (up to 1MB)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var (
		eventType string
		dataLines []string
		eventID   string
		retry     string
	)

	clientGone := c.Request.Context().Done()

	for scanner.Scan() {
		select {
		case <-clientGone:
			log.Printf("SSE client disconnected, closing target connection")
			return
		default:
		}

		line := scanner.Text()

		// An empty line signals the end of an event
		if line == "" {
			if len(dataLines) > 0 || eventType != "" {
				h.flushEvent(c.Writer, eventType, dataLines, eventID, retry)
				c.Writer.Flush()

				// Reset for the next event
				eventType = ""
				dataLines = nil
				eventID = ""
				retry = ""
			}
			continue
		}

		// Parse SSE fields
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimPrefix(line, "data:")
			if len(data) > 0 && data[0] == ' ' {
				data = data[1:]
			}
			dataLines = append(dataLines, data)
		} else if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "id:") {
			eventID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		} else if strings.HasPrefix(line, "retry:") {
			retry = strings.TrimSpace(strings.TrimPrefix(line, "retry:"))
		} else if strings.HasPrefix(line, ":") {
			// Comment line -- forward as-is for transparency
			fmt.Fprintf(c.Writer, "%s\n", line)
			c.Writer.Flush()
		} else {
			// Unknown field -- forward raw line as data
			dataLines = append(dataLines, line)
		}
	}

	// Flush any remaining partial event
	if len(dataLines) > 0 || eventType != "" {
		h.flushEvent(c.Writer, eventType, dataLines, eventID, retry)
		c.Writer.Flush()
	}

	if err := scanner.Err(); err != nil {
		log.Printf("SSE scanner error: %v", err)
		writeSSEEvent(c.Writer, "error", fmt.Sprintf(`{"message":"Stream error: %s"}`, err.Error()))
		c.Writer.Flush()
	}

	// Notify the frontend the stream has ended
	writeSSEEvent(c.Writer, "done", `{"message":"Stream ended"}`)
	c.Writer.Flush()
}

// flushEvent writes a single SSE event to the response writer.
func (h *SSEHandler) flushEvent(w io.Writer, eventType string, dataLines []string, id string, retry string) {
	if id != "" {
		fmt.Fprintf(w, "id: %s\n", id)
	}
	if retry != "" {
		fmt.Fprintf(w, "retry: %s\n", retry)
	}
	if eventType != "" {
		fmt.Fprintf(w, "event: %s\n", eventType)
	}
	for _, d := range dataLines {
		fmt.Fprintf(w, "data: %s\n", d)
	}
	// Empty line terminates the event
	fmt.Fprint(w, "\n")
}

// writeSSEEvent is a convenience function that writes a single-field SSE event.
func writeSSEEvent(w io.Writer, event string, data string) {
	if event != "" {
		fmt.Fprintf(w, "event: %s\n", event)
	}
	// Split data on newlines so each line gets its own "data:" prefix (per SSE spec)
	for _, line := range strings.Split(data, "\n") {
		fmt.Fprintf(w, "data: %s\n", line)
	}
	fmt.Fprint(w, "\n")
}

// ConnectGET provides a GET-based SSE endpoint for simpler use cases where
// the target URL and headers are passed as query parameters.
// GET /api/sse/connect?url=...&header_Key=Value
func (h *SSEHandler) ConnectGET(c *gin.Context) {
	url := c.Query("url")
	if url == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "url query parameter is required"})
		return
	}

	// Extract headers from query params prefixed with "header_"
	headers := make(map[string]string)
	for key, values := range c.Request.URL.Query() {
		if strings.HasPrefix(key, "header_") && len(values) > 0 {
			headerName := strings.TrimPrefix(key, "header_")
			headers[headerName] = values[0]
		}
	}

	// Build the outgoing HTTP request
	httpReq, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, url, nil)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid URL: " + err.Error()})
		return
	}

	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Cache-Control", "no-cache")
	httpReq.Header.Set("Connection", "keep-alive")

	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}

	client := &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
			IdleConnTimeout: 0,
		},
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Failed to connect to SSE server: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		c.JSON(resp.StatusCode, gin.H{
			"error":      fmt.Sprintf("Target server returned %s", resp.Status),
			"statusCode": resp.StatusCode,
			"body":       string(body),
		})
		return
	}

	// Stream response
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	writeSSEEvent(c.Writer, "connected", fmt.Sprintf(`{"url":"%s","status":%d}`, url, resp.StatusCode))
	c.Writer.Flush()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var (
		eventType string
		dataLines []string
		eventID   string
		retry     string
	)

	clientGone := c.Request.Context().Done()

	for scanner.Scan() {
		select {
		case <-clientGone:
			return
		default:
		}

		line := scanner.Text()

		if line == "" {
			if len(dataLines) > 0 || eventType != "" {
				h.flushEvent(c.Writer, eventType, dataLines, eventID, retry)
				c.Writer.Flush()
				eventType = ""
				dataLines = nil
				eventID = ""
				retry = ""
			}
			continue
		}

		if strings.HasPrefix(line, "data:") {
			data := strings.TrimPrefix(line, "data:")
			if len(data) > 0 && data[0] == ' ' {
				data = data[1:]
			}
			dataLines = append(dataLines, data)
		} else if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "id:") {
			eventID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		} else if strings.HasPrefix(line, "retry:") {
			retry = strings.TrimSpace(strings.TrimPrefix(line, "retry:"))
		} else if strings.HasPrefix(line, ":") {
			fmt.Fprintf(c.Writer, "%s\n", line)
			c.Writer.Flush()
		} else {
			dataLines = append(dataLines, line)
		}
	}

	if len(dataLines) > 0 || eventType != "" {
		h.flushEvent(c.Writer, eventType, dataLines, eventID, retry)
		c.Writer.Flush()
	}

	if err := scanner.Err(); err != nil {
		writeSSEEvent(c.Writer, "error", fmt.Sprintf(`{"message":"Stream error: %s"}`, err.Error()))
		c.Writer.Flush()
	}

	writeSSEEvent(c.Writer, "done", `{"message":"Stream ended"}`)
	c.Writer.Flush()
}

// Ping is a simple SSE test endpoint that sends periodic pings.
// Useful for verifying the SSE pipeline works end-to-end.
// GET /api/sse/ping
func (h *SSEHandler) Ping(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	clientGone := c.Request.Context().Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// Send initial event
	writeSSEEvent(c.Writer, "ping", fmt.Sprintf(`{"time":"%s"}`, time.Now().UTC().Format(time.RFC3339)))
	c.Writer.Flush()

	for {
		select {
		case <-clientGone:
			return
		case t := <-ticker.C:
			writeSSEEvent(c.Writer, "ping", fmt.Sprintf(`{"time":"%s"}`, t.UTC().Format(time.RFC3339)))
			c.Writer.Flush()
		}
	}
}
