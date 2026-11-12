package handlers

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/miekg/dns"
)

// DNSHandler backs the Dev Tools "DNS Toolkit" utility. It is a stateless
// resolver front-end — no DB, no connection registry, no grants — mirroring the
// codegen/corsproxy utility handlers. It speaks real DNS via github.com/miekg/dns
// (rather than the Go stdlib net.Resolver), so it returns TTLs and richer record
// types (SOA, CAA) the stdlib can't reach. Each requested record type runs
// concurrently and tolerates its own failure, so one bad type never sinks the
// whole request.
type DNSHandler struct{}

func NewDNSHandler() *DNSHandler {
	return &DNSHandler{}
}

// dnsLookupRequest is the wire body. server is an optional custom resolver as
// "ip" or "ip:port" (default port 53); empty uses the system resolver.
type dnsLookupRequest struct {
	Name   string   `json:"name"`
	Types  []string `json:"types"`
	Server string   `json:"server"`
}

// dnsRecord is one resource record. Value is the presentation string (the thing
// to copy/read); Ttl is the record's remaining time-to-live in seconds. The
// optional structured fields carry the parsed pieces for richer rendering and
// are set only for the record types that have them. Pointer types are used for
// the numeric structured fields so a legitimate zero (e.g. CAA flag 0, SRV
// priority 0) still marshals instead of being dropped by omitempty.
//
// CAA is the one asymmetry: it has no dedicated content field in the contract,
// so Value holds the CAA value (e.g. "letsencrypt.org") with Flag/Tag structured;
// the FE composes `flag tag "value"`. Every other type puts its full presentation
// string in Value.
type dnsRecord struct {
	Value string `json:"value"`
	TTL   uint32 `json:"ttl"`

	// MX
	Preference *uint16 `json:"preference,omitempty"`
	// SRV
	Priority *uint16 `json:"priority,omitempty"`
	Weight   *uint16 `json:"weight,omitempty"`
	Port     *uint16 `json:"port,omitempty"`
	// SRV + MX target host
	Target string `json:"target,omitempty"`
	// CAA
	Flag *uint8 `json:"flag,omitempty"`
	Tag  string `json:"tag,omitempty"`
	// SOA
	Mname   string  `json:"mname,omitempty"`
	Rname   string  `json:"rname,omitempty"`
	Serial  *uint32 `json:"serial,omitempty"`
	Refresh *uint32 `json:"refresh,omitempty"`
	Retry   *uint32 `json:"retry,omitempty"`
	Expire  *uint32 `json:"expire,omitempty"`
	Minttl  *uint32 `json:"minttl,omitempty"`
}

// dnsTypeResult is one record type's outcome. Records is always non-nil so it
// marshals as [] rather than null. Error carries this type's own failure
// without failing the sibling types.
type dnsTypeResult struct {
	Type       string      `json:"type"`
	Records    []dnsRecord `json:"records"`
	Error      string      `json:"error,omitempty"`
	DurationMs int64       `json:"durationMs"`
}

type dnsLookupResponse struct {
	Name     string          `json:"name"`
	Resolver string          `json:"resolver"`
	Results  []dnsTypeResult `json:"results"`
}

// dnsPerTypeTimeout caps a single record-type lookup (UDP query plus any TCP
// retry share this budget) so a black-holed resolver can't hang the request.
const dnsPerTypeTimeout = 4 * time.Second

// dnsQTypes maps our public type names to miekg/dns query types. PTR is handled
// separately (its query name is derived from the IP), so it is not listed here.
var dnsQTypes = map[string]uint16{
	"A":     dns.TypeA,
	"AAAA":  dns.TypeAAAA,
	"MX":    dns.TypeMX,
	"TXT":   dns.TypeTXT,
	"NS":    dns.TypeNS,
	"CNAME": dns.TypeCNAME,
	"SRV":   dns.TypeSRV,
	"SOA":   dns.TypeSOA,
	"CAA":   dns.TypeCAA,
	"PTR":   dns.TypePTR,
}

// Lookup resolves the requested record types for a name concurrently.
// POST /api/devtools/dns  { name, types, server? } →
//
//	{ data: { name, resolver, results: [{ type, records, error?, durationMs }] } }
//
// records is now a list of objects ({ value, ttl, ...structured }) rather than
// bare strings.
func (h *DNSHandler) Lookup(c *gin.Context) {
	var req dnsLookupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	// Normalize + dedupe requested types; default to A + AAAA (matches the FE
	// default) when none are supplied.
	types := normalizeDNSTypes(req.Types)
	if len(types) == 0 {
		types = []string{"A", "AAAA"}
	}

	server := resolveDNSServer(strings.TrimSpace(req.Server))

	// Each type writes its own index — no shared mutation, so no mutex needed.
	results := make([]dnsTypeResult, len(types))
	var wg sync.WaitGroup
	for i, t := range types {
		wg.Add(1)
		go func(i int, t string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(c.Request.Context(), dnsPerTypeTimeout)
			defer cancel()
			results[i] = resolveOne(ctx, server, t, name)
		}(i, t)
	}
	wg.Wait()

	c.JSON(http.StatusOK, gin.H{"data": dnsLookupResponse{
		Name:     name,
		Resolver: server,
		Results:  results,
	}})
}

// normalizeDNSTypes upper-cases, trims and de-duplicates the requested types,
// preserving first-seen order.
func normalizeDNSTypes(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, t := range in {
		u := strings.ToUpper(strings.TrimSpace(t))
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	return out
}

// resolveDNSServer decides which resolver address ("ip:port") to query. When the
// caller supplies a server it wins (bare host gets the default DNS port 53).
// Otherwise the system resolver is read from /etc/resolv.conf; if that is
// missing/empty (e.g. a stripped container) it falls back to Cloudflare's
// 1.1.1.1:53. The returned string is both the dial target and the resolver label
// echoed back to the client.
func resolveDNSServer(server string) string {
	if server != "" {
		if _, _, err := net.SplitHostPort(server); err == nil {
			return server
		}
		return net.JoinHostPort(server, "53")
	}
	if cfg, err := dns.ClientConfigFromFile("/etc/resolv.conf"); err == nil && len(cfg.Servers) > 0 {
		port := cfg.Port
		if port == "" {
			port = "53"
		}
		return net.JoinHostPort(cfg.Servers[0], port)
	}
	return "1.1.1.1:53"
}

// exchangeDNS sends a single query to server. It asks over UDP first (with an
// EDNS0 buffer of 1232 bytes so larger answers — e.g. multi-string TXT — don't
// truncate at the legacy 512-byte cap), then retries over TCP if the resolver
// still sets the TC (truncated) bit. A fresh client is used per call because the
// TCP retry mutates c.Net and callers run concurrently.
func exchangeDNS(ctx context.Context, server, qname string, qtype uint16) (*dns.Msg, error) {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(qname), qtype)
	m.RecursionDesired = true
	m.SetEdns0(1232, false) // larger UDP buffer; DO=false (no DNSSEC records requested)

	c := &dns.Client{Net: "udp", Timeout: dnsPerTypeTimeout}
	resp, _, err := c.ExchangeContext(ctx, m, server)
	if err != nil {
		return nil, err
	}
	if resp.Truncated {
		c.Net = "tcp"
		resp, _, err = c.ExchangeContext(ctx, m, server)
		if err != nil {
			return nil, err
		}
	}
	return resp, nil
}

// resolveOne performs a single record-type lookup against server. Every branch
// is self-contained and records its own error + duration; it never panics on an
// unknown type (returns an "unsupported" error instead). A NXDOMAIN response is
// surfaced as an error for that type (so a nonexistent name reads differently
// from an existing name with no records of that type); a genuine failure
// (timeout / SERVFAIL / REFUSED) also lands in that type's error.
func resolveOne(ctx context.Context, server, typ, name string) dnsTypeResult {
	start := time.Now()
	res := dnsTypeResult{Type: typ, Records: []dnsRecord{}}

	qtype, ok := dnsQTypes[typ]
	if !ok {
		res.Error = fmt.Sprintf("unsupported record type %q", typ)
		res.DurationMs = time.Since(start).Milliseconds()
		return res
	}

	// PTR queries a reversed IP name (in-addr.arpa / ip6.arpa), not the raw name.
	qname := name
	if typ == "PTR" {
		if net.ParseIP(name) == nil {
			res.Error = "PTR (reverse) lookup requires an IP address"
			res.DurationMs = time.Since(start).Milliseconds()
			return res
		}
		arpa, err := dns.ReverseAddr(name)
		if err != nil {
			res.Error = err.Error()
			res.DurationMs = time.Since(start).Milliseconds()
			return res
		}
		qname = arpa
	}

	resp, err := exchangeDNS(ctx, server, qname, qtype)
	if err != nil {
		res.Error = err.Error()
		res.DurationMs = time.Since(start).Milliseconds()
		return res
	}

	switch resp.Rcode {
	case dns.RcodeSuccess:
		// fall through to parse the answers (may still be empty → NODATA)
	case dns.RcodeNameError:
		res.Error = "NXDOMAIN (name does not exist)"
		res.DurationMs = time.Since(start).Milliseconds()
		return res
	default:
		res.Error = fmt.Sprintf("query failed: %s", dns.RcodeToString[resp.Rcode])
		res.DurationMs = time.Since(start).Milliseconds()
		return res
	}

	for _, rr := range resp.Answer {
		// The Answer section can include intermediate RRs (e.g. a CNAME chain in
		// front of an A answer). Keep only records of the type we asked for.
		if rr.Header().Rrtype != qtype {
			continue
		}
		res.Records = append(res.Records, mapDNSRecord(rr))
	}

	res.DurationMs = time.Since(start).Milliseconds()
	return res
}

// mapDNSRecord converts a miekg/dns resource record into our wire record,
// setting Value plus any type-specific structured fields. The TTL always comes
// from the record header.
func mapDNSRecord(rr dns.RR) dnsRecord {
	rec := dnsRecord{TTL: rr.Header().Ttl}
	switch v := rr.(type) {
	case *dns.A:
		rec.Value = v.A.String()
	case *dns.AAAA:
		rec.Value = v.AAAA.String()
	case *dns.MX:
		rec.Value = fmt.Sprintf("%d %s", v.Preference, v.Mx)
		pref := v.Preference
		rec.Preference = &pref
		rec.Target = v.Mx
	case *dns.TXT:
		rec.Value = strings.Join(v.Txt, "")
	case *dns.NS:
		rec.Value = v.Ns
	case *dns.CNAME:
		rec.Value = v.Target
	case *dns.SRV:
		rec.Value = fmt.Sprintf("%d %d %d %s", v.Priority, v.Weight, v.Port, v.Target)
		prio, wt, port := v.Priority, v.Weight, v.Port
		rec.Priority = &prio
		rec.Weight = &wt
		rec.Port = &port
		rec.Target = v.Target
	case *dns.PTR:
		rec.Value = v.Ptr
	case *dns.SOA:
		rec.Value = fmt.Sprintf("%s %s %d %d %d %d %d", v.Ns, v.Mbox, v.Serial, v.Refresh, v.Retry, v.Expire, v.Minttl)
		serial, refresh, retry, expire, minttl := v.Serial, v.Refresh, v.Retry, v.Expire, v.Minttl
		rec.Mname = v.Ns
		rec.Rname = v.Mbox
		rec.Serial = &serial
		rec.Refresh = &refresh
		rec.Retry = &retry
		rec.Expire = &expire
		rec.Minttl = &minttl
	case *dns.CAA:
		// Value holds the CAA content; the FE composes `flag tag "value"`.
		rec.Value = v.Value
		flag := v.Flag
		rec.Flag = &flag
		rec.Tag = v.Tag
	default:
		// Any other RR that slipped through the type filter: fall back to the
		// record's own presentation string, stripped of the leading header.
		rec.Value = strings.TrimSpace(strings.TrimPrefix(rr.String(), rr.Header().String()))
	}
	return rec
}
