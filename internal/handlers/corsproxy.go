package handlers

import (
	"io"
	"net/http"
	"strings"
	"time"

	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
)

// CORSProxyHandler is a lightweight transparent proxy that bypasses CORS restrictions.
// Unlike the full proxy handler, it returns raw response data without timing overhead.
type CORSProxyHandler struct{}

func NewCORSProxyHandler() *CORSProxyHandler {
	return &CORSProxyHandler{}
}

type corsProxyRequest struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

func (h *CORSProxyHandler) Proxy(c *gin.Context) {
	var req corsProxyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	if req.URL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "URL is required"})
		return
	}

	if req.Method == "" {
		req.Method = "GET"
	}

	// Build HTTP request
	var bodyReader io.Reader
	if req.Body != "" {
		bodyReader = strings.NewReader(req.Body)
	}

	httpReq, err := http.NewRequest(req.Method, req.URL, bodyReader)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to create request: " + err.Error()})
		return
	}

	// Set headers
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}

	if httpReq.Header.Get("Accept-Encoding") == "" {
		httpReq.Header.Set("Accept-Encoding", services.DefaultAcceptEncoding)
	}

	// Execute with timeout. DisableCompression lets us decode the body
	// ourselves so gzip/deflate/br/zstd all flow through the same path.
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DisableCompression: true,
			// Per-request transport is never reused/closed — disable
			// keep-alives so idle connections don't leak fds over time.
			DisableKeepAlives: true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}

	start := time.Now()
	resp, err := client.Do(httpReq)
	duration := time.Since(start).Milliseconds()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"data": gin.H{
				"statusCode": 0,
				"statusText": "Error",
				"headers":    map[string]string{},
				"body":       "CORS Proxy Error: " + err.Error(),
				"size":       0,
				"duration":   duration,
			},
		})
		return
	}
	defer resp.Body.Close()

	// Read + decode response body (gzip/deflate/br/zstd) with a 10MB cap.
	body, decodedFrom, err := services.DecodeResponseBodyEncoding(resp, 10*1024*1024)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"data": gin.H{
				"statusCode": resp.StatusCode,
				"statusText": resp.Status,
				"headers":    map[string]string{},
				"body":       "Error reading response body: " + err.Error(),
				"size":       0,
				"duration":   duration,
			},
		})
		return
	}

	// Collect response headers
	respHeaders := make(map[string]string)
	for k, v := range resp.Header {
		if len(v) > 0 {
			respHeaders[k] = v[0]
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"statusCode":      resp.StatusCode,
			"statusText":      resp.Status,
			"headers":         respHeaders,
			"body":            string(body),
			"size":            len(body),
			"duration":        duration,
			"contentEncoding": decodedFrom,
		},
	})
}
