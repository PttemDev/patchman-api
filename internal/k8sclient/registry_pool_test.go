package k8sclient

import (
	"testing"

	"patchman-be/internal/models"
	"patchman-be/internal/services"
)

func poolTestKubeconfig(token string) string {
	return `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://rancher.example.invalid/k8s/clusters/c-1
contexts:
- name: ctx
  context:
    cluster: c
    user: u
current-context: ctx
users:
- name: u
  user:
    token: ` + token + "\n"
}

// Regression: a request that loaded the connection row before a kubeconfig
// edit can land after Close() and re-pool the old credentials. The pool must
// rebuild whenever the row's kubeconfig differs from the pooled one.
func TestResolveRebuildsOnKubeconfigChange(t *testing.T) {
	cipher, err := services.NewTokenCipher("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	if err != nil {
		t.Fatal(err)
	}
	r := &Registry{cipher: cipher, pooled: map[string]*pooled{}, promCache: map[string]promCacheEntry{}}

	enc := func(tok string) string {
		s, err := cipher.Encrypt(poolTestKubeconfig(tok))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	oldConn := models.K8sConnection{ID: "conn-1", DefaultContext: "ctx", KubeconfigEnc: enc("old-token")}
	newConn := oldConn
	newConn.KubeconfigEnc = enc("new-token")

	// Stale request pools the old kubeconfig (e.g. after the edit's Close).
	p1, err := r.resolve(oldConn, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := p1.rest.BearerToken; got != "old-token" {
		t.Fatalf("old entry token = %q", got)
	}

	// A request with the edited row must not reuse it.
	p2, err := r.resolve(newConn, "")
	if err != nil {
		t.Fatal(err)
	}
	if p2 == p1 || p2.rest.BearerToken != "new-token" {
		t.Fatalf("stale pooled client reused after kubeconfig change (token %q)", p2.rest.BearerToken)
	}

	// Same row again → pooled entry is reused.
	p3, err := r.resolve(newConn, "")
	if err != nil {
		t.Fatal(err)
	}
	if p3 != p2 {
		t.Fatal("unchanged connection should reuse the pooled client")
	}
}
