// Package webaudit implements a browser-less "Web Audit" checker for the Dev
// Tools surface. Audit fetches a single URL over HTTP (no browser, no JS
// execution), follows redirects while recording the chain, parses the returned
// HTML with golang.org/x/net/html, best-effort probes robots.txt + sitemap.xml,
// and rolls the findings up into a categorized checklist of SEO / social /
// structured-data / performance / security / crawlability issues.
//
// It is stateless and stdlib-plus-x/net only — no DB, no cipher, no connection
// registry — mirroring the DNS / codegen utility handlers so it runs on every
// BE (embedded + remote) regardless of configuration. All work is bounded: a
// ~15s HTTP timeout, a ~5MB body cap, a ~10-hop redirect cap, and short
// timeouts on the robots/sitemap probes that never fail the whole audit.
package webaudit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

// ErrInvalidURL is returned when the caller supplies a URL that is empty,
// unparseable, or not http(s). The handler maps it to a 400; every other
// failure (DNS, connection, timeout, …) maps to a 502.
var ErrInvalidURL = errors.New("invalid url: must be an absolute http(s) URL")

const (
	requestTimeout = 15 * time.Second      // overall HTML fetch budget
	probeTimeout   = 5 * time.Second        // robots.txt / sitemap.xml budget
	maxBodyBytes   = 5 << 20                 // 5MB HTML body cap
	maxRedirects   = 10                      // redirect-chain hop cap
	maxHeadings    = 50                      // heading-outline cap
	maxHeadingText = 300                     // per-heading text cap (runes)
	maxRobotsBytes = 64 << 10                // robots.txt read cap
	userAgent      = "Mozilla/5.0 (compatible; PatchManWebAudit/1.0; +https://patchman)"
)

// Issue categories rendered by the FE as grouped sections.
const (
	catSEO         = "SEO"
	catSocial      = "Social"
	catStructured  = "Structured data"
	catPerformance = "Performance"
	catSecurity    = "Security"
	catCrawl       = "Crawlability"
)

// Issue severities.
const (
	sevError   = "error"
	sevWarning = "warning"
	sevOK      = "ok"
	sevInfo    = "info"
)

// Redirect is one hop of the redirect chain (from → to at the given status).
type Redirect struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Status int    `json:"status"`
}

// TextField pairs an extracted value with its character (rune) length so the FE
// can flag length without recomputing.
type TextField struct {
	Value  string `json:"value"`
	Length int    `json:"length"`
}

// Hreflang is one <link rel=alternate hreflang> entry.
type Hreflang struct {
	Lang string `json:"lang"`
	Href string `json:"href"`
}

// Heading is one node in the h1–h6 outline.
type Heading struct {
	Level int    `json:"level"`
	Text  string `json:"text"`
}

// Images summarizes <img> alt coverage.
type Images struct {
	Total      int `json:"total"`
	MissingAlt int `json:"missingAlt"`
}

// StructuredData is a single JSON-LD @type value found on the page.
type StructuredData struct {
	Type string `json:"type"`
}

// SEO holds every extracted on-page SEO / social / structured-data signal.
type SEO struct {
	Title           TextField         `json:"title"`
	MetaDescription TextField         `json:"metaDescription"`
	Canonical       string            `json:"canonical"`
	RobotsMeta      string            `json:"robotsMeta"`
	Lang            string            `json:"lang"`
	Viewport        string            `json:"viewport"`
	Hreflang        []Hreflang        `json:"hreflang"`
	H1Count         int               `json:"h1Count"`
	H1              []string          `json:"h1"`
	Headings        []Heading         `json:"headings"`
	Images          Images            `json:"images"`
	OpenGraph       map[string]string `json:"openGraph"`
	Twitter         map[string]string `json:"twitter"`
	StructuredData  []StructuredData  `json:"structuredData"`
}

// Security records presence of hardening response headers.
type Security struct {
	HSTS                bool `json:"hsts"`
	CSP                 bool `json:"csp"`
	XFrameOptions       bool `json:"xFrameOptions"`
	XContentTypeOptions bool `json:"xContentTypeOptions"`
	ReferrerPolicy      bool `json:"referrerPolicy"`
}

// Quality holds transport-level quality signals.
type Quality struct {
	HTTPS           bool     `json:"https"`
	ContentEncoding string   `json:"contentEncoding"`
	CacheControl    string   `json:"cacheControl"`
	ContentType     string   `json:"contentType"`
	Bytes           int      `json:"bytes"`
	Security        Security `json:"security"`
}

// RobotsTxt is the best-effort robots.txt probe result.
type RobotsTxt struct {
	Present    bool   `json:"present"`
	URL        string `json:"url"`
	HasSitemap bool   `json:"hasSitemap"`
}

// Sitemap is the best-effort sitemap.xml probe result.
type Sitemap struct {
	Present bool   `json:"present"`
	URL     string `json:"url"`
}

// Issue is one entry in the rolled-up checklist. Why is a short, one-sentence
// "why it matters" rationale for the check (surfaced by the FE as a tooltip);
// it explains the check itself, so it's shared across a check's ok/warn/error
// branches.
type Issue struct {
	Severity string `json:"severity"`
	Category string `json:"category"`
	Message  string `json:"message"`
	Why      string `json:"why"`
}

// Report is the full audit result. It is the payload the handler wraps in
// {data: …}; the field/JSON shape mirrors the shared FE contract exactly.
type Report struct {
	URL        string     `json:"url"`
	FinalURL   string     `json:"finalUrl"`
	StatusCode int        `json:"statusCode"`
	OK         bool       `json:"ok"`
	FetchedAt  string     `json:"fetchedAt"`
	DurationMs int64      `json:"durationMs"`
	Redirects  []Redirect `json:"redirects"`
	SEO        SEO        `json:"seo"`
	Quality    Quality    `json:"quality"`
	RobotsTxt  RobotsTxt  `json:"robotsTxt"`
	Sitemap    Sitemap    `json:"sitemap"`
	Issues     []Issue    `json:"issues"`
}

// Audit fetches rawURL, parses the HTML, probes robots/sitemap, and returns a
// fully-populated Report. It returns ErrInvalidURL for a malformed/non-http(s)
// input and a wrapped transport error if the page can't be fetched.
func Audit(ctx context.Context, rawURL string, followRedirects bool) (*Report, error) {
	rawURL = strings.TrimSpace(rawURL)
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, ErrInvalidURL
	}
	if s := strings.ToLower(parsed.Scheme); s != "http" && s != "https" {
		return nil, ErrInvalidURL
	}

	// Redirect chain is recorded via CheckRedirect. The client runs a single
	// request sequentially, so the closure needs no synchronization. Transport
	// is left nil on purpose so the shared http.DefaultTransport pools
	// connections — a fresh *http.Transport per call would leak idle fds.
	redirects := make([]Redirect, 0, 4)
	client := &http.Client{
		Timeout: requestTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			status := 0
			if req.Response != nil {
				status = req.Response.StatusCode
			}
			from := ""
			if len(via) > 0 {
				from = via[len(via)-1].URL.String()
			}
			redirects = append(redirects, Redirect{From: from, To: req.URL.String(), Status: status})
			// Don't follow when the caller opted out, or past the hop cap. Return
			// ErrUseLastResponse (keep the 3xx response readable) rather than
			// erroring; the non-2xx status then surfaces naturally as an issue.
			if !followRedirects || len(redirects) >= maxRedirects {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, ErrInvalidURL
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch failed: %w", err)
	}
	defer resp.Body.Close()

	// Read the raw body (capped) first: len(buf) is the true bytes-on-wire size
	// after transport-level gzip decompression, kept distinct from the runes
	// we later parse.
	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("read body failed: %w", err)
	}
	durationMs := time.Since(start).Milliseconds()

	finalURL := rawURL
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}

	// Go's transport transparently decompresses gzip and strips the header,
	// flagging resp.Uncompressed; surface that as "gzip" for the report.
	contentEncoding := resp.Header.Get("Content-Encoding")
	if contentEncoding == "" && resp.Uncompressed {
		contentEncoding = "gzip"
	}
	contentType := resp.Header.Get("Content-Type")

	report := &Report{
		URL:        rawURL,
		FinalURL:   finalURL,
		StatusCode: resp.StatusCode,
		OK:         resp.StatusCode >= 200 && resp.StatusCode < 300,
		FetchedAt:  time.Now().UTC().Format(time.RFC3339),
		DurationMs: durationMs,
		Redirects:  redirects,
		SEO: SEO{
			Hreflang:       make([]Hreflang, 0),
			H1:             make([]string, 0),
			Headings:       make([]Heading, 0),
			OpenGraph:      make(map[string]string),
			Twitter:        make(map[string]string),
			StructuredData: make([]StructuredData, 0),
		},
		Quality: Quality{
			HTTPS:           strings.EqualFold(schemeOf(finalURL), "https"),
			ContentEncoding: contentEncoding,
			CacheControl:    resp.Header.Get("Cache-Control"),
			ContentType:     contentType,
			Bytes:           len(buf),
			Security: Security{
				HSTS:                resp.Header.Get("Strict-Transport-Security") != "",
				CSP:                 resp.Header.Get("Content-Security-Policy") != "",
				XFrameOptions:       resp.Header.Get("X-Frame-Options") != "",
				XContentTypeOptions: resp.Header.Get("X-Content-Type-Options") != "",
				ReferrerPolicy:      resp.Header.Get("Referrer-Policy") != "",
			},
		},
		Issues: make([]Issue, 0, 24),
	}

	// Parse the HTML. charset.NewReader transcodes non-UTF-8 bodies (declared
	// via Content-Type or a <meta charset>) so extracted text isn't mojibake.
	// html.Parse never errors on arbitrary bytes — a non-HTML URL simply yields
	// empty SEO fields, which is acceptable.
	if r, cerr := charset.NewReader(bytes.NewReader(buf), contentType); cerr == nil {
		if doc, perr := html.Parse(r); perr == nil {
			st := &parseState{seo: &report.SEO, jsonldSeen: make(map[string]bool)}
			walk(doc, st)
		}
	}

	// Best-effort crawlability probes against the final URL's origin. Failures
	// are swallowed — they only downgrade the relevant issue, never the audit.
	report.RobotsTxt, report.Sitemap = probeCrawlability(ctx, finalURL)

	buildIssues(report)
	return report, nil
}

// schemeOf returns the (lowercased) scheme of a URL string, or "" if unparseable.
func schemeOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Scheme
	}
	return ""
}

// parseState carries the mutable accumulators through the HTML walk.
type parseState struct {
	seo        *SEO
	titleSet   bool
	jsonldSeen map[string]bool
}

// walk recurses the node tree, extracting SEO signals. It only matches elements
// in the null namespace so foreign content (SVG <title>/<image>, MathML) never
// pollutes the page title or image counts.
func walk(n *html.Node, st *parseState) {
	if n.Type == html.ElementNode && n.Namespace == "" {
		switch n.Data {
		case "html":
			if v, ok := attr(n, "lang"); ok && st.seo.Lang == "" {
				st.seo.Lang = strings.TrimSpace(v)
			}
		case "title":
			if !st.titleSet {
				v := textContent(n)
				st.seo.Title = TextField{Value: v, Length: utf8.RuneCountInString(v)}
				st.titleSet = true
			}
		case "meta":
			handleMeta(n, st.seo)
		case "link":
			handleLink(n, st.seo)
		case "h1", "h2", "h3", "h4", "h5", "h6":
			level := int(n.Data[1] - '0')
			text := textContent(n)
			if n.Data == "h1" {
				st.seo.H1 = append(st.seo.H1, text)
				st.seo.H1Count++
			}
			if len(st.seo.Headings) < maxHeadings {
				st.seo.Headings = append(st.seo.Headings, Heading{Level: level, Text: truncateRunes(text, maxHeadingText)})
			}
		case "img":
			st.seo.Images.Total++
			if alt, ok := attr(n, "alt"); !ok || strings.TrimSpace(alt) == "" {
				st.seo.Images.MissingAlt++
			}
		case "script":
			if t, _ := attr(n, "type"); strings.EqualFold(strings.TrimSpace(t), "application/ld+json") {
				collectJSONLD(rawText(n), st.seo, st.jsonldSeen)
			}
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, st)
	}
}

// handleMeta routes a <meta> element to the right SEO field. name= carries
// description/robots/viewport/twitter; property= carries og. OG and Twitter are
// checked under both attributes because real pages disagree on which to use.
func handleMeta(n *html.Node, seo *SEO) {
	content, _ := attr(n, "content")
	name, _ := attr(n, "name")
	prop, _ := attr(n, "property")
	ln := strings.ToLower(strings.TrimSpace(name))
	lp := strings.ToLower(strings.TrimSpace(prop))

	switch ln {
	case "description":
		if seo.MetaDescription.Value == "" {
			seo.MetaDescription = TextField{Value: strings.TrimSpace(content), Length: utf8.RuneCountInString(strings.TrimSpace(content))}
		}
	case "robots":
		if seo.RobotsMeta == "" {
			seo.RobotsMeta = strings.TrimSpace(content)
		}
	case "viewport":
		if seo.Viewport == "" {
			seo.Viewport = strings.TrimSpace(content)
		}
	}

	for _, id := range []string{ln, lp} {
		switch {
		case strings.HasPrefix(id, "og:"):
			putOnce(seo.OpenGraph, id, content)
		case strings.HasPrefix(id, "twitter:"):
			putOnce(seo.Twitter, id, content)
		}
	}
}

// handleLink routes a <link> element. rel is token-split and matched
// case-insensitively (rel="alternate stylesheet" and friends exist in the wild).
func handleLink(n *html.Node, seo *SEO) {
	rel, ok := attr(n, "rel")
	if !ok {
		return
	}
	href, _ := attr(n, "href")
	rels := strings.Fields(strings.ToLower(rel))
	has := func(t string) bool {
		for _, r := range rels {
			if r == t {
				return true
			}
		}
		return false
	}
	if has("canonical") && seo.Canonical == "" {
		seo.Canonical = strings.TrimSpace(href)
	}
	if has("alternate") {
		if hl, ok := attr(n, "hreflang"); ok {
			seo.Hreflang = append(seo.Hreflang, Hreflang{Lang: strings.TrimSpace(hl), Href: strings.TrimSpace(href)})
		}
	}
}

// collectJSONLD parses one JSON-LD blob and records every distinct @type it
// finds (recursing through nested objects, arrays, and @graph). Malformed JSON
// is ignored.
func collectJSONLD(raw string, seo *SEO, seen map[string]bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	var v interface{}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return
	}
	var rec func(interface{})
	rec = func(x interface{}) {
		switch t := x.(type) {
		case map[string]interface{}:
			if tv, ok := t["@type"]; ok {
				for _, s := range typeStrings(tv) {
					if s != "" && !seen[s] {
						seen[s] = true
						seo.StructuredData = append(seo.StructuredData, StructuredData{Type: s})
					}
				}
			}
			for _, val := range t {
				rec(val)
			}
		case []interface{}:
			for _, val := range t {
				rec(val)
			}
		}
	}
	rec(v)
}

// typeStrings normalizes an @type value (string or array of strings) to a slice.
func typeStrings(v interface{}) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// attr returns the value of the named attribute (keys are already lowercased by
// the parser) and whether it was present.
func attr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

// textContent returns the whitespace-collapsed visible text of a node, skipping
// script/style subtrees.
func textContent(n *html.Node) string {
	var sb strings.Builder
	var rec func(*html.Node)
	rec = func(c *html.Node) {
		if c.Type == html.TextNode {
			sb.WriteString(c.Data)
			return
		}
		if c.Type == html.ElementNode && (c.Data == "script" || c.Data == "style") {
			return
		}
		for ch := c.FirstChild; ch != nil; ch = ch.NextSibling {
			rec(ch)
		}
	}
	rec(n)
	return strings.Join(strings.Fields(sb.String()), " ")
}

// rawText returns the concatenated direct text children of a node without
// collapsing whitespace — used for <script type=application/ld+json> bodies.
func rawText(n *html.Node) string {
	var sb strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode {
			sb.WriteString(c.Data)
		}
	}
	return sb.String()
}

// putOnce sets m[key]=val only if key is absent, preserving the first-seen tag.
func putOnce(m map[string]string, key, val string) {
	if key == "" {
		return
	}
	if _, ok := m[key]; !ok {
		m[key] = val
	}
}

// truncateRunes caps s to n runes, appending an ellipsis when it trims.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// probeCrawlability best-effort fetches origin/robots.txt and origin/sitemap.xml.
// Any error leaves the corresponding struct at its zero (not-present) value.
func probeCrawlability(ctx context.Context, finalURL string) (RobotsTxt, Sitemap) {
	robots := RobotsTxt{}
	sitemap := Sitemap{}

	u, err := url.Parse(finalURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return robots, sitemap
	}
	origin := u.Scheme + "://" + u.Host
	robots.URL = origin + "/robots.txt"
	sitemap.URL = origin + "/sitemap.xml"

	// Dedicated short-timeout client, nil transport → pooled DefaultTransport.
	client := &http.Client{Timeout: probeTimeout}

	if body, ok := probeGET(ctx, client, robots.URL, maxRobotsBytes); ok {
		robots.Present = true
		robots.HasSitemap = robotsDeclaresSitemap(body)
	}
	// A tiny GET (not HEAD — 405-on-HEAD is common); we only need the status.
	if _, ok := probeGET(ctx, client, sitemap.URL, 1024); ok {
		sitemap.Present = true
	}

	return robots, sitemap
}

// probeGET performs a capped GET and reports whether the status was 2xx along
// with up to limit bytes of the body.
func probeGET(ctx context.Context, client *http.Client, target string, limit int64) ([]byte, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, limit))
	return body, resp.StatusCode >= 200 && resp.StatusCode < 300
}

// robotsDeclaresSitemap reports whether any line declares a Sitemap: directive
// (case-insensitive, leading whitespace tolerated).
func robotsDeclaresSitemap(body []byte) bool {
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "sitemap:") {
			return true
		}
	}
	return false
}

// buildIssues runs every check against the populated report and appends the
// rolled-up checklist. Order groups roughly by category for a readable list.
func buildIssues(r *Report) {
	// why is shared across a check's ok/warn/error branches — it explains the
	// check, not the outcome — so each block sets one `why` and reuses it.
	add := func(sev, cat, msg, why string) {
		r.Issues = append(r.Issues, Issue{Severity: sev, Category: cat, Message: msg, Why: why})
	}

	// --- Crawlability: status + redirects ---
	whyStatus := "A 2xx status confirms the page is reachable; any other code stops users and search-engine crawlers from seeing the content."
	if r.OK {
		add(sevOK, catCrawl, fmt.Sprintf("Fetched successfully (HTTP %d)", r.StatusCode), whyStatus)
	} else {
		add(sevError, catCrawl, fmt.Sprintf("Non-2xx response (HTTP %d)", r.StatusCode), whyStatus)
	}
	whyRedirects := "Each redirect hop adds latency and bleeds off link equity, so a short chain loads faster and crawls cleaner."
	if n := len(r.Redirects); n > 3 {
		add(sevWarning, catCrawl, fmt.Sprintf("Long redirect chain (%d hops)", n), whyRedirects)
	} else if n > 0 {
		add(sevOK, catCrawl, fmt.Sprintf("Redirect chain OK (%d hop%s)", n, plural(n)), whyRedirects)
	}

	// --- SEO: title ---
	whyTitle := "Search engines show the title as the clickable result headline; a good one improves rankings and click-through."
	switch tl := r.SEO.Title.Length; {
	case r.SEO.Title.Value == "":
		add(sevWarning, catSEO, "Missing <title> tag", whyTitle)
	case tl < 10 || tl > 60:
		add(sevWarning, catSEO, fmt.Sprintf("Title length %d is outside the recommended 10–60 characters", tl), whyTitle)
	default:
		add(sevOK, catSEO, fmt.Sprintf("Title present (%d characters)", tl), whyTitle)
	}

	// --- SEO: meta description ---
	whyMetaDesc := "Used as the search-result snippet — a compelling one raises click-through even though it isn't a direct ranking factor."
	switch dl := r.SEO.MetaDescription.Length; {
	case r.SEO.MetaDescription.Value == "":
		add(sevWarning, catSEO, "Missing meta description", whyMetaDesc)
	case dl < 50 || dl > 160:
		add(sevWarning, catSEO, fmt.Sprintf("Meta description length %d is outside the recommended 50–160 characters", dl), whyMetaDesc)
	default:
		add(sevOK, catSEO, fmt.Sprintf("Meta description present (%d characters)", dl), whyMetaDesc)
	}

	// --- SEO: single H1 ---
	whyH1 := "A single clear H1 tells search engines and assistive tech what the page is primarily about."
	switch r.SEO.H1Count {
	case 0:
		add(sevWarning, catSEO, "No H1 heading found", whyH1)
	case 1:
		add(sevOK, catSEO, "Exactly one H1 heading", whyH1)
	default:
		add(sevWarning, catSEO, fmt.Sprintf("Multiple H1 headings (%d)", r.SEO.H1Count), whyH1)
	}

	// --- SEO: canonical / lang / viewport ---
	whyCanonical := "Tells search engines the preferred URL, preventing duplicate-content dilution across variants."
	if r.SEO.Canonical != "" {
		add(sevOK, catSEO, "Canonical link present", whyCanonical)
	} else {
		add(sevWarning, catSEO, "No canonical link", whyCanonical)
	}
	whyLang := "Declares the page's language so search engines target it correctly and screen readers pronounce it right."
	if r.SEO.Lang != "" {
		add(sevOK, catSEO, fmt.Sprintf("Document language set (%s)", r.SEO.Lang), whyLang)
	} else {
		add(sevWarning, catSEO, "No <html lang> attribute", whyLang)
	}
	whyViewport := "Enables responsive mobile rendering and is required for Google's mobile-first indexing."
	if r.SEO.Viewport != "" {
		add(sevOK, catSEO, "Viewport meta present", whyViewport)
	} else {
		add(sevWarning, catSEO, "No viewport meta (mobile rendering)", whyViewport)
	}

	// --- SEO: image alt coverage ---
	whyAlt := "Alt text makes images accessible to screen readers and gives search engines context for image search."
	switch {
	case r.SEO.Images.Total == 0:
		add(sevInfo, catSEO, "No <img> elements found", whyAlt)
	case r.SEO.Images.MissingAlt == 0:
		add(sevOK, catSEO, fmt.Sprintf("All %d image(s) have alt text", r.SEO.Images.Total), whyAlt)
	default:
		add(sevWarning, catSEO, fmt.Sprintf("%d of %d images missing alt text", r.SEO.Images.MissingAlt, r.SEO.Images.Total), whyAlt)
	}

	// --- Social: Open Graph core + Twitter card ---
	whyOG := "Controls the title, description, and image shown when the page is shared on social platforms."
	var missingOG []string
	for _, k := range []string{"og:title", "og:description", "og:image"} {
		if _, ok := r.SEO.OpenGraph[k]; !ok {
			missingOG = append(missingOG, k)
		}
	}
	switch {
	case len(r.SEO.OpenGraph) == 0:
		add(sevWarning, catSocial, "No Open Graph tags", whyOG)
	case len(missingOG) > 0:
		add(sevWarning, catSocial, "Missing core Open Graph tags: "+strings.Join(missingOG, ", "), whyOG)
	default:
		add(sevOK, catSocial, "Core Open Graph tags present (title, description, image)", whyOG)
	}
	whyTwitter := "Defines how the page renders as a rich card when shared on Twitter/X."
	if _, ok := r.SEO.Twitter["twitter:card"]; ok {
		add(sevOK, catSocial, "Twitter Card present", whyTwitter)
	} else {
		add(sevWarning, catSocial, "No Twitter Card (twitter:card)", whyTwitter)
	}

	// --- Structured data ---
	whyStructured := "Helps search engines understand the page and can unlock rich results like stars, prices, and FAQs."
	if len(r.SEO.StructuredData) > 0 {
		types := make([]string, 0, len(r.SEO.StructuredData))
		for _, sd := range r.SEO.StructuredData {
			types = append(types, sd.Type)
		}
		add(sevOK, catStructured, "JSON-LD structured data present: "+strings.Join(types, ", "), whyStructured)
	} else {
		add(sevInfo, catStructured, "No JSON-LD structured data", whyStructured)
	}

	// --- Security: HTTPS + hardening headers ---
	whyHTTPS := "Encrypts traffic, is trusted by browsers, and is a confirmed Google ranking signal."
	if r.Quality.HTTPS {
		add(sevOK, catSecurity, "Served over HTTPS", whyHTTPS)
	} else {
		add(sevError, catSecurity, "Not served over HTTPS", whyHTTPS)
	}
	secHeader := func(present bool, name, why string) {
		if present {
			add(sevOK, catSecurity, name+" header present", why)
		} else {
			add(sevWarning, catSecurity, "Missing "+name+" header", why)
		}
	}
	secHeader(r.Quality.Security.HSTS, "Strict-Transport-Security",
		"Forces HTTPS on future visits, blocking SSL-strip downgrade attacks.")
	secHeader(r.Quality.Security.CSP, "Content-Security-Policy",
		"Restricts which scripts and resources can load, mitigating cross-site scripting (XSS) and injection.")
	secHeader(r.Quality.Security.XFrameOptions, "X-Frame-Options",
		"Stops other sites from framing the page, defending against clickjacking.")
	secHeader(r.Quality.Security.XContentTypeOptions, "X-Content-Type-Options",
		"nosniff stops browsers from MIME-sniffing responses into executable content.")
	secHeader(r.Quality.Security.ReferrerPolicy, "Referrer-Policy",
		"Controls how much of the referring URL leaks to other sites, protecting user privacy.")

	// --- Performance: compression + caching ---
	whyCompression := "Compressing responses (gzip/brotli) shrinks transfer size and speeds up page loads."
	if r.Quality.ContentEncoding != "" {
		add(sevOK, catPerformance, fmt.Sprintf("Response compressed (%s)", r.Quality.ContentEncoding), whyCompression)
	} else {
		add(sevWarning, catPerformance, "Response not compressed", whyCompression)
	}
	whyCaching := "Cache-Control lets browsers and CDNs reuse assets, cutting repeat load times and server traffic."
	if r.Quality.CacheControl != "" {
		add(sevOK, catPerformance, "Cache-Control header present", whyCaching)
	} else {
		add(sevInfo, catPerformance, "No Cache-Control header", whyCaching)
	}

	// --- Crawlability: robots.txt + sitemap ---
	whyRobots := "Tells crawlers what they may fetch and can point them to your sitemap for efficient crawling."
	switch {
	case !r.RobotsTxt.Present:
		add(sevWarning, catCrawl, "No robots.txt found", whyRobots)
	case r.RobotsTxt.HasSitemap:
		add(sevOK, catCrawl, "robots.txt present and declares a Sitemap", whyRobots)
	default:
		add(sevInfo, catCrawl, "robots.txt present but declares no Sitemap", whyRobots)
	}
	whySitemap := "Lists your URLs so search engines discover and crawl pages faster and more completely."
	if r.Sitemap.Present {
		add(sevOK, catCrawl, "sitemap.xml found", whySitemap)
	} else {
		add(sevWarning, catCrawl, "No sitemap.xml found", whySitemap)
	}
}

// plural returns "s" for counts != 1.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
