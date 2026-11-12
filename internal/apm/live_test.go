package apm

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/servicemap"
)

// TestCorootLive runs against a real Coroot when COROOT_LIVE_URL and
// COROOT_LIVE_TOKEN (a Viewer service account's crt_ key) are set.
func TestCorootLive(t *testing.T) {
	u, tok := os.Getenv("COROOT_LIVE_URL"), os.Getenv("COROOT_LIVE_TOKEN")
	if u == "" || tok == "" {
		t.Skip("COROOT_LIVE_URL / COROOT_LIVE_TOKEN not set")
	}
	cipher := testCipher(t)
	enc, _ := EncryptSecrets(cipher, Secrets{Token: tok, APIKey: os.Getenv("COROOT_LIVE_PROJECT_KEY")})
	cl, err := NewRegistry(cipher).Client(models.ObservabilityConnection{Provider: "coroot", URL: u, AuthKind: "bearer", SecretsEnc: enc})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := cl.Provider.Test(ctx, cl)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("test: %+v", res)
	b := servicemap.NewBuilder()
	st := &servicemap.SourceStatus{OK: true}
	if err := cl.Provider.Map(ctx, cl, MapRequest{Range: 15 * time.Minute}, b, st); err != nil {
		t.Fatal(err)
	}
	g := b.Graph(nil)
	t.Logf("nodes %d edges %d notes %v", len(g.Nodes), len(g.Edges), st.Notes)
	for _, e := range g.Edges {
		if strings.Contains(e.From, "shop") || strings.Contains(e.To, "shop") {
			t.Logf("edge %s -> %s %s %+v issue=%q", e.From, e.To, e.Protocol, e.Stats, e.Issue)
		}
	}
	for _, n := range g.Nodes {
		if n.Namespace == "shop" {
			t.Logf("node %s kind=%s sub=%s health=%s replicas=%s", n.ID, n.Kind, n.Subtype, n.Health, n.Replicas)
		}
	}
}
