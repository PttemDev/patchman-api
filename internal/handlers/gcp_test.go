package handlers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"golang.org/x/oauth2"
	"gorm.io/gorm"

	"patchman-be/internal/config"
	"patchman-be/internal/gcp"
	"patchman-be/internal/k8sclient"
	"patchman-be/internal/models"
	"patchman-be/internal/services"
)

const gcpSAKey = `{"type":"service_account","project_id":"shop-prod","client_email":"pm@shop-prod.iam.gserviceaccount.com","private_key":"-----BEGIN PRIVATE KEY-----\nx\n-----END PRIVATE KEY-----\n"}`

type gcpCall struct {
	Method, Path, Query string
	Body                map[string]any
}

func fakeGoogle(t *testing.T) (*httptest.Server, func() []gcpCall) {
	var mu sync.Mutex
	var calls []gcpCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		calls = append(calls, gcpCall{r.Method, r.URL.Path, r.URL.RawQuery, body})
		mu.Unlock()
		p := r.URL.Path
		switch {
		case p == "/crm/projects":
			_, _ = w.Write([]byte(`{"projects":[{"projectId":"shop-prod","name":"Shop"},{"projectId":"shop-dev","name":"Shop dev"}]}`))
		case p == "/storage/b":
			_, _ = w.Write([]byte(`{"items":[{"name":"invoices","location":"EU","storageClass":"STANDARD"}]}`))
		case p == "/storage/b/invoices/o" && r.Method == "GET":
			_, _ = w.Write([]byte(`{"prefixes":["2026/"],"items":[{"name":"readme.txt","size":"5","contentType":"text/plain"}]}`))
		case p == "/storage/b/invoices/o/readme.txt" && r.URL.Query().Get("alt") == "media":
			_, _ = w.Write([]byte("hello"))
		case p == "/storage/b/invoices/o/readme.txt" && r.Method == "GET":
			_, _ = w.Write([]byte(`{"name":"readme.txt","size":"5","contentType":"text/plain"}`))
		case p == "/storage/b/invoices/o/readme.txt" && r.Method == "DELETE":
			w.WriteHeader(204)
		case p == "/upload/b/invoices/o":
			_, _ = w.Write([]byte(`{"name":"` + r.URL.Query().Get("name") + `","size":"` + itoa(len(raw)) + `"}`))
		case p == "/pubsub/projects/shop-prod/topics":
			_, _ = w.Write([]byte(`{"topics":[{"name":"projects/shop-prod/topics/orders"}]}`))
		case p == "/pubsub/projects/shop-prod/subscriptions":
			_, _ = w.Write([]byte(`{"subscriptions":[{"name":"projects/shop-prod/subscriptions/orders-worker","topic":"projects/shop-prod/topics/orders","ackDeadlineSeconds":10,"deadLetterPolicy":{"deadLetterTopic":"projects/shop-prod/topics/orders-dlq","maxDeliveryAttempts":5}}]}`))
		case p == "/pubsub/projects/shop-prod/subscriptions/orders-worker:pull":
			_, _ = w.Write([]byte(`{"receivedMessages":[{"ackId":"a1","message":{"data":"` + base64.StdEncoding.EncodeToString([]byte(`{"order":7}`)) + `","messageId":"m1","attributes":{"k":"v"}},"deliveryAttempt":2}]}`))
		case p == "/pubsub/projects/shop-prod/subscriptions/orders-worker:modifyAckDeadline":
			_, _ = w.Write([]byte(`{}`))
		case p == "/pubsub/projects/shop-prod/topics/orders:publish":
			_, _ = w.Write([]byte(`{"messageIds":["m9"]}`))
		case p == "/logging/entries:list":
			_, _ = w.Write([]byte(`{"entries":[{"insertId":"i1","logName":"projects/shop-prod/logs/stderr","timestamp":"2026-11-18T10:00:00Z","severity":"ERROR","jsonPayload":{"message":"boom","code":7},"resource":{"type":"k8s_container","labels":{"pod_name":"api-0"}}},{"insertId":"i2","logName":"projects/shop-prod/logs/run.googleapis.com%2Frequests","timestamp":"2026-11-18T09:59:00Z","textPayload":"GET /"}],"nextPageToken":"n2"}`))
		case p == "/container/projects/shop-prod/locations/-/clusters":
			_, _ = w.Write([]byte(`{"clusters":[{"name":"prod","location":"europe-west1","status":"RUNNING","endpoint":"10.0.0.1","currentMasterVersion":"1.31","currentNodeCount":3}]}`))
		case p == "/container/projects/shop-prod/locations/europe-west1/clusters/prod":
			_, _ = w.Write([]byte(`{"name":"prod","endpoint":"` + strings.TrimPrefix(r.Host, "") + `","masterAuth":{"clusterCaCertificate":"` + base64.StdEncoding.EncodeToString([]byte("not-a-real-ca")) + `"}}`))
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":{"code":404,"message":"Not found: ` + p + `"}}`))
		}
	}))
	return srv, func() []gcpCall { mu.Lock(); defer mu.Unlock(); return append([]gcpCall(nil), calls...) }
}

type gcpRig struct {
	h     *GCPHandler
	db    *gorm.DB
	calls func() []gcpCall
}

func newGCPRig(t *testing.T) *gcpRig {
	t.Helper()
	srv, calls := fakeGoogle(t)
	t.Cleanup(srv.Close)
	gcp.UseEndpoint(srv.URL)
	t.Cleanup(func() { gcp.UseEndpoint("") })
	db, err := gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.GCPCredential{}, &models.K8sConnection{}, &models.DBConnection{}, &models.OAuthPendingState{}); err != nil {
		t.Fatal(err)
	}
	cipher, _ := services.NewTokenCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	gin.SetMode(gin.TestMode)
	return &gcpRig{h: NewGCPHandler(db, cipher, &config.Config{GoogleClientID: "cid", GoogleClientSecret: "cs", AllowedRedirectOrigins: []string{"https://pm.example"}}), db: db, calls: calls}
}

func (r *gcpRig) do(user, method, q, id string, body any, fn gin.HandlerFunc) (int, map[string]any) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	var rd io.Reader
	ct := "application/json"
	switch b := body.(type) {
	case nil:
	case *bytes.Buffer:
		rd = b
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	if mb, ok := body.(*bytes.Buffer); ok {
		_ = mb
		ct = multipartCT
	}
	c.Request = httptest.NewRequest(method, "/x?"+q, rd)
	c.Request.Header.Set("Content-Type", ct)
	c.Params = gin.Params{{Key: "id", Value: id}}
	c.Set("userID", user)
	fn(c)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

var multipartCT string

func TestGCPCredentials(t *testing.T) {
	r := newGCPRig(t)
	h := r.h
	if code, _ := r.do("u1", "POST", "", "", map[string]any{"kind": "service_account", "key": `{"type":"authorized_user"}`}, h.Create); code != 400 {
		t.Errorf("not a SA key: %d", code)
	}
	code, out := r.do("u1", "POST", "", "", map[string]any{"kind": "service_account", "key": gcpSAKey}, h.Create)
	d := out["data"].(map[string]any)
	if code != 201 || d["projectId"] != "shop-prod" || d["account"] != "pm@shop-prod.iam.gserviceaccount.com" || d["readWrite"] != false {
		t.Fatalf("create: %d %v", code, out)
	}
	id := d["id"].(string)
	var row models.GCPCredential
	r.db.First(&row, "id = ?", id)
	if row.SecretEnc == "" || strings.Contains(row.SecretEnc, "PRIVATE KEY") {
		t.Error("key stored encrypted")
	}
	_, out = r.do("u1", "GET", "", "", nil, h.List)
	if b, _ := json.Marshal(out); strings.Contains(string(b), "PRIVATE") || strings.Contains(string(b), "secret") {
		t.Errorf("list leaks the key: %s", b)
	}
	if out["meta"].(map[string]any)["oauthAvailable"] != true {
		t.Errorf("meta: %v", out["meta"])
	}
	if code, _ = r.do("u2", "GET", "", id, nil, h.Projects); code != 404 {
		t.Errorf("another user can't use it: %d", code)
	}
	_, out = r.do("u1", "GET", "", id, nil, h.Projects)
	if ps := out["data"].([]any); len(ps) != 2 {
		t.Errorf("projects: %v", out)
	}
	embeddedMode.Store(false)
	if code, _ = r.do("u1", "POST", "", "", map[string]any{"kind": "adc"}, h.Create); code != 400 {
		t.Errorf("gcloud credentials only on the desktop: %d", code)
	}
}

func (r *gcpRig) sa(t *testing.T, rw bool) string {
	_, out := r.do("u1", "POST", "", "", map[string]any{"kind": "service_account", "key": gcpSAKey, "readWrite": rw}, r.h.Create)
	return out["data"].(map[string]any)["id"].(string)
}

func TestGCPStorage(t *testing.T) {
	r := newGCPRig(t)
	h := r.h
	id := r.sa(t, false)
	_, out := r.do("u1", "GET", "", id, nil, h.Buckets)
	if b := out["data"].([]any); len(b) != 1 || b[0].(map[string]any)["name"] != "invoices" {
		t.Errorf("buckets: %v", out)
	}
	_, out = r.do("u1", "GET", "bucket=invoices", id, nil, h.Objects)
	d := out["data"].(map[string]any)
	if d["prefixes"].([]any)[0] != "2026/" || d["objects"].([]any)[0].(map[string]any)["size"].(float64) != 5 {
		t.Errorf("objects: %v", out)
	}
	_, out = r.do("u1", "GET", "bucket=invoices&name=readme.txt", id, nil, h.Object)
	if c, _ := base64.StdEncoding.DecodeString(out["data"].(map[string]any)["content"].(string)); string(c) != "hello" {
		t.Errorf("object: %v", out)
	}
	if code, _ := r.do("u1", "DELETE", "bucket=invoices&name=readme.txt&allowWrite=true", id, nil, h.DeleteObject); code != 403 {
		t.Errorf("read-only credential can't delete: %d", code)
	}
	rw := r.sa(t, true)
	if code, _ := r.do("u1", "DELETE", "bucket=invoices&name=readme.txt", rw, nil, h.DeleteObject); code != 400 {
		t.Errorf("delete without allowWrite: %d", code)
	}
	if code, _ := r.do("u1", "DELETE", "bucket=invoices&name=readme.txt&allowWrite=true", rw, nil, h.DeleteObject); code != 200 {
		t.Errorf("delete: %d", code)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("bucket", "invoices")
	_ = mw.WriteField("name", "2026/a.csv")
	_ = mw.WriteField("allowWrite", "true")
	fw, _ := mw.CreateFormFile("file", "a.csv")
	_, _ = fw.Write([]byte("a,b\n"))
	_ = mw.Close()
	multipartCT = mw.FormDataContentType()
	code, out := r.do("u1", "POST", "", rw, &buf, h.Upload)
	if code != 200 || out["data"].(map[string]any)["name"] != "2026/a.csv" || out["data"].(map[string]any)["size"].(float64) != 4 {
		t.Errorf("upload: %d %v", code, out)
	}
}

func TestGCPPubSubAndLogs(t *testing.T) {
	r := newGCPRig(t)
	h := r.h
	id := r.sa(t, false)
	_, out := r.do("u1", "GET", "", id, nil, h.Topics)
	if out["data"].([]any)[0].(map[string]any)["name"] != "orders" {
		t.Errorf("topics: %v", out)
	}
	_, out = r.do("u1", "GET", "topic=orders", id, nil, h.Subscriptions)
	s := out["data"].([]any)[0].(map[string]any)
	if s["name"] != "orders-worker" || s["deadLetterTopic"] != "orders-dlq" {
		t.Errorf("subs: %v", out)
	}
	code, out := r.do("u1", "POST", "", id, map[string]any{"subscription": "orders-worker", "max": 5}, h.Peek)
	pd := out["data"].(map[string]any)
	m := pd["messages"].([]any)[0].(map[string]any)
	if code != 200 || m["data"] != `{"order":7}` || pd["released"] != true || !strings.Contains(pd["note"].(string), "delivery attempt") {
		t.Errorf("peek: %d %v", code, out)
	}
	cs := r.calls()
	last := cs[len(cs)-1]
	if !strings.HasSuffix(last.Path, ":modifyAckDeadline") || last.Body["ackDeadlineSeconds"].(float64) != 0 || last.Body["ackIds"].([]any)[0] != "a1" {
		t.Errorf("peek must hand the messages back: %+v", last)
	}
	if code, _ = r.do("u1", "POST", "", id, map[string]any{"topic": "orders", "data": "x", "allowWrite": true}, h.Publish); code != 403 {
		t.Errorf("read-only publish: %d", code)
	}
	rw := r.sa(t, true)
	if code, out = r.do("u1", "POST", "", rw, map[string]any{"topic": "orders", "data": "hi", "allowWrite": true}, h.Publish); code != 200 {
		t.Errorf("publish: %d %v", code, out)
	}
	cs = r.calls()
	if b := cs[len(cs)-1].Body["messages"].([]any)[0].(map[string]any)["data"]; b != base64.StdEncoding.EncodeToString([]byte("hi")) {
		t.Errorf("publish encodes data: %v", b)
	}

	_, out = r.do("u1", "POST", "", id, map[string]any{"filter": `severity>=ERROR`, "from": "2026-11-18T09:00:00Z"}, h.Logs)
	ld := out["data"].(map[string]any)
	es := ld["entries"].([]any)
	if len(es) != 2 || es[0].(map[string]any)["message"] != "boom" || es[0].(map[string]any)["log"] != "stderr" || es[1].(map[string]any)["log"] != "run.googleapis.com/requests" || ld["nextPageToken"] != "n2" {
		t.Errorf("logs: %v", out)
	}
	cs = r.calls()
	lb := cs[len(cs)-1].Body
	if lb["filter"] != `(severity>=ERROR) AND timestamp>="2026-11-18T09:00:00Z"` || lb["resourceNames"].([]any)[0] != "projects/shop-prod" || lb["orderBy"] != "timestamp desc" {
		t.Errorf("logging request: %v", lb)
	}
}

// A GKE cluster becomes a Kubernetes connection whose requests carry a token
// from the credential.
func TestGCPAddGKECluster(t *testing.T) {
	r := newGCPRig(t)
	id := r.sa(t, false)
	_, out := r.do("u1", "GET", "", id, nil, r.h.Clusters)
	if c := out["data"].([]any)[0].(map[string]any); c["name"] != "prod" || c["nodes"].(float64) != 3 {
		t.Errorf("clusters: %v", out)
	}
	code, out := r.do("u1", "POST", "", id, map[string]any{"location": "europe-west1", "cluster": "prod"}, r.h.AddCluster)
	if code != 201 {
		t.Fatalf("add: %d %v", code, out)
	}
	var k models.K8sConnection
	r.db.First(&k, "id = ?", out["data"].(map[string]any)["id"])
	if k.AuthMode != "gke" || k.UserID != "u1" || !strings.Contains(k.OptionsJSON, id) || k.DefaultContext != "gke_shop-prod_europe-west1_prod" {
		t.Errorf("connection: %+v", k)
	}
	// Token source for the owner only.
	if _, err := r.h.TokenSourceFor("u2", id); err == nil {
		t.Error("another user's credential must not resolve")
	}
	ts, err := r.h.TokenSourceFor("u1", id)
	if err != nil {
		t.Fatal(err)
	}
	if tok, _ := ts.Token(); tok.AccessToken == "" {
		t.Error("token")
	}
	// The usage check names the cluster before deleting the credential.
	if code, out = r.do("u1", "DELETE", "", id, nil, r.h.Delete); code != 409 || !strings.Contains(out["usedBy"].([]any)[0].(string), "prod") {
		t.Errorf("delete in use: %d %v", code, out)
	}
	if code, _ = r.do("u1", "DELETE", "confirm=1", id, nil, r.h.Delete); code != 200 {
		t.Errorf("confirmed delete: %d", code)
	}
}

func TestK8sGKEAuthHeader(t *testing.T) {
	var auth string
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"major":"1","minor":"31","gitVersion":"v1.31.0"}`))
	}))
	defer api.Close()
	old := k8sclient.GCPTokenSource
	defer func() { k8sclient.GCPTokenSource = old }()
	k8sclient.GCPTokenSource = func(owner, cred string) (oauth2.TokenSource, error) {
		if owner != "u1" || cred != "c1" {
			t.Errorf("owner/cred %s %s", owner, cred)
		}
		return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "ya29.test"}), nil
	}
	cipher, _ := services.NewTokenCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	kc := "apiVersion: v1\nkind: Config\nclusters:\n- name: g\n  cluster:\n    server: " + api.URL + "\n    insecure-skip-tls-verify: true\ncontexts:\n- name: g\n  context:\n    cluster: g\n    user: g\ncurrent-context: g\nusers:\n- name: g\n  user: {}\n"
	enc, _ := cipher.Encrypt(kc)
	reg := k8sclient.NewRegistry(cipher, false)
	v, err := reg.Test(context.Background(), models.K8sConnection{ID: "k1", UserID: "u1", KubeconfigEnc: enc, OptionsJSON: `{"gcpCredentialId":"c1"}`}, "")
	if err != nil || v.Version != "v1.31.0" || auth != "Bearer ya29.test" {
		t.Fatalf("version %v err %v auth %q", v, err, auth)
	}
}

func TestGCPOAuthLink(t *testing.T) {
	r := newGCPRig(t)
	code, out := r.do("u1", "POST", "", "", map[string]any{"origin": "https://pm.example", "services": []string{"storage", "logging"}}, r.h.OAuthStart)
	if code != 200 {
		t.Fatalf("start: %d %v", code, out)
	}
	u := out["data"].(map[string]any)["url"].(string)
	for _, want := range []string{"access_type=offline", "include_granted_scopes=true", "devstorage.read_only", "logging.read", "redirect_uri=https%3A%2F%2Fpm.example%2Fauth%2Fgoogle-cloud%2Fcallback"} {
		if !strings.Contains(u, want) {
			t.Errorf("auth url lacks %s: %s", want, u)
		}
	}
	if strings.Contains(u, "devstorage.read_write") || strings.Contains(u, "pubsub") {
		t.Errorf("only the asked-for, read-only scopes: %s", u)
	}
	if code, _ = r.do("u1", "POST", "", "", map[string]any{"origin": "https://evil.example"}, r.h.OAuthStart); code != 400 {
		t.Errorf("foreign origin: %d", code)
	}
	var p models.OAuthPendingState
	r.db.Where("kind = ?", gcpOAuthKind).First(&p)
	old := gcpExchange
	defer func() { gcpExchange = old }()
	idt := "x." + base64.RawURLEncoding.EncodeToString([]byte(`{"email":"ada@shop.com"}`)) + ".y"
	gcpExchange = func(_ context.Context, cfg *oauth2.Config, code string) (*oauth2.Token, error) {
		if code != "c0de" {
			t.Errorf("code %s", code)
		}
		return (&oauth2.Token{AccessToken: "a", RefreshToken: "1//refresh"}).WithExtra(map[string]any{"id_token": idt}), nil
	}
	if code, _ = r.do("u2", "POST", "", "", map[string]any{"code": "c0de", "state": p.State}, r.h.OAuthComplete); code != 400 {
		t.Errorf("someone else's state: %d", code)
	}
	r.db.Create(&p) // the failed attempt above consumed nothing; recreate if deleted
	code, out = r.do("u1", "POST", "", "", map[string]any{"code": "c0de", "state": p.State}, r.h.OAuthComplete)
	if code != 200 || out["data"].(map[string]any)["kind"] != "oauth" || out["data"].(map[string]any)["account"] != "ada@shop.com" {
		t.Fatalf("complete: %d %v", code, out)
	}
	var row models.GCPCredential
	r.db.First(&row, "kind = ?", "oauth")
	if row.SecretEnc == "" || strings.Contains(row.SecretEnc, "refresh") {
		t.Error("refresh token stored encrypted")
	}
	if code, _ = r.do("u1", "POST", "", "", map[string]any{"code": "c0de", "state": p.State}, r.h.OAuthComplete); code != 400 {
		t.Errorf("a state works once: %d", code)
	}
}
