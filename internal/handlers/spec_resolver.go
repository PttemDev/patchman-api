package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// SpecResolverHandler resolves a user-supplied URL to an OpenAPI/Swagger
// JSON document. The user might paste:
//
//   1. A direct JSON URL (e.g. /openapi.json)            → return as-is
//   2. A docs UI page rendered by Scalar / Swagger UI /
//      Redoc                                             → scrape the HTML
//                                                          for the embedded
//                                                          spec URL, fetch
//                                                          that
//   3. A "looks like docs" URL where common conventions
//      apply but the HTML doesn't expose a spec link     → probe a small
//                                                          set of well-known
//                                                          endpoints
//
// Done server-side because (a) CORS would block the browser from reading
// most third-party docs pages and (b) it gives us a single place to bound
// the fetch fan-out + timeout.
type SpecResolverHandler struct {
	http *http.Client
}

func NewSpecResolverHandler() *SpecResolverHandler {
	return &SpecResolverHandler{
		http: &http.Client{Timeout: 15 * time.Second},
	}
}

type resolveSpecInput struct {
	URL string `json:"url" binding:"required"`
}

type resolveSpecOutput struct {
	// The actual spec JSON as a string. The caller's existing parser
	// runs on this directly.
	Spec string `json:"spec"`
	// Where we ultimately found it. Surfaces in the success toast so
	// the user knows what we resolved to.
	ResolvedURL string `json:"resolvedUrl"`
	// Hint about how we got there: 'direct' | 'scalar' | 'swagger-ui' |
	// 'redoc' | 'probe'. Cheap context for analytics + debugging.
	Source string `json:"source"`
}

func (h *SpecResolverHandler) Resolve(c *gin.Context) {
	var in resolveSpecInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	raw := strings.TrimSpace(in.URL)
	if raw == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "URL is required"})
		return
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "URL must be http(s)"})
		return
	}
	// Strip the fragment — most docs pages put the section in the hash
	// and the server doesn't see it anyway, but it confuses our probe
	// candidates.
	parsed.Fragment = ""
	raw = parsed.String()

	out, err := h.resolve(raw, parsed)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// ResolveOpenAPISpec is the package-level entry point for other
// handlers (Sync refresh, FE-side imports) that need to follow a
// user-supplied URL all the way to the underlying OpenAPI JSON.
// Returns the spec bytes, the URL it ultimately resolved to (for
// telemetry/debug), the source kind ("direct" / "scalar" / etc), and
// an error. Strips the fragment so a Scalar deep-link
// (.../scalar/#tag/foo/POST/...) doesn't confuse the URL parser or
// the path probing.
func ResolveOpenAPISpec(rawURL string) (specBytes []byte, resolvedURL string, source string, err error) {
	parsed, perr := url.Parse(strings.TrimSpace(rawURL))
	if perr != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, "", "", fmt.Errorf("URL must be http(s)")
	}
	parsed.Fragment = ""
	raw := parsed.String()
	h := &SpecResolverHandler{http: &http.Client{Timeout: 15 * time.Second}}
	out, rerr := h.resolve(raw, parsed)
	if rerr != nil {
		return nil, "", "", rerr
	}
	return []byte(out.Spec), out.ResolvedURL, out.Source, nil
}

func (h *SpecResolverHandler) resolve(raw string, parsed *url.URL) (*resolveSpecOutput, error) {
	body, ctype, err := h.fetch(raw)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", raw, err)
	}
	// 1. Direct JSON?
	if isJSONLike(body, ctype) {
		return &resolveSpecOutput{Spec: string(body), ResolvedURL: raw, Source: "direct"}, nil
	}
	// 2. Recognised renderer? Scrape its embedded spec URL.
	if specURL := extractSpecURL(string(body)); specURL != "" {
		// Try the URL resolved both ways: against the docs page (RFC
		// 3986 default) and against the host root. Some renderers
		// (Scalar's .NET integration in particular) put the spec at a
		// host-root path even though the value in the config looks
		// relative to the docs directory.
		candidates := []string{absolutise(specURL, parsed)}
		if !strings.HasPrefix(specURL, "/") && !strings.HasPrefix(specURL, "http") {
			rooted := *parsed
			rooted.Path = "/"
			rooted.RawQuery = ""
			rooted.Fragment = ""
			candidates = append(candidates, absolutise(specURL, &rooted))
		}
		seen := map[string]bool{}
		for _, full := range candidates {
			if seen[full] {
				continue
			}
			seen[full] = true
			body2, ctype2, err := h.fetch(full)
			if err == nil && isJSONLike(body2, ctype2) {
				return &resolveSpecOutput{
					Spec:        string(body2),
					ResolvedURL: full,
					Source:      detectRendererSource(string(body)),
				}, nil
			}
		}
	}
	// 3. Probe well-known paths under the host.
	candidates := buildProbeCandidates(parsed)
	for _, candidate := range candidates {
		body3, ctype3, err := h.fetch(candidate)
		if err != nil {
			continue
		}
		if isJSONLike(body3, ctype3) {
			return &resolveSpecOutput{
				Spec:        string(body3),
				ResolvedURL: candidate,
				Source:      "probe",
			}, nil
		}
	}
	return nil, errors.New("could not locate an OpenAPI/Swagger spec at that URL — paste a direct JSON URL or upload the file")
}

// ----------------------------------------------------------------------
// Renderer detection
// ----------------------------------------------------------------------

// extractSpecURL pulls a spec URL out of the rendered docs HTML. Covers
// the three popular renderers in PatchMan-land:
//
//   Scalar     — `<script id="api-reference" data-url="…">`,
//                `Scalar.createApiReference({ url: "…" })`,
//                or the .NET integration's `sources:[{url:"…"}]` blob
//   Swagger UI — `SwaggerUIBundle({ url: "…" })`, the bare
//                `SwaggerUI({...})` constructor, OR the Swashbuckle .NET
//                page which serialises its full config through
//                `JSON.parse('{"urls":[{"url":"…","name":"…"}], …}')`
//                (the "url" field nested under a "urls" array — distinct
//                from Scalar's "sources" array)
//   Redoc      — `<redoc spec-url="…">` / `Redoc.init('…')`
func extractSpecURL(html string) string {
	patterns := []*regexp.Regexp{
		// Scalar — most precise first
		regexp.MustCompile(`<script[^>]+id=["']api-reference["'][^>]+data-url=["']([^"']+)["']`),
		regexp.MustCompile(`<api-reference[^>]+data-url=["']([^"']+)["']`),
		regexp.MustCompile(`Scalar\.createApiReference\([^)]*url\s*:\s*["']([^"']+)["']`),
		// Scalar .NET integration: `sources:[{"title":"v1","url":"openapi/v1.json"}]`
		regexp.MustCompile(`"sources"\s*:\s*\[\s*\{[^}]*"url"\s*:\s*"([^"]+)"`),

		// Redoc
		regexp.MustCompile(`<redoc[^>]+spec-url=["']([^"']+)["']`),
		regexp.MustCompile(`Redoc\.init\(\s*["']([^"']+)["']`),

		// Swagger UI / Swashbuckle. The Swashbuckle template wraps its
		// whole config in JSON.parse('{...}'), so the raw bundle/regex
		// matchers below never see a literal `SwaggerUIBundle({url:…})`
		// — instead the spec URL appears inside a "urls":[{ "url": "…" }]
		// array. Match that first; fall back to the plain bundle calls
		// for hand-rolled pages.
		regexp.MustCompile(`"urls"\s*:\s*\[\s*\{[^}]*"url"\s*:\s*"([^"]+)"`),
		regexp.MustCompile(`SwaggerUIBundle\([^)]*url\s*:\s*["']([^"']+)["']`),
		regexp.MustCompile(`SwaggerUI\([^)]*url\s*:\s*["']([^"']+)["']`),
	}
	for _, re := range patterns {
		m := re.FindStringSubmatch(html)
		if len(m) >= 2 && m[1] != "" {
			return m[1]
		}
	}
	return ""
}

func detectRendererSource(html string) string {
	switch {
	case strings.Contains(html, "@scalar/api-reference"), strings.Contains(html, "Scalar.createApiReference"), strings.Contains(html, "id=\"api-reference\""):
		return "scalar"
	case strings.Contains(html, "swagger-ui"), strings.Contains(html, "SwaggerUI"):
		return "swagger-ui"
	case strings.Contains(html, "redoc"), strings.Contains(html, "Redoc"):
		return "redoc"
	default:
		return "probe"
	}
}

// ----------------------------------------------------------------------
// Path probing
// ----------------------------------------------------------------------

// buildProbeCandidates returns a small ordered list of well-known spec
// endpoints relative to the given URL — both alongside the docs path and
// at the host root. Includes Swashbuckle's versioned paths
// (`v1/swagger.json`, `v2/swagger.json`) so a `/swagger/index.html` URL
// resolves even if HTML scraping fails to extract the JSON.parse'd
// `urls:[…]` config (e.g. on a CSP-stripped or minified template).
func buildProbeCandidates(u *url.URL) []string {
	rel := []string{
		"openapi.json",
		"swagger.json",
		"v1/swagger.json",
		"v2/swagger.json",
		"v3/api-docs",
		"api-docs",
		"api-docs.json",
	}
	out := make([]string, 0, len(rel)*2)
	// Docs-relative first (e.g. /scalar/openapi.json)
	base := strings.TrimRight(u.Path, "/")
	if base == "" {
		base = ""
	}
	for _, p := range rel {
		out = append(out, fmt.Sprintf("%s://%s%s/%s", u.Scheme, u.Host, base, p))
	}
	// Then the host root
	for _, p := range rel {
		out = append(out, fmt.Sprintf("%s://%s/%s", u.Scheme, u.Host, p))
	}
	return out
}

// ----------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------

func (h *SpecResolverHandler) fetch(u string) ([]byte, string, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, "", err
	}
	// Set a polite UA — some servers redirect bots to a captcha page
	// when there's no UA at all.
	req.Header.Set("User-Agent", "PatchMan-Spec-Resolver/1.0")
	req.Header.Set("Accept", "application/json, text/html;q=0.5, */*;q=0.1")
	resp, err := h.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// Bound the read so a malicious endpoint can't ask us to slurp a 4 GB
	// "spec". 16 MB is generous for OpenAPI files in the wild.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, "", err
	}
	return body, resp.Header.Get("Content-Type"), nil
}

func isJSONLike(body []byte, ctype string) bool {
	if strings.Contains(strings.ToLower(ctype), "application/json") {
		return true
	}
	// Some servers send 'text/plain' for JSON payloads. Sniff the first
	// non-whitespace byte: '{' or '[' → JSON.
	for _, b := range body {
		if b == ' ' || b == '\t' || b == '\n' || b == '\r' {
			continue
		}
		if b == '{' || b == '[' {
			// Validate it actually parses — guards against an HTML page
			// that happens to start with '{'.
			var probe any
			return json.Unmarshal(body, &probe) == nil
		}
		return false
	}
	return false
}

func absolutise(specURL string, base *url.URL) string {
	parsed, err := url.Parse(specURL)
	if err != nil {
		return specURL
	}
	if parsed.IsAbs() {
		return parsed.String()
	}
	return base.ResolveReference(parsed).String()
}
