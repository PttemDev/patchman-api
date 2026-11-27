package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"

	"github.com/gin-gonic/gin"
)

func terminalGet(t *testing.T, h gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	h(c)
	return w
}

func TestTerminalStatusHostedReportsNotDesktop(t *testing.T) {
	w := terminalGet(t, NewTerminalHandler(false).Status)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d", w.Code)
	}
	var body struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Data["desktop"] != false {
		t.Fatalf("desktop = %v", body.Data["desktop"])
	}
	if _, ok := body.Data["shell"]; ok {
		t.Fatal("shell must be absent when hosted")
	}
}

func TestTerminalStatusEmbeddedReportsShellAndHome(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("HOME", tmp)
	w := terminalGet(t, NewTerminalHandler(true).Status)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d", w.Code)
	}
	var body struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Data["desktop"] != true || body.Data["shell"] != "sh" || body.Data["home"] != tmp {
		t.Fatalf("data = %v", body.Data)
	}
}

func TestTerminalShellRefusesWhenNotEmbedded(t *testing.T) {
	w := terminalGet(t, NewTerminalHandler(false).Shell)
	if w.Code != http.StatusForbidden {
		t.Fatalf("code = %d", w.Code)
	}
}

func TestTerminalCommandPlainShell(t *testing.T) {
	home := t.TempDir()
	cmd := terminalCommand("/bin/bash", home)
	if !reflect.DeepEqual(cmd.Args, []string{"/bin/bash", "-l"}) {
		t.Fatalf("args = %v", cmd.Args)
	}
	if cmd.Dir != home {
		t.Fatalf("dir = %q", cmd.Dir)
	}
	if want := append(os.Environ(), "TERM=xterm-256color"); !reflect.DeepEqual(cmd.Env, want) {
		t.Fatalf("env = %v", cmd.Env)
	}
}
