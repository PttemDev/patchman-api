package graylogquery

import (
	"context"
	"encoding/base64"
	"os"
	"strings"
	"testing"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/services"
)

// TestLiveGraylog runs against a real Graylog when PATCHMAN_TEST_GRAYLOG_URL
// is set, with PATCHMAN_TEST_GRAYLOG_TOKEN (access token) and/or
// PATCHMAN_TEST_GRAYLOG_USER + _PASSWORD. The server needs some messages
// with a "service" field (the local rig sends order-api / basket-sync).
func TestLiveGraylog(t *testing.T) {
	base := os.Getenv("PATCHMAN_TEST_GRAYLOG_URL")
	if base == "" {
		t.Skip("PATCHMAN_TEST_GRAYLOG_URL not set")
	}
	cipher, err := services.NewTokenCipher(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))))
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(cipher)
	var conns []models.GraylogConnection
	if tok := os.Getenv("PATCHMAN_TEST_GRAYLOG_TOKEN"); tok != "" {
		enc, _ := cipher.Encrypt(tok)
		conns = append(conns, models.GraylogConnection{ID: "tok", URL: base, AuthKind: AuthToken, SecretEnc: enc, UpdatedAt: time.Now()})
	}
	if u := os.Getenv("PATCHMAN_TEST_GRAYLOG_USER"); u != "" {
		enc, _ := cipher.Encrypt(os.Getenv("PATCHMAN_TEST_GRAYLOG_PASSWORD"))
		conns = append(conns, models.GraylogConnection{ID: "pw", URL: base + "/api/", AuthKind: AuthBasic, Username: u, SecretEnc: enc, UpdatedAt: time.Now()})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, conn := range conns {
		t.Run(conn.AuthKind, func(t *testing.T) {
			tr, err := reg.Test(ctx, conn)
			if err != nil || tr.Version == "" || tr.Streams == 0 {
				t.Fatalf("test = %+v, %v", tr, err)
			}
			t.Logf("Graylog %s as %q, %d streams", tr.Version, tr.Username, tr.Streams)
			streams, err := reg.Streams(ctx, conn)
			if err != nil {
				t.Fatal(err)
			}
			var defStream string
			for _, s := range streams {
				if s.IsDefault {
					defStream = s.ID
				}
			}
			res, err := reg.Search(ctx, conn, SearchRequest{Query: "service:order-api", Range: TimeRange{Type: "relative", Range: 86400}, Streams: []string{defStream}, Limit: 5})
			if err != nil || res.Total == 0 || len(res.Messages) == 0 || len(res.Histogram) == 0 || res.Interval == "" {
				t.Fatalf("search = %+v, %v", res, err)
			}
			m := res.Messages[0]
			if m.ID == "" || m.Timestamp == "" || m.Fields["service"] != "order-api" || m.Fields["gl2_remote_ip"] != nil {
				t.Fatalf("message = %+v", m)
			}
			var sum int64
			for _, b := range res.Histogram {
				sum += b.Count
			}
			if sum != res.Total {
				t.Fatalf("histogram sums to %d, total %d", sum, res.Total)
			}
			t.Logf("search: %d hits, %d buckets of %s, took %dms", res.Total, len(res.Histogram), res.Interval, res.TookMs)
			if _, err := reg.Search(ctx, conn, SearchRequest{Query: "service:((", Range: TimeRange{Type: "relative", Range: 3600}}); err == nil {
				t.Fatal("broken query should fail")
			} else if ae, ok := AsAPIError(err); !ok || ae.Code != "bad_query" {
				t.Fatalf("broken query error = %v", err)
			}
			views, err := reg.Views(ctx, conn, "")
			if err != nil || len(views) == 0 || !strings.Contains(views[0].WebURL, "/dashboards/") {
				t.Fatalf("views = %+v, %v", views, err)
			}
		})
	}
	bad := models.GraylogConnection{URL: base, AuthKind: AuthBasic, Username: "admin", UpdatedAt: time.Now()}
	bad.SecretEnc, _ = cipher.Encrypt("wrong-password")
	if _, err := reg.Test(ctx, bad); err == nil {
		t.Fatal("wrong password should fail")
	} else if ae, ok := AsAPIError(err); !ok || ae.Code != "auth" {
		t.Fatalf("wrong password error = %v", err)
	}
}
