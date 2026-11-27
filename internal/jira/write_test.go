package jira

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWritesBothFlavors(t *testing.T) {
	for _, flavor := range []string{FlavorCloud, FlavorDataCenter} {
		var got []map[string]any
		var paths []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			got = append(got, m)
			paths = append(paths, r.URL.Path)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"1","key":"MER-77"}`))
		}))
		cl, err := New(Config{BaseURL: srv.URL, Flavor: flavor, Email: "a@example.com", Secret: "s"})
		if err != nil {
			t.Fatal(err)
		}
		if err := cl.AddComment(context.Background(), "MER-1", "line1\nline2\n\npara2"); err != nil {
			t.Fatal(err)
		}
		key, err := cl.CreateIssue(context.Background(), "MER", "", "Login fails", "steps")
		if err != nil || key != "MER-77" {
			t.Fatalf("%s create: %v %s", flavor, err, key)
		}
		srv.Close()
		api := "/rest/api/3"
		if flavor == FlavorDataCenter {
			api = "/rest/api/2"
		}
		if paths[0] != api+"/issue/MER-1/comment" || paths[1] != api+"/issue" {
			t.Fatalf("%s paths %v", flavor, paths)
		}
		_, isText := got[0]["body"].(string)
		if (flavor == FlavorDataCenter) != isText {
			t.Fatalf("%s body shape: %v", flavor, got[0]["body"])
		}
		f := got[1]["fields"].(map[string]any)
		if f["issuetype"].(map[string]any)["name"] != "Bug" || f["project"].(map[string]any)["key"] != "MER" {
			t.Fatalf("%s fields %v", flavor, f)
		}
	}
}

func TestProjectsBothFlavors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/api/2/project":
			_, _ = w.Write([]byte(`[{"key":"MER","name":"Merchant"},{"key":"PAY","name":"Payments"}]`))
		case "/rest/api/3/project/search":
			if r.URL.Query().Get("query") != "mer" {
				t.Errorf("cloud query %q", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"values":[{"key":"MER","name":"Merchant"}],"isLast":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	dc, _ := New(Config{BaseURL: srv.URL, Flavor: FlavorDataCenter, Secret: "s"})
	ps, err := dc.Projects(context.Background(), "pay")
	if err != nil || len(ps) != 1 || ps[0].Key != "PAY" {
		t.Fatalf("dc: %v %v", ps, err)
	}
	cl, _ := New(Config{BaseURL: srv.URL, Flavor: FlavorCloud, Email: "a@example.com", Secret: "s"})
	ps, err = cl.Projects(context.Background(), "mer")
	if err != nil || len(ps) != 1 || ps[0].Name != "Merchant" {
		t.Fatalf("cloud: %v %v", ps, err)
	}
}
