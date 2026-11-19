package egress

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := map[string]Class{
		"8.8.8.8": Public, "1.1.1.1": Public, "2606:4700::1111": Public,
		"127.0.0.1": Forbidden, "127.9.9.9": Forbidden, "::1": Forbidden, "0.0.0.0": Forbidden, "::": Forbidden,
		"169.254.169.254": Forbidden, "fe80::1": Forbidden, "100.100.100.200": Forbidden, "100.64.0.1": Forbidden,
		"::ffff:127.0.0.1": Forbidden, "::ffff:169.254.169.254": Forbidden, "64:ff9b::7f00:1": Forbidden,
		"2002:7f00:1::": Forbidden, "2001:0::1": Forbidden, "224.0.0.1": Forbidden, "ff02::1": Forbidden,
		"255.255.255.255": Forbidden, "240.0.0.1": Forbidden, "fd00:ec2::254": Forbidden,
		"10.1.2.3": Private, "172.16.0.1": Private, "192.168.1.1": Private, "fd12::1": Private, "::ffff:10.0.0.1": Private,
	}
	for s, want := range cases {
		if got := Classify(netip.MustParseAddr(s)); got != want {
			t.Errorf("%s: got %d want %d", s, got, want)
		}
	}
}

type fakeResolver map[string][]string

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	var out []netip.Addr
	for _, s := range f[host] {
		out = append(out, netip.MustParseAddr(s))
	}
	if len(out) == 0 {
		return nil, errors.New("nxdomain")
	}
	return out, nil
}

// guard whose dial only records the target instead of connecting.
func recGuard(p Policy, r Resolver) (*Guard, *[]string) {
	g := NewGuard(p)
	g.Resolver = r
	var dialed []string
	g.dial = func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		c1, c2 := net.Pipe()
		c2.Close()
		return c1, nil
	}
	return g, &dialed
}

func TestDialDecisions(t *testing.T) {
	res := fakeResolver{
		"api.example.com":  {"93.184.216.34"},
		"rebind.example":   {"127.0.0.1"},
		"mixed.example":    {"169.254.169.254", "93.184.216.34"},
		"internal.corp":    {"10.0.0.5"},
		"wild.example.com": {"93.184.216.35"},
		"mapped.example":   {"::ffff:127.0.0.1"},
	}
	pol := Policy{Rules: []Rule{
		{Host: "api.example.com", Ports: []int{443}},
		{Host: "rebind.example", Ports: []int{443}},
		{Host: "mixed.example", Ports: []int{443}},
		{Host: "internal.corp", Ports: []int{9200}},
		{Host: "*.example.com", Ports: []int{8443}},
		{Host: "mapped.example", Ports: []int{80}},
		{Host: "127.0.0.1", Ports: []int{6379}},
	}}
	cases := []struct {
		addr, dialed string
		ok           bool
	}{
		{"api.example.com:443", "93.184.216.34:443", true},
		{"api.example.com:80", "", false},                // port not declared
		{"evil.com:443", "", false},                      // host not declared
		{"rebind.example:443", "", false},                // DNS answer is loopback
		{"mixed.example:443", "93.184.216.34:443", true}, // forbidden candidate skipped, never dialed
		{"internal.corp:9200", "", false},                // private without private:true
		{"wild.example.com:8443", "93.184.216.35:8443", true},
		{"example.com:8443", "", false},  // wildcard needs a subdomain
		{"mapped.example:80", "", false}, // v4-mapped loopback
		{"127.0.0.1:6379", "", false},    // literal loopback even if declared
	}
	for _, c := range cases {
		g, dialed := recGuard(pol, res)
		conn, err := g.DialContext(context.Background(), "tcp", c.addr)
		if conn != nil {
			conn.Close()
		}
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v want ok=%v", c.addr, err, c.ok)
			continue
		}
		if c.ok && (len(*dialed) != 1 || (*dialed)[0] != c.dialed) {
			t.Errorf("%s: dialed %v want %s", c.addr, *dialed, c.dialed)
		}
		if !c.ok && len(*dialed) != 0 {
			t.Errorf("%s: denied but dialed %v", c.addr, *dialed)
		}
	}
	if _, err := NewGuard(pol).DialContext(context.Background(), "udp", "api.example.com:443"); err == nil {
		t.Error("udp must be refused")
	}
}

func TestPrivateRules(t *testing.T) {
	res := fakeResolver{"es.corp": {"10.0.0.5"}}
	rule := Rule{Host: "es.corp", Ports: []int{9200}, Private: true}
	g, _ := recGuard(Policy{Rules: []Rule{rule}}, res)
	if _, err := g.DialContext(context.Background(), "tcp", "es.corp:9200"); err != nil {
		t.Fatalf("desktop private:true should pass: %v", err)
	}
	g, _ = recGuard(Policy{Rules: []Rule{rule}, RestrictPrivate: true, PrivateAllow: []netip.Prefix{netip.MustParsePrefix("192.168.0.0/16")}}, res)
	if _, err := g.DialContext(context.Background(), "tcp", "es.corp:9200"); !errors.Is(err, ErrDenied) {
		t.Fatalf("hosted: outside admin CIDR must be denied, got %v", err)
	}
	// Hosted with no admin list: private:true reaches nothing private.
	g, _ = recGuard(Policy{Rules: []Rule{rule}, RestrictPrivate: true}, res)
	if _, err := g.DialContext(context.Background(), "tcp", "es.corp:9200"); !errors.Is(err, ErrDenied) {
		t.Fatalf("hosted: empty admin list must deny private, got %v", err)
	}
	g, _ = recGuard(Policy{Rules: []Rule{rule}, RestrictPrivate: true, PrivateAllow: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")}}, res)
	if _, err := g.DialContext(context.Background(), "tcp", "es.corp:9200"); err != nil {
		t.Fatalf("hosted: inside admin CIDR should pass: %v", err)
	}
}

func TestControlRejectsForbiddenSockaddr(t *testing.T) {
	g := NewGuard(Policy{})
	for _, a := range []string{"127.0.0.1:80", "[::1]:80", "[::ffff:169.254.169.254]:80"} {
		if err := g.control("tcp", a, nil); !errors.Is(err, ErrDenied) {
			t.Errorf("%s: %v", a, err)
		}
	}
	if err := g.control("tcp", "93.184.216.34:443", nil); err != nil {
		t.Error(err)
	}
}

func hostport(s *httptest.Server) string {
	u, _ := url.Parse(s.URL)
	return u.Host
}

// mapDial sends checked test IPs to httptest listeners.
func mapDial(m map[string]string) DialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		target, ok := m[addr]
		if !ok {
			return nil, errors.New("unmapped test address " + addr)
		}
		var d net.Dialer
		return d.DialContext(ctx, network, target)
	}
}

func TestHTTPRedirectsAndProxy(t *testing.T) {
	var gotAuth string
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		io.WriteString(w, "B")
	}))
	defer b.Close()
	undeclared := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "LEAK") }))
	defer undeclared.Close()
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/to-b":
			http.Redirect(w, r, "http://b.test/", http.StatusFound)
		case "/to-undeclared":
			http.Redirect(w, r, "http://u.test/", http.StatusFound)
		case "/to-loopback":
			http.Redirect(w, r, "http://"+hostport(undeclared)+"/", http.StatusFound)
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		}
	}))
	defer a.Close()
	res := fakeResolver{"a.test": {"203.0.113.1"}, "b.test": {"203.0.113.2"}, "u.test": {"203.0.113.3"}}
	g := NewGuardWith(Policy{Rules: []Rule{{Host: "a.test", Ports: []int{80}}, {Host: "b.test", Ports: []int{80}}}}, res,
		mapDial(map[string]string{"203.0.113.1:80": hostport(a), "203.0.113.2:80": hostport(b), "203.0.113.3:80": hostport(undeclared)}))
	t.Setenv("HTTP_PROXY", "http://10.255.255.1:1") // must be ignored
	c := g.HTTPClient(HTTPLimits{})

	req, _ := http.NewRequest("GET", "http://a.test/to-b", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "B" {
		t.Fatalf("redirect target body = %q", body)
	}
	if gotAuth != "" {
		t.Errorf("Authorization leaked across host change: %q", gotAuth)
	}
	if _, err := c.Get("http://a.test/to-undeclared"); !errors.Is(err, ErrDenied) {
		t.Errorf("redirect to undeclared host: %v", err)
	}
	if _, err := c.Get("http://a.test/to-loopback"); !errors.Is(err, ErrDenied) {
		t.Errorf("redirect to loopback: %v", err)
	}
	if _, err := c.Get("http://a.test/loop"); err == nil {
		t.Error("redirect loop must stop")
	}
}
