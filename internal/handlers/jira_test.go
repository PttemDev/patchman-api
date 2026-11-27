package handlers

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"patchman-be/internal/jira/jiratest"
	"patchman-be/internal/models"
	"patchman-be/internal/services"
)

func jiraTestHandlers(t *testing.T) (*JiraConnectionHandler, *JiraGrantHandler, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s_%d?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"), time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.JiraConnection{}, &models.JiraConnectionGrant{}, &models.JiraActivityHistory{}); err != nil {
		t.Fatal(err)
	}
	db.Exec("CREATE TABLE IF NOT EXISTS team_members (team_id TEXT, user_id TEXT)")
	db.Exec("CREATE TABLE IF NOT EXISTS users (id TEXT, email TEXT, name TEXT)")
	cipher, _ := services.NewTokenCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	conns := NewJiraConnectionHandler(db, cipher)
	return conns, NewJiraGrantHandler(db, conns), db
}

// jiraCall runs a handler with path params (id, key) and a query string.
func jiraCall(t *testing.T, fn gin.HandlerFunc, user, method string, params map[string]string, query string, body any) (int, map[string]any, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, "/x?"+query, rdr)
	c.Request.Header.Set("Content-Type", "application/json")
	for k, v := range params {
		c.Params = append(c.Params, gin.Param{Key: k, Value: v})
	}
	c.Set("userID", user)
	fn(c)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out, w.Body.String()
}

func TestJiraConnectionFlavors(t *testing.T) {
	for _, flavor := range []string{"cloud", "datacenter"} {
		t.Run(flavor, func(t *testing.T) {
			conns, grants, db := jiraTestHandlers(t)
			fake := jiratest.New(flavor)
			t.Cleanup(fake.Close)
			secret, who := jiratest.PAT, "Ada (DC)"
			payload := map[string]any{"name": "jira", "baseUrl": fake.URL + "/browse/MER-1", "flavor": flavor, "secret": secret,
				"options": map[string]any{"acFieldId": jiratest.ACField, "defaultJql": "project = MER", "junk": "x"}}
			if flavor == "cloud" {
				secret, who = jiratest.APIToken, "Ada Lovelace"
				payload["secret"], payload["email"] = secret, jiratest.Email
			}
			noSecret := func(where, s string) {
				t.Helper()
				if strings.Contains(s, secret) {
					t.Fatalf("%s leaks the secret: %s", where, s)
				}
			}

			// Unsaved Test (before a name is typed) → display name + custom
			// fields for the AC picker.
			unnamed := map[string]any{}
			for k, v := range payload {
				unnamed[k] = v
			}
			unnamed["name"] = ""
			code, out, raw := jiraCall(t, conns.Test, "owner", "POST", nil, "", unnamed)
			td, _ := out["data"].(map[string]any)
			if code != 200 || td["ok"] != true || td["displayName"] != who || len(td["fields"].([]any)) != 2 || td["warning"] != nil {
				t.Fatalf("test draft = %d %s", code, raw)
			}
			noSecret("test", raw)

			code, out, raw = jiraCall(t, conns.Create, "owner", "POST", nil, "", payload)
			if code != 201 {
				t.Fatalf("create = %d %s", code, raw)
			}
			noSecret("create", raw)
			d := out["data"].(map[string]any)
			id := d["id"].(string)
			wantKind := map[string]string{"cloud": "email_token", "datacenter": "pat"}[flavor]
			if d["baseUrl"] != fake.URL || d["authKind"] != wantKind || d["hasSecret"] != true ||
				mustJSON(d["options"]) != `{"acFieldId":"`+jiratest.ACField+`","defaultJql":"project = MER"}` {
				t.Fatalf("create dto = %s", raw)
			}
			var stored models.JiraConnection
			db.First(&stored, "id = ?", id)
			if stored.CredentialEnc == "" || strings.Contains(stored.CredentialEnc, secret) {
				t.Fatal("secret must be stored encrypted")
			}

			// Saved Test; a changed URL never gets the stored token.
			if _, out, _ := jiraCall(t, conns.Test, "owner", "POST", map[string]string{"id": id}, "", nil); out["data"].(map[string]any)["ok"] != true {
				t.Fatalf("test saved = %v", out)
			}
			before := len(fake.Seen())
			_, out, _ = jiraCall(t, conns.Test, "owner", "POST", map[string]string{"id": id}, "", map[string]any{"baseUrl": "http://elsewhere.example"})
			if td := out["data"].(map[string]any); td["ok"] != false || !strings.Contains(fmt.Sprint(td["error"]), "again") || len(fake.Seen()) != before {
				t.Fatalf("test changed url = %v", out)
			}
			other := map[string]string{"cloud": "datacenter", "datacenter": "cloud"}[flavor]
			if code, _, _ := jiraCall(t, conns.Update, "owner", "PATCH", map[string]string{"id": id}, "", map[string]any{"flavor": other, "email": "x@example.com"}); code != 400 {
				t.Fatalf("switch flavor without token = %d", code)
			}
			if code, _, _ := jiraCall(t, conns.Update, "owner", "PATCH", map[string]string{"id": id}, "", map[string]any{"options": map[string]any{"acFieldId": "../x"}}); code != 400 {
				t.Fatalf("bad acFieldId = %d", code)
			}

			// Edit without a token: name, URL and AC field change, the stored
			// token is kept and still works.
			code, out, raw = jiraCall(t, conns.Update, "owner", "PATCH", map[string]string{"id": id}, "", map[string]any{
				"name": "jira 2", "baseUrl": fake.URL + "/", "options": map[string]any{"acFieldId": "customfield_1", "defaultJql": "project = MER"}})
			if ud, _ := out["data"].(map[string]any); code != 200 || ud["name"] != "jira 2" || ud["hasSecret"] != true ||
				mustJSON(ud["options"]) != `{"acFieldId":"customfield_1","defaultJql":"project = MER"}` {
				t.Fatalf("update without token = %d %s", code, raw)
			}
			noSecret("update", raw)
			if _, out, raw := jiraCall(t, conns.Test, "owner", "POST", map[string]string{"id": id}, "", nil); out["data"].(map[string]any)["ok"] != true {
				t.Fatalf("test after edit = %s", raw)
			}
			// A new (wrong) token is stored and used; the right one restores it.
			jiraCall(t, conns.Update, "owner", "PATCH", map[string]string{"id": id}, "", map[string]any{"secret": "wrong-token-yy"})
			if _, out, _ := jiraCall(t, conns.Test, "owner", "POST", map[string]string{"id": id}, "", nil); out["data"].(map[string]any)["ok"] != false {
				t.Fatalf("test with the new wrong token = %v", out)
			}
			jiraCall(t, conns.Update, "owner", "PATCH", map[string]string{"id": id}, "", map[string]any{"secret": secret,
				"options": map[string]any{"acFieldId": jiratest.ACField, "defaultJql": "project = MER"}})
			if _, out, _ := jiraCall(t, conns.Test, "owner", "POST", map[string]string{"id": id}, "", nil); out["data"].(map[string]any)["ok"] != true {
				t.Fatalf("test with the restored token = %v", out)
			}

			// Stranger: 404 on every read, no Jira call.
			before = len(fake.Seen())
			for name, fn := range map[string]gin.HandlerFunc{"get": conns.Get, "search": conns.Search, "fields": conns.Fields, "issue": conns.Issue, "grants": grants.List} {
				if code, _, _ := jiraCall(t, fn, "stranger", "GET", map[string]string{"id": id, "key": "MER-936"}, "", nil); code != 404 {
					t.Fatalf("stranger %s = %d", name, code)
				}
			}
			if code, _, _ := jiraCall(t, conns.Test, "stranger", "POST", map[string]string{"id": id}, "", nil); code != 404 {
				t.Fatalf("stranger test = %d", code)
			}
			if len(fake.Seen()) != before {
				t.Fatal("a stranger's call reached Jira")
			}

			// Grantee reads; can't edit, delete, reveal or re-share.
			if code, _, raw := jiraCall(t, grants.Add, "owner", "POST", map[string]string{"id": id}, "", map[string]any{"subjectKind": "user", "subjectId": "reader"}); code != 201 {
				t.Fatalf("grant = %d %s", code, raw)
			}
			code, out, raw = jiraCall(t, conns.List, "reader", "GET", nil, "", nil)
			if code != 200 || len(out["data"].([]any)) != 1 || out["data"].([]any)[0].(map[string]any)["grantRole"] != "read" {
				t.Fatalf("grantee list = %s", raw)
			}
			noSecret("list", raw)
			for name, call := range map[string]func() int{
				"update": func() int {
					c, _, _ := jiraCall(t, conns.Update, "reader", "PATCH", map[string]string{"id": id}, "", map[string]any{"name": "x"})
					return c
				},
				"delete": func() int {
					c, _, _ := jiraCall(t, conns.Delete, "reader", "DELETE", map[string]string{"id": id}, "", nil)
					return c
				},
				"reveal": func() int {
					c, _, _ := jiraCall(t, conns.RevealSecret, "reader", "POST", map[string]string{"id": id}, "", nil)
					return c
				},
				"share": func() int {
					c, _, _ := jiraCall(t, grants.Add, "reader", "POST", map[string]string{"id": id}, "", map[string]any{"subjectKind": "user", "subjectId": "x"})
					return c
				},
			} {
				if code := call(); code != 404 {
					t.Fatalf("grantee %s = %d", name, code)
				}
			}

			// Search: default JQL, paging cursor, total per flavor.
			code, out, raw = jiraCall(t, conns.Search, "reader", "GET", map[string]string{"id": id}, "limit=2", nil)
			if code != 200 {
				t.Fatalf("search = %d %s", code, raw)
			}
			sd := out["data"].(map[string]any)
			if sd["jql"] != "project = MER" || len(sd["issues"].([]any)) != 2 || sd["next"] == "" || sd["isLast"] != false {
				t.Fatalf("search = %s", raw)
			}
			if (flavor == "cloud") != (sd["total"] == nil) {
				t.Fatalf("total for %s = %v", flavor, sd["total"])
			}
			code, out, raw = jiraCall(t, conns.Search, "reader", "GET", map[string]string{"id": id}, "limit=2&next="+fmt.Sprint(sd["next"]), nil)
			if code != 200 || out["data"].(map[string]any)["issues"].([]any)[0].(map[string]any)["key"] != "MER-3" {
				t.Fatalf("search page 2 = %d %s", code, raw)
			}
			if code, out, _ := jiraCall(t, conns.Search, "reader", "GET", map[string]string{"id": id}, "jql=project+%3D+((", nil); code != 400 || out["code"] != "bad_query" {
				t.Fatalf("bad jql = %d %v", code, out)
			}

			// Issue with the AC field (ADF on Cloud, wiki on DC) and sprint.
			code, out, raw = jiraCall(t, conns.Issue, "reader", "GET", map[string]string{"id": id, "key": "mer-936"}, "", nil)
			if code != 200 {
				t.Fatalf("issue = %d %s", code, raw)
			}
			is := out["data"].(map[string]any)
			ac := fmt.Sprint(is["acceptanceCriteria"])
			if !strings.Contains(ac, "Given a merchant") || !strings.Contains(ac, "- lists orders by date") || !strings.Contains(ac, "- sees 20 per page") {
				t.Fatalf("AC (%s) = %q", flavor, ac)
			}
			if mustJSON(is["sprints"]) != `["Sprint 12 (active)"]` && mustJSON(is["sprints"]) != `["Sprint 12"]` {
				t.Fatalf("sprints = %v", is["sprints"])
			}
			if code, out, _ := jiraCall(t, conns.Issue, "reader", "GET", map[string]string{"id": id, "key": "../myself"}, "", nil); code != 400 || out["code"] != "bad_request" {
				t.Fatalf("crafted key = %d %v", code, out)
			}
			if code, out, _ := jiraCall(t, conns.Issue, "reader", "GET", map[string]string{"id": id, "key": "MER-1"}, "", nil); code != 404 || out["code"] != "not_found" {
				t.Fatalf("missing issue = %d %v", code, out)
			}
			code, out, raw = jiraCall(t, conns.Fields, "reader", "GET", map[string]string{"id": id}, "", nil)
			if code != 200 || len(out["data"].([]any)) != 3 {
				t.Fatalf("fields = %d %s", code, raw)
			}

			// Audit: the grantee's reads against the owner's connection; no secret.
			time.Sleep(100 * time.Millisecond)
			var rows []models.JiraActivityHistory
			db.Where("connection_id = ?", id).Order("executed_at").Find(&rows)
			if len(rows) != 5 || rows[0].UserID != "reader" || rows[0].DelegatedOwner == nil || *rows[0].DelegatedOwner != "owner" ||
				rows[0].Action != "search" || rows[0].Target != "project = MER" || rows[3].Action != "issue" || rows[3].Target != "MER-936" {
				t.Fatalf("audit = %+v", rows)
			}
			noSecret("audit", mustJSON(rows))

			// Wrong token: a readable auth failure (200 ok:false), no secret echoed.
			bad := map[string]any{"name": "x", "baseUrl": fake.URL, "flavor": flavor, "email": jiratest.Email, "secret": "wrong-token-zz"}
			_, out, raw = jiraCall(t, conns.Test, "owner", "POST", nil, "", bad)
			if td := out["data"].(map[string]any); td["ok"] != false || td["code"] != "auth" || strings.Contains(raw, "wrong-token-zz") {
				t.Fatalf("wrong token = %s", raw)
			}

			// Owner reveal works; delete drops the grants.
			if code, out, _ := jiraCall(t, conns.RevealSecret, "owner", "POST", map[string]string{"id": id}, "", nil); code != 200 || out["data"].(map[string]any)["value"] != secret {
				t.Fatalf("owner reveal = %d", code)
			}
			if code, _, _ := jiraCall(t, conns.Delete, "owner", "DELETE", map[string]string{"id": id}, "", nil); code != 200 {
				t.Fatalf("delete = %d", code)
			}
			var n int64
			db.Model(&models.JiraConnectionGrant{}).Where("connection_id = ?", id).Count(&n)
			if n != 0 {
				t.Fatalf("grants left after delete: %d", n)
			}
		})
	}
}

func TestJiraFlavorMismatchWarning(t *testing.T) {
	conns, _, _ := jiraTestHandlers(t)
	fake := jiratest.New("datacenter")
	t.Cleanup(fake.Close)
	// The DC fake reports deploymentType Server; a Cloud connection can't
	// sign in there (wrong header shape) — the auth error comes first.
	_, out, _ := jiraCall(t, conns.Test, "owner", "POST", nil, "", map[string]any{"name": "x", "baseUrl": fake.URL, "flavor": "cloud", "email": jiratest.Email, "secret": jiratest.PAT})
	if td := out["data"].(map[string]any); td["ok"] != false || td["code"] != "auth" {
		t.Fatalf("cloud against DC = %v", out)
	}
	if code, out, _ := jiraCall(t, conns.Create, "owner", "POST", nil, "", map[string]any{"name": "x", "baseUrl": fake.URL, "flavor": "cloud", "secret": "t"}); code != 400 || !strings.Contains(fmt.Sprint(out["error"]), "email") {
		t.Fatalf("cloud without email = %d %v", code, out)
	}
	if code, _, _ := jiraCall(t, conns.Create, "owner", "POST", nil, "", map[string]any{"name": "x", "baseUrl": "https://u:p@jira.example.com", "flavor": "datacenter", "secret": "t"}); code != 400 {
		t.Fatalf("userinfo URL = %d", code)
	}
}
