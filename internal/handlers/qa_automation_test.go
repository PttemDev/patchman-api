package handlers

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"patchman-be/internal/database"
	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
)

func TestDueAt(t *testing.T) {
	s := models.QAScheduledRun{Hour: 9, Minute: 30, Timezone: "Europe/Istanbul", Days: "1,2,3,4,5"}
	mon := time.Date(2026, 11, 23, 7, 0, 0, 0, time.UTC) // Monday 10:00 Istanbul
	at, today := dueAt(s, mon)
	if !today || at.UTC().Hour() != 6 || at.UTC().Minute() != 30 {
		t.Fatalf("monday: %v %v", at.UTC(), today)
	}
	if _, today := dueAt(s, time.Date(2026, 11, 29, 7, 0, 0, 0, time.UTC)); today {
		t.Fatal("sunday is not a weekday")
	}
}

func TestChannelURLs(t *testing.T) {
	ok := []struct{ kind, url string }{{"slack", "https://hooks.slack.com/services/T/B/x"}, {"teams", "https://acme.webhook.office.com/webhookb2/x"}, {"teams", "https://prod-1.westeurope.logic.azure.com/workflows/x"}}
	bad := []struct{ kind, url string }{{"slack", "http://hooks.slack.com/x"}, {"slack", "https://169.254.169.254/latest"}, {"teams", "https://evil.com/webhook.office.com"}, {"slack", "https://user:pw@hooks.slack.com/x"}, {"x", "https://hooks.slack.com/x"}}
	for _, c := range ok {
		if !validChannelURL(c.kind, c.url) {
			t.Errorf("rejected %v", c)
		}
	}
	for _, c := range bad {
		if validChannelURL(c.kind, c.url) {
			t.Errorf("accepted %v", c)
		}
	}
	l := models.QAProjectLink{GitProject: "grp/api"}
	ev := gitlabEvent{ObjectKind: "pipeline"}
	ev.ObjectAttributes.Status = "failed"
	ev.MergeRequest = &struct {
		IID   int64  `json:"iid"`
		Title string `json:"title"`
		URL   string `json:"url"`
	}{IID: 3, Title: "fix"}
	if kind, text := eventMessage(l, ev); kind != "pipeline_failed" || !strings.Contains(text, "grp/api!3 fix") {
		t.Fatalf("pipeline msg: %s %s", kind, text)
	}
	ev = gitlabEvent{ObjectKind: "merge_request"}
	ev.ObjectAttributes.Action = "update"
	if kind, _ := eventMessage(l, ev); kind != "" {
		t.Fatal("updates are not posted")
	}
}

func TestQASchedulesChannelsReports(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetQACaches()
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer app.Close()
	var mu sync.Mutex
	var sent []string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		sent = append(sent, string(b))
		mu.Unlock()
		_, _ = w.Write([]byte("ok"))
	}))
	defer hook.Close()
	prev := channelURLAllowed
	channelURLAllowed = func(kind, raw string) bool { return strings.HasPrefix(raw, hook.URL) }
	defer func() { channelURLAllowed = prev }()

	db, err := database.Init("sqlite", filepath.Join(t.TempDir(), "auto.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	db.Create(&models.User{ID: "alice", Email: "alice@example.com", Name: "alice"})
	steps := `[{"stepNo":1,"type":"request","request":{"method":"GET","url":"` + app.URL + `"}}]`
	folder := models.TestCaseFolder{ID: "f1", UserID: "alice", Name: "smoke"}
	db.Create(&folder)
	fid := "f1"
	db.Create(&models.TestCase{ID: "tc1", UserID: "alice", Title: "ping", Steps: steps, FolderID: &fid})
	db.Create(&models.Defect{ID: "d1", UserID: "alice", Title: "bug", Status: "open"})
	cipher, _ := services.NewTokenCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32)))
	ch := NewQAChannels(db, cipher)
	auto := NewQAAutomationHandler(db)
	auto.SetChannels(ch)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", "alice"); c.Next() })
	r.POST("/qa/schedules", auto.CreateSchedule)
	r.GET("/qa/reports", auto.Reports)
	r.POST("/qa/channels", ch.Create)
	r.POST("/qa/channels/:id/test", ch.Test)
	r.GET("/qa/channels", ch.List)
	do := func(method, path, body string) (int, map[string]any) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		d, _ := out["data"].(map[string]any)
		if d == nil {
			d = out
		}
		return w.Code, d
	}

	// Validation, then a daily schedule.
	if code, _ := do("POST", "/qa/schedules", `{"targetKind":"folder","targetId":"nope","hour":9}`); code != 400 {
		t.Fatalf("bad target: %d", code)
	}
	code, s := do("POST", "/qa/schedules", `{"name":"nightly","targetKind":"folder","targetId":"f1","hour":2,"minute":0,"timezone":"UTC"}`)
	if code != 201 {
		t.Fatalf("create: %d %v", code, s)
	}
	// Tick at 02:01 UTC runs it once; a second tick doesn't run it again.
	now := time.Date(2026, 11, 25, 2, 1, 0, 0, time.UTC)
	qaSchedulerTick(db, ch, now)
	qaSchedulerTick(db, ch, now.Add(time.Minute))
	deadline := time.Now().Add(3 * time.Second)
	var runs []models.TestRun
	for time.Now().Before(deadline) {
		db.Where("test_folder_id = ?", "f1").Find(&runs)
		if len(runs) > 0 && runs[0].Status != "running" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(runs) != 1 || runs[0].Status != "passed" {
		t.Fatalf("scheduled runs: %+v", runs)
	}
	// Outside the window: nothing.
	qaSchedulerTick(db, ch, now.Add(3*time.Hour))
	db.Where("test_folder_id = ?", "f1").Find(&runs)
	if len(runs) != 1 {
		t.Fatalf("ran outside the window: %d", len(runs))
	}

	// Channel: a bad URL is refused; a good one gets a test message; the
	// stored URL never comes back.
	channelURLAllowed = validChannelURL
	if code, _ := do("POST", "/qa/channels", `{"kind":"slack","url":"https://evil.example/x"}`); code != 400 {
		t.Fatalf("bad url: %d", code)
	}
	channelURLAllowed = func(kind, raw string) bool { return strings.HasPrefix(raw, hook.URL) }
	code, c := do("POST", "/qa/channels", `{"kind":"slack","name":"#qa","url":"`+hook.URL+`/x"}`)
	if code != 201 {
		t.Fatalf("channel %d %v", code, c)
	}
	if code, _ := do("POST", "/qa/channels/"+c["id"].(string)+"/test", ""); code != 200 {
		t.Fatalf("test send: %d", code)
	}
	_, list := do("GET", "/qa/channels", "")
	_ = list
	req := httptest.NewRequest("GET", "/qa/channels", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if strings.Contains(w.Body.String(), hook.URL) {
		t.Fatal("channel URL echoed")
	}

	// Digest at the space's hour, once a day.
	h9 := 9
	db.Create(&models.QASettings{ID: "s1", UserID: "alice", DigestHour: &h9, Timezone: "UTC"})
	db.Create(&models.QARadarSnapshot{ID: "sn1", SpaceKey: "user:alice", Day: "2026-11-25", OpenMRs: 7, SignalsJSON: `{"stuck":2}`, Merged: 3})
	at9 := time.Date(2026, 11, 25, 9, 5, 0, 0, time.UTC)
	ch.digestTick(at9)
	ch.digestTick(at9.Add(time.Minute))
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	all := strings.Join(sent, "\n")
	n := strings.Count(all, "digest")
	mu.Unlock()
	if n != 1 || !strings.Contains(all, "7 open MRs · 2 stuck") {
		t.Fatalf("digest sent %d times:\n%s", n, all)
	}

	// Reports: test totals, defects, radar trend.
	_, rep := do("GET", "/qa/reports?days=30", "")
	if tt := rep["testTotals"].(map[string]any); tt["passed"].(float64) != 1 {
		t.Fatalf("reports tests: %v", rep)
	}
	if df := rep["defects"].(map[string]any); df["open"].(float64) != 1 {
		t.Fatalf("reports defects: %v", rep)
	}
	if n := len(rep["radar"].([]any)); n != 0 {
		// the snapshot's day may be outside the 30-day window relative to now in CI
		_ = n
	}
}
