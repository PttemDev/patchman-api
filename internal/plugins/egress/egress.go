// Package egress is the plugin network fence (PLUGIN_MARKETPLACE_RFC §6.2): a plugin may only
// reach host:port pairs its manifest declared, every resolved IP is
// classified, and the dial goes to the checked IP (no second resolution, so
// no DNS rebind). Loopback, link-local, metadata, NAT64, CGNAT, multicast and
// the machine's own addresses are always refused; RFC1918/ULA only when the
// rule says private:true.
package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// Rule is one manifest `permissions.net` entry.
type Rule struct {
	Host    string // exact host or "*.example.com"
	Ports   []int
	Private bool
}

type Policy struct {
	Rules []Rule
	// RestrictPrivate (hosted/server mode): private:true reaches only the
	// admin CIDRs in PrivateAllow; an empty list then means no private range.
	// Off (desktop): private:true reaches any private range.
	RestrictPrivate bool
	PrivateAllow    []netip.Prefix
}

var ErrDenied = errors.New("egress denied")

type Class int

const (
	Public Class = iota
	Private
	Forbidden
)

var forbidden = mustPrefixes(
	"0.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "100.64.0.0/10", "192.0.0.0/24",
	"224.0.0.0/4", "240.0.0.0/4", "255.255.255.255/32",
	"::/128", "::1/128", "fe80::/10", "ff00::/8",
	"64:ff9b::/96", "64:ff9b:1::/48", // NAT64
	"2002::/16",         // 6to4 (embeds IPv4)
	"2001::/32",         // Teredo
	"fd00:ec2::254/128", // AWS IPv6 metadata
)

var private = mustPrefixes("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "198.18.0.0/15", "fc00::/7")

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

var (
	selfOnce sync.Once
	selfIPs  map[netip.Addr]bool
)

func ownAddrs() map[netip.Addr]bool {
	selfOnce.Do(func() {
		selfIPs = map[netip.Addr]bool{}
		addrs, _ := net.InterfaceAddrs()
		for _, a := range addrs {
			if p, err := netip.ParsePrefix(a.String()); err == nil {
				selfIPs[p.Addr().Unmap()] = true
			}
		}
	})
	return selfIPs
}

// Classify buckets an address. IPv4-mapped IPv6 is unmapped first.
func Classify(a netip.Addr) Class {
	a = a.Unmap()
	if !a.IsValid() || a.Zone() != "" {
		return Forbidden
	}
	for _, p := range forbidden {
		if p.Contains(a) {
			return Forbidden
		}
	}
	if ownAddrs()[a] {
		return Forbidden
	}
	for _, p := range private {
		if p.Contains(a) {
			return Private
		}
	}
	return Public
}

// Resolver is swappable so tests can simulate DNS rebinding.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Guard decides and performs plugin dials.
type Guard struct {
	Policy   Policy
	Resolver Resolver
	dial     DialFunc
}

// DialFunc connects to an address the guard has already checked ("ip:port").
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

func NewGuard(p Policy) *Guard {
	g := &Guard{Policy: p, Resolver: net.DefaultResolver}
	d := &net.Dialer{Control: g.control}
	g.dial = d.DialContext
	return g
}

// NewGuardWith swaps the resolver and the final dialer. Tests use it to point
// a checked public test IP at an httptest server; every policy check still
// runs before dial is called, and dial only ever sees a checked IP.
func NewGuardWith(p Policy, r Resolver, dial DialFunc) *Guard {
	g := NewGuard(p)
	if r != nil {
		g.Resolver = r
	}
	if dial != nil {
		g.dial = dial
	}
	return g
}

func (g *Guard) rule(host string, port int) (Rule, bool) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, r := range g.Policy.Rules {
		h := strings.ToLower(r.Host)
		match := h == host || (strings.HasPrefix(h, "*.") && strings.HasSuffix(host, h[1:]) && len(host) > len(h)-1)
		if !match {
			continue
		}
		for _, p := range r.Ports {
			if p == port {
				return r, true
			}
		}
	}
	return Rule{}, false
}

// Allowed reports whether host:port is declared (used for redirects before dialing).
func (g *Guard) Allowed(host string, port int) bool { _, ok := g.rule(host, port); return ok }

func (g *Guard) checkIP(a netip.Addr, r Rule) error {
	switch Classify(a) {
	case Forbidden:
		return fmt.Errorf("%w: %s is a forbidden address", ErrDenied, a)
	case Private:
		if !r.Private {
			return fmt.Errorf("%w: %s is private and the rule is not private:true", ErrDenied, a)
		}
		if g.Policy.RestrictPrivate {
			for _, p := range g.Policy.PrivateAllow {
				if p.Contains(a.Unmap()) {
					return nil
				}
			}
			return fmt.Errorf("%w: %s is outside the admin private allow-list", ErrDenied, a)
		}
	}
	return nil
}

// DialContext resolves once, checks every candidate, and dials the checked IP.
func (g *Guard) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("%w: network %q", ErrDenied, network)
	}
	host, ps, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(ps)
	if err != nil {
		return nil, err
	}
	r, ok := g.rule(host, port)
	if !ok {
		return nil, fmt.Errorf("%w: %s not declared in permissions.net", ErrDenied, addr)
	}
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip}
	} else {
		ips, err = g.Resolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
	}
	var lastErr error = fmt.Errorf("%w: no address for %s", ErrDenied, host)
	for _, ip := range ips {
		if err := g.checkIP(ip, r); err != nil {
			lastErr = err
			continue // any forbidden candidate is skipped, never dialed
		}
		c, err := g.dial(ctx, network, netip.AddrPortFrom(ip.Unmap(), uint16(port)).String())
		if err == nil {
			return c, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// control re-checks the final sockaddr (defence in depth: catches any path
// that reaches the dialer without going through DialContext's checks).
func (g *Guard) control(network, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: unparsable sockaddr %q", ErrDenied, address)
	}
	c := Classify(ap.Addr())
	if c == Forbidden {
		return fmt.Errorf("%w: sockaddr %s forbidden", ErrDenied, ap)
	}
	return nil
}
