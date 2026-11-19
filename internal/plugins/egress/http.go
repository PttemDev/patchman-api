package egress

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

type HTTPLimits struct {
	MaxRedirects  int
	MaxHeaderSize int64
	Timeout       time.Duration
}

// HTTPClient builds a client bound to this guard: no proxy (a proxy would make
// the dial check see the proxy, not the target), every redirect re-checked,
// credentials dropped when the host changes. One client per plugin connection.
func (g *Guard) HTTPClient(l HTTPLimits) *http.Client {
	if l.MaxRedirects == 0 {
		l.MaxRedirects = 5
	}
	if l.MaxHeaderSize == 0 {
		l.MaxHeaderSize = 64 << 10
	}
	tr := &http.Transport{
		Proxy:                  nil,
		DialContext:            g.DialContext,
		ForceAttemptHTTP2:      true,
		MaxResponseHeaderBytes: l.MaxHeaderSize,
		MaxIdleConnsPerHost:    4,
		IdleConnTimeout:        90 * time.Second,
		TLSHandshakeTimeout:    10 * time.Second,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   l.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= l.MaxRedirects {
				return errors.New("too many redirects")
			}
			port := 443
			if req.URL.Scheme == "http" {
				port = 80
			}
			if p := req.URL.Port(); p != "" {
				port, _ = strconv.Atoi(p)
			}
			if !g.Allowed(req.URL.Hostname(), port) {
				return fmt.Errorf("%w: redirect to %s", ErrDenied, req.URL.Host)
			}
			if req.URL.Host != via[0].URL.Host {
				for _, h := range []string{"Authorization", "Cookie", "Proxy-Authorization", "X-Api-Key"} {
					req.Header.Del(h)
				}
			}
			return nil
		},
	}
}
