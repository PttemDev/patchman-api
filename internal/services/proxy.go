package services

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

type ProxyRequest struct {
	Method          string            `json:"method"`
	URL             string            `json:"url"`
	Headers         map[string]string `json:"headers"`
	Params          map[string]string `json:"params"`
	Body            *RequestBody      `json:"body"`
	Auth            *RequestAuth      `json:"auth"`
	Timeout         int               `json:"timeout"`         // seconds
	FollowRedirects bool             `json:"followRedirects"`
	VerifySSL       bool             `json:"verifySsl"`
}

type FormField struct {
	Key      string `json:"key"`
	Value    string `json:"value"`
	Type     string `json:"type"`     // "text" or "file"
	Enabled  bool   `json:"enabled"`
	FileName string `json:"fileName,omitempty"`
	FileType string `json:"fileType,omitempty"`
}

type RequestBody struct {
	Type       string      `json:"type"`    // json, form, xml, raw, binary, graphql, soap, none
	Content    string      `json:"content"`
	FormFields []FormField `json:"formFields,omitempty"`
	// FormEncoding disambiguates a "form" body: "multipart" forces
	// multipart/form-data even for text-only fields (curl -F semantics),
	// "urlencoded" forces application/x-www-form-urlencoded. Empty falls back
	// to the legacy heuristic (multipart iff a file field is present).
	FormEncoding string `json:"formEncoding,omitempty"`
	// SOAP metadata (mode "soap"). Only consumed by code generation to emit
	// the right Content-Type + SOAPAction; the live proxy send path uses the
	// caller-supplied headers (the FE derives them at send time).
	SoapVersion string `json:"soapVersion,omitempty"`
	SoapAction  string `json:"soapAction,omitempty"`
}

type RequestAuth struct {
	Type     string `json:"type"`     // none, bearer, basic, apikey
	Token    string `json:"token"`    // for bearer
	Username string `json:"username"` // for basic
	Password string `json:"password"` // for basic
	Key      string `json:"key"`      // for apikey
	Value    string `json:"value"`    // for apikey
	AddTo    string `json:"addTo"`    // header or query (for apikey)
}

type ProxyResponse struct {
	StatusCode      int               `json:"statusCode"`
	StatusText      string            `json:"statusText"`
	Headers         map[string]string `json:"headers"`
	Body            string            `json:"body"`
	// BodyBase64 is true when Body holds base64-encoded bytes instead of
	// text — set for binary responses (images, PDFs, audio/video, octet-
	// stream, or any non-UTF-8 payload). Without this, string(bodyBytes)
	// would mangle binary through JSON (invalid UTF-8 → U+FFFD) and the FE
	// couldn't render it. The FE decodes it back into a data: URL.
	BodyBase64      bool              `json:"bodyBase64"`
	Size            int64             `json:"size"`
	Duration        int64             `json:"duration"`        // ms
	Timing          ResponseTiming    `json:"timing"`
	ContentEncoding string            `json:"contentEncoding"` // gzip/deflate/br/zstd if the wire payload was compressed; empty otherwise
	// RemoteAddr is the actual resolved IP:port the upstream request connected
	// to (via httptrace GotConn) — like Postman's "request sent to" address.
	// Reflects the final host after any redirects; empty if no connection was
	// ever established (DNS/connect failure).
	RemoteAddr      string            `json:"remoteAddr"`
}

// isBinaryResponse decides whether a response body should be base64-encoded
// for transport (so the FE can render images/PDF/media and round-trip the
// raw bytes). Text-ish content types stay as strings; known binary types
// or any non-UTF-8 payload are treated as binary.
func isBinaryResponse(contentType string, body []byte) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	// Explicitly textual — keep as string even if the CT is uncommon.
	if strings.HasPrefix(ct, "text/") ||
		ct == "image/svg+xml" ||
		strings.Contains(ct, "json") ||
		strings.Contains(ct, "xml") ||
		strings.Contains(ct, "javascript") ||
		strings.Contains(ct, "ecmascript") ||
		strings.Contains(ct, "x-www-form-urlencoded") ||
		strings.Contains(ct, "csv") {
		return false
	}
	// Known binary families.
	if strings.HasPrefix(ct, "image/") ||
		strings.HasPrefix(ct, "audio/") ||
		strings.HasPrefix(ct, "video/") ||
		strings.HasPrefix(ct, "font/") ||
		ct == "application/pdf" ||
		ct == "application/octet-stream" ||
		ct == "application/zip" ||
		ct == "application/wasm" ||
		strings.Contains(ct, "protobuf") {
		return true
	}
	// Fallback: anything that isn't valid UTF-8 is binary.
	return len(body) > 0 && !utf8.Valid(body)
}

type ResponseTiming struct {
	DNS   int64 `json:"dns"`   // ms
	TCP   int64 `json:"tcp"`   // ms
	TLS   int64 `json:"tls"`   // ms
	TTFB  int64 `json:"ttfb"`  // ms
	Total int64 `json:"total"` // ms
}

// ExecuteRequest runs a proxied request with no external cancellation (used by
// the collection runner + data-driven paths, which bound themselves via the
// per-request timeout). The interactive Send path uses ExecuteRequestCtx.
func ExecuteRequest(req *ProxyRequest) (*ProxyResponse, error) {
	return ExecuteRequestCtx(context.Background(), req)
}

// ExecuteRequestCtx runs a proxied request tied to ctx: when the caller's
// context is cancelled — the client aborted the send or the client connection
// dropped — the in-flight upstream request is cancelled instead of running to
// completion (so a timed-out/cancelled request doesn't keep the API working).
func ExecuteRequestCtx(ctx context.Context, req *ProxyRequest) (*ProxyResponse, error) {
	// Build URL with params
	targetURL, err := url.Parse(req.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}

	q := targetURL.Query()
	for k, v := range req.Params {
		q.Set(k, v)
	}

	// Handle apikey in query params
	if req.Auth != nil && req.Auth.Type == "apikey" && req.Auth.AddTo == "query" {
		q.Set(req.Auth.Key, req.Auth.Value)
	}
	targetURL.RawQuery = q.Encode()

	// Build body
	var bodyReader io.Reader
	var contentTypeOverride string

	if req.Body != nil && req.Body.Type == "form" && len(req.Body.FormFields) > 0 {
		// Structured form fields — check if any are file fields
		hasFile := false
		for _, f := range req.Body.FormFields {
			if f.Type == "file" && f.Enabled {
				hasFile = true
				break
			}
		}

		// curl's -F is always multipart/form-data, even with no file fields, so
		// honor an explicit multipart request; otherwise a file field forces
		// multipart and everything else is url-encoded. Without this, an
		// imported `-F` text-only form would go out url-encoded and a server
		// expecting multipart answers 415.
		if hasFile || req.Body.FormEncoding == "multipart" {
			buf, contentType := buildMultipartForm(req.Body.FormFields)
			bodyReader = buf
			contentTypeOverride = contentType
		} else {
			// URL-encoded for text-only form fields
			values := url.Values{}
			for _, f := range req.Body.FormFields {
				if f.Enabled && f.Key != "" {
					values.Add(f.Key, f.Value)
				}
			}
			bodyReader = strings.NewReader(values.Encode())
		}
	} else if req.Body != nil && req.Body.Type != "none" && req.Body.Content != "" {
		bodyReader = strings.NewReader(req.Body.Content)
	}

	// Create HTTP request — tied to ctx so a client abort/disconnect cancels
	// the upstream request instead of letting it run to completion.
	httpReq, err := http.NewRequestWithContext(ctx, strings.ToUpper(req.Method), targetURL.String(), bodyReader)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}

	// Advertise every encoding we can decode. We only override if the caller
	// didn't set one explicitly, so users can still force a specific value.
	if httpReq.Header.Get("Accept-Encoding") == "" {
		httpReq.Header.Set("Accept-Encoding", DefaultAcceptEncoding)
	}

	// Set content-type
	if contentTypeOverride != "" {
		httpReq.Header.Set("Content-Type", contentTypeOverride)
	} else if req.Body != nil && httpReq.Header.Get("Content-Type") == "" {
		switch req.Body.Type {
		case "json":
			httpReq.Header.Set("Content-Type", "application/json")
		case "xml":
			httpReq.Header.Set("Content-Type", "application/xml")
		case "form":
			httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		case "graphql":
			httpReq.Header.Set("Content-Type", "application/json")
		}
	}

	// Apply auth
	if req.Auth != nil {
		switch req.Auth.Type {
		case "bearer":
			httpReq.Header.Set("Authorization", "Bearer "+req.Auth.Token)
		case "basic":
			credentials := base64.StdEncoding.EncodeToString(
				[]byte(req.Auth.Username + ":" + req.Auth.Password))
			httpReq.Header.Set("Authorization", "Basic "+credentials)
		case "apikey":
			if req.Auth.AddTo != "query" {
				httpReq.Header.Set(req.Auth.Key, req.Auth.Value)
			}
		}
	}

	// Setup timing with httptrace
	var dnsStart, dnsEnd, connectStart, connectEnd, tlsStart, tlsEnd, gotFirstByte time.Time
	var remoteAddr string
	requestStart := time.Now()

	trace := &httptrace.ClientTrace{
		DNSStart: func(_ httptrace.DNSStartInfo) {
			dnsStart = time.Now()
		},
		DNSDone: func(_ httptrace.DNSDoneInfo) {
			dnsEnd = time.Now()
		},
		ConnectStart: func(_, _ string) {
			connectStart = time.Now()
		},
		ConnectDone: func(_, _ string, _ error) {
			connectEnd = time.Now()
		},
		TLSHandshakeStart: func() {
			tlsStart = time.Now()
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, _ error) {
			tlsEnd = time.Now()
		},
		GotFirstResponseByte: func() {
			gotFirstByte = time.Now()
		},
		// Capture the actual remote IP:port the request connected to — the
		// final host after any redirects (fires again on each new connection,
		// so the last value wins).
		GotConn: func(info httptrace.GotConnInfo) {
			if info.Conn != nil {
				remoteAddr = info.Conn.RemoteAddr().String()
			}
		},
	}

	httpReq = httpReq.WithContext(httptrace.WithClientTrace(httpReq.Context(), trace))

	// Configure client
	timeout := 30 * time.Second
	if req.Timeout > 0 {
		timeout = time.Duration(req.Timeout) * time.Second
	}

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: !req.VerifySSL,
		},
		// We set Accept-Encoding ourselves and decode the body via
		// DecodeResponseBody so we can handle gzip/deflate/br/zstd uniformly.
		// Leaving Go's transparent gzip path on would strip Content-Encoding
		// before we see it and prevent us from decoding the other algorithms.
		DisableCompression: true,
		// A fresh transport is built per request (per-request TLS/verify
		// settings), so keep-alives MUST be off: otherwise each request's
		// idle connection is retained by a transport that's never closed
		// (IdleConnTimeout 0 = never), leaking an fd + goroutines per Send.
		// In the embedded desktop sidecar this eventually exhausts the fd
		// table and the server stops accepting connections until restart
		// (the "requests fail with Network Error after a while" bug).
		DisableKeepAlives: true,
	}

	client := &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}

	if !req.FollowRedirects {
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	// Execute request
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	requestEnd := time.Now()

	// Read + transparently decode response body (gzip/deflate/br/zstd).
	bodyBytes, decodedFrom, err := DecodeResponseBodyEncoding(resp, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	// Calculate timing
	totalDuration := requestEnd.Sub(requestStart).Milliseconds()

	var dnsDuration, tcpDuration, tlsDuration, ttfbDuration int64
	if !dnsStart.IsZero() && !dnsEnd.IsZero() {
		dnsDuration = dnsEnd.Sub(dnsStart).Milliseconds()
	}
	if !connectStart.IsZero() && !connectEnd.IsZero() {
		tcpDuration = connectEnd.Sub(connectStart).Milliseconds()
	}
	if !tlsStart.IsZero() && !tlsEnd.IsZero() {
		tlsDuration = tlsEnd.Sub(tlsStart).Milliseconds()
	}
	if !gotFirstByte.IsZero() {
		ttfbDuration = gotFirstByte.Sub(requestStart).Milliseconds()
	}

	// Build response headers map
	responseHeaders := make(map[string]string)
	for k, v := range resp.Header {
		responseHeaders[k] = strings.Join(v, ", ")
	}

	bodyBinary := isBinaryResponse(responseHeaders["Content-Type"]+responseHeaders["content-type"], bodyBytes)
	bodyStr := string(bodyBytes)
	if bodyBinary {
		bodyStr = base64.StdEncoding.EncodeToString(bodyBytes)
	}

	return &ProxyResponse{
		StatusCode:      resp.StatusCode,
		StatusText:      resp.Status,
		Headers:         responseHeaders,
		Body:            bodyStr,
		BodyBase64:      bodyBinary,
		Size:            int64(len(bodyBytes)),
		Duration:        totalDuration,
		ContentEncoding: decodedFrom,
		RemoteAddr:      remoteAddr,
		Timing: ResponseTiming{
			DNS:   dnsDuration,
			TCP:   tcpDuration,
			TLS:   tlsDuration,
			TTFB:  ttfbDuration,
			Total: totalDuration,
		},
	}, nil
}

func escapeQuotes(s string) string {
	return strings.NewReplacer("\\", "\\\\", `"`, "\\\"").Replace(s)
}

// buildMultipartForm renders enabled form fields as a multipart/form-data
// body and returns the buffer plus the Content-Type (with the generated
// boundary). File fields carry a base64 value + optional filename/MIME;
// everyone else is a plain text part. The returned Content-Type MUST be set
// on the request so the boundary matches the body.
func buildMultipartForm(fields []FormField) (*bytes.Buffer, string) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	for _, f := range fields {
		if !f.Enabled || f.Key == "" {
			continue
		}
		if f.Type == "file" && f.Value != "" {
			fileData, err := base64.StdEncoding.DecodeString(f.Value)
			if err != nil {
				continue
			}
			fileName := f.FileName
			if fileName == "" {
				fileName = "file"
			}
			h := make(textproto.MIMEHeader)
			h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, escapeQuotes(f.Key), escapeQuotes(fileName)))
			if f.FileType != "" {
				h.Set("Content-Type", f.FileType)
			} else {
				h.Set("Content-Type", "application/octet-stream")
			}
			part, err := writer.CreatePart(h)
			if err != nil {
				continue
			}
			part.Write(fileData)
		} else {
			writer.WriteField(f.Key, f.Value)
		}
	}
	writer.Close()
	return &buf, writer.FormDataContentType()
}
