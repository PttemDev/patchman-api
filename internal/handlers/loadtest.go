package handlers

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ---------------------------------------------------------------------------
// Constants & safety limits
// ---------------------------------------------------------------------------

const (
	maxVirtualUsers = 500
	maxDurationSec  = 300
	dbSaveInterval  = 2 * time.Second
	maxFeedRows     = 10000
)

// ---------------------------------------------------------------------------
// Config / Result types (JSON serialized into LoadTest.Config / .Results)
// ---------------------------------------------------------------------------

type loadTestTarget struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
}

type loadTestThresholds struct {
	MaxResponseTime int     `json:"maxResponseTime,omitempty"` // ms
	MaxErrorRate    float64 `json:"maxErrorRate,omitempty"`    // percentage
	P95ResponseTime int     `json:"p95ResponseTime,omitempty"` // ms
}

type loadTestParameters struct {
	VirtualUsers  int                `json:"virtualUsers"`
	Duration      int                `json:"duration"`               // seconds
	ThinkTime     int                `json:"thinkTime,omitempty"`    // ms between requests per VU (0 = no delay)
	RampUpTime    int                `json:"rampUpTime,omitempty"`   // seconds
	RampUpUsers   int                `json:"rampUpUsers,omitempty"`  // max users for ramp_up
	SpikeUsers    int                `json:"spikeUsers,omitempty"`   // spike peak
	SpikeDuration int                `json:"spikeDuration,omitempty"` // spike duration seconds
	Thresholds    loadTestThresholds `json:"thresholds,omitempty"`
}

type loadTestConfig struct {
	Profile    string             `json:"profile"` // constant, ramp_up, spike, soak, stress
	Target     loadTestTarget     `json:"target"`
	Parameters loadTestParameters `json:"parameters"`
	DataFeed   *loadTestDataFeed  `json:"dataFeed,omitempty"`
}

// loadTestDataFeed is an optional data-driven feed: each request iteration
// pulls the next row (per Strategy) and substitutes {{column}} tokens into the
// target URL, body, and header values. Rows are parsed FE-side from CSV/JSON.
type loadTestDataFeed struct {
	Columns  []string            `json:"columns"`  // ordered column names
	Rows     []map[string]string `json:"rows"`     // parsed rows
	Strategy string              `json:"strategy"` // circular | sequential | random | shuffle | once
}

// feedCursor is a thread-safe row dispenser shared across all VU goroutines.
// It is built once per run in runLoadTest when a data feed is present.
type feedCursor struct {
	rows     []map[string]string
	strategy string
	cursor   atomic.Int64
	shuffled []int // pre-shuffled index order for "shuffle"
}

func newFeedCursor(feed *loadTestDataFeed) *feedCursor {
	fc := &feedCursor{
		rows:     feed.Rows,
		strategy: feed.Strategy,
	}
	if feed.Strategy == "shuffle" && len(feed.Rows) > 0 {
		fc.shuffled = rand.Perm(len(feed.Rows))
	}
	return fc
}

// next returns the next row per the configured strategy. The bool is false when
// the feed is depleted (sequential/once) so the caller stops firing that VU.
func (fc *feedCursor) next() (map[string]string, bool) {
	n := len(fc.rows)
	if n == 0 {
		return nil, false
	}
	switch fc.strategy {
	case "sequential", "once":
		idx := fc.cursor.Add(1) - 1
		if idx >= int64(n) {
			return nil, false
		}
		return fc.rows[idx], true
	case "random":
		return fc.rows[rand.Intn(n)], true
	case "shuffle":
		idx := fc.cursor.Add(1) - 1
		return fc.rows[fc.shuffled[int(idx%int64(n))]], true
	default: // circular
		idx := fc.cursor.Add(1) - 1
		return fc.rows[idx%int64(n)], true
	}
}

// resolveFeedTarget returns a copy of target with the feed row's {{column}}
// tokens substituted into URL, Body, and each header value.
func resolveFeedTarget(target *loadTestTarget, row map[string]string) *loadTestTarget {
	resolved := *target
	resolved.URL = substituteVariables(target.URL, row)
	resolved.Body = substituteVariables(target.Body, row)
	if target.Headers != nil {
		resolved.Headers = make(map[string]string, len(target.Headers))
		for k, v := range target.Headers {
			resolved.Headers[k] = substituteVariables(v, row)
		}
	}
	return &resolved
}

type loadTestThresholdResult struct {
	Passed    bool    `json:"passed"`
	Threshold float64 `json:"threshold"`
	Actual    float64 `json:"actual"`
}

type loadTestErrorEntry struct {
	Message string `json:"message"`
	Count   int    `json:"count"`
}

type loadTestTimelineEntry struct {
	Second      int     `json:"second"`
	Requests    int     `json:"requests"`
	AvgTime     float64 `json:"avgTime"`
	Errors      int     `json:"errors"`
	ActiveUsers int     `json:"activeUsers"`
}

type loadTestResults struct {
	TotalRequests       int64                              `json:"totalRequests"`
	SuccessfulRequests  int64                              `json:"successfulRequests"`
	FailedRequests      int64                              `json:"failedRequests"`
	TotalDuration       int64                              `json:"totalDuration"`       // ms
	AvgResponseTime     float64                            `json:"avgResponseTime"`     // ms
	MinResponseTime     int64                              `json:"minResponseTime"`     // ms
	MaxResponseTime     int64                              `json:"maxResponseTime"`     // ms
	P50ResponseTime     int64                              `json:"p50ResponseTime"`     // ms
	P95ResponseTime     int64                              `json:"p95ResponseTime"`     // ms
	P99ResponseTime     int64                              `json:"p99ResponseTime"`     // ms
	RequestsPerSecond   float64                            `json:"requestsPerSecond"`
	ErrorRate           float64                            `json:"errorRate"`           // percentage
	ThroughputBytesPerSec float64                          `json:"throughputBytesPerSec"`
	StatusCodes         map[string]int                     `json:"statusCodes"`
	Errors              []loadTestErrorEntry               `json:"errors"`
	Timeline            []loadTestTimelineEntry            `json:"timeline"`
	ThresholdResults    map[string]loadTestThresholdResult `json:"thresholdResults,omitempty"`
}

// ---------------------------------------------------------------------------
// Internal types for the results collector
// ---------------------------------------------------------------------------

type requestSample struct {
	timestamp    time.Time
	duration     int64 // ms
	statusCode   int
	err          string
	responseSize int64
}

type resultsCollector struct {
	mu           sync.Mutex
	samples      []requestSample
	startTime    time.Time
	activeUsers  int64 // atomic

	vuHistoryMu  sync.Mutex
	vuHistory    map[int]int // second -> max active users during that second
}

func newResultsCollector(start time.Time) *resultsCollector {
	return &resultsCollector{
		samples:   make([]requestSample, 0, 10000),
		startTime: start,
		vuHistory: make(map[int]int),
	}
}

func (rc *resultsCollector) addSample(s requestSample) {
	rc.mu.Lock()
	rc.samples = append(rc.samples, s)
	rc.mu.Unlock()
}

func (rc *resultsCollector) setActiveUsers(n int64) {
	atomic.StoreInt64(&rc.activeUsers, n)
	// Record per-second history
	sec := int(time.Since(rc.startTime).Seconds()) + 1
	if sec < 1 {
		sec = 1
	}
	rc.vuHistoryMu.Lock()
	if int(n) > rc.vuHistory[sec] {
		rc.vuHistory[sec] = int(n)
	}
	rc.vuHistoryMu.Unlock()
}

func (rc *resultsCollector) getActiveUsers() int64 {
	return atomic.LoadInt64(&rc.activeUsers)
}

func (rc *resultsCollector) getVUHistory() map[int]int {
	rc.vuHistoryMu.Lock()
	defer rc.vuHistoryMu.Unlock()
	cp := make(map[int]int, len(rc.vuHistory))
	for k, v := range rc.vuHistory {
		cp[k] = v
	}
	return cp
}

// snapshot builds a loadTestResults from collected samples so far.
func (rc *resultsCollector) snapshot() loadTestResults {
	rc.mu.Lock()
	samplesCopy := make([]requestSample, len(rc.samples))
	copy(samplesCopy, rc.samples)
	rc.mu.Unlock()

	r := loadTestResults{
		StatusCodes: make(map[string]int),
		Errors:      []loadTestErrorEntry{},
		Timeline:    []loadTestTimelineEntry{},
	}

	if len(samplesCopy) == 0 {
		return r
	}

	var totalDurationMs int64
	var totalResponseTime int64
	var totalBytes int64
	var minRT int64 = math.MaxInt64
	var maxRT int64

	// Per-second buckets for timeline
	type secondBucket struct {
		requests    int
		totalTime   int64
		errors      int
		activeUsers int
	}
	buckets := make(map[int]*secondBucket)
	errorCounts := make(map[string]int)

	elapsed := time.Since(rc.startTime)
	if elapsed < time.Second {
		elapsed = time.Second
	}
	totalDurationMs = elapsed.Milliseconds()

	for _, s := range samplesCopy {
		r.TotalRequests++
		totalResponseTime += s.duration
		totalBytes += s.responseSize

		if s.duration < minRT {
			minRT = s.duration
		}
		if s.duration > maxRT {
			maxRT = s.duration
		}

		if s.err != "" {
			r.FailedRequests++
			errorCounts[s.err]++
		} else {
			r.SuccessfulRequests++
		}

		if s.statusCode > 0 {
			key := fmt.Sprintf("%d", s.statusCode)
			r.StatusCodes[key]++
		}

		// Timeline bucket
		sec := int(s.timestamp.Sub(rc.startTime).Seconds()) + 1
		if sec < 1 {
			sec = 1
		}
		b, ok := buckets[sec]
		if !ok {
			b = &secondBucket{}
			buckets[sec] = b
		}
		b.requests++
		b.totalTime += s.duration
		if s.err != "" {
			b.errors++
		}
	}

	// Get per-second VU history for accurate active user counts
	vuHistory := rc.getVUHistory()
	currentActive := int(rc.getActiveUsers())

	// Build sorted timeline
	maxSec := 0
	for sec := range buckets {
		if sec > maxSec {
			maxSec = sec
		}
	}
	// Also include seconds from vuHistory
	for sec := range vuHistory {
		if sec > maxSec {
			maxSec = sec
		}
	}
	for sec := 1; sec <= maxSec; sec++ {
		b := buckets[sec]
		avgTime := float64(0)
		reqs := 0
		errs := 0
		if b != nil {
			reqs = b.requests
			errs = b.errors
			if b.requests > 0 {
				avgTime = float64(b.totalTime) / float64(b.requests)
			}
		}
		au := vuHistory[sec]
		if au == 0 {
			au = currentActive
		}
		r.Timeline = append(r.Timeline, loadTestTimelineEntry{
			Second:      sec,
			Requests:    reqs,
			AvgTime:     math.Round(avgTime*100) / 100,
			Errors:      errs,
			ActiveUsers: au,
		})
	}

	// Error list
	for msg, cnt := range errorCounts {
		r.Errors = append(r.Errors, loadTestErrorEntry{Message: msg, Count: cnt})
	}
	sort.Slice(r.Errors, func(i, j int) bool {
		return r.Errors[i].Count > r.Errors[j].Count
	})

	// Aggregates
	r.TotalDuration = totalDurationMs
	if r.TotalRequests > 0 {
		r.AvgResponseTime = math.Round(float64(totalResponseTime)/float64(r.TotalRequests)*100) / 100
		r.ErrorRate = math.Round(float64(r.FailedRequests)/float64(r.TotalRequests)*10000) / 100
	}
	if minRT == math.MaxInt64 {
		minRT = 0
	}
	r.MinResponseTime = minRT
	r.MaxResponseTime = maxRT

	durationSec := float64(totalDurationMs) / 1000.0
	if durationSec > 0 {
		r.RequestsPerSecond = math.Round(float64(r.TotalRequests)/durationSec*100) / 100
		r.ThroughputBytesPerSec = math.Round(float64(totalBytes) / durationSec)
	}

	// Percentiles from sorted durations
	durations := make([]int64, len(samplesCopy))
	for i, s := range samplesCopy {
		durations[i] = s.duration
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	r.P50ResponseTime = percentile(durations, 50)
	r.P95ResponseTime = percentile(durations, 95)
	r.P99ResponseTime = percentile(durations, 99)

	return r
}

// percentile returns the p-th percentile from a sorted slice of int64 values.
// Uses the nearest-rank method.
func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[len(sorted)-1]
	}
	rank := (p / 100.0) * float64(len(sorted))
	idx := int(math.Ceil(rank)) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

type LoadTestHandler struct {
	db     *gorm.DB
	mu     sync.Mutex
	cancel map[string]context.CancelFunc
	live   map[string][]chan loadTestResults // SSE subscribers per test ID
	liveMu sync.Mutex
}

func NewLoadTestHandler(db *gorm.DB) *LoadTestHandler {
	return &LoadTestHandler{
		db:     db,
		cancel: make(map[string]context.CancelFunc),
		live:   make(map[string][]chan loadTestResults),
	}
}

// serializeLoadTest converts JSON string fields to parsed objects for the API response
func serializeLoadTest(lt models.LoadTest) gin.H {
	var config interface{}
	var results interface{}
	if err := json.Unmarshal([]byte(lt.Config), &config); err != nil {
		config = lt.Config
	}
	if lt.Results != "" && lt.Results != "{}" {
		if err := json.Unmarshal([]byte(lt.Results), &results); err != nil {
			results = nil
		}
	}
	return gin.H{
		"id":          lt.ID,
		"userId":      lt.UserID,
		"name":        lt.Name,
		"description": lt.Description,
		"status":      lt.Status,
		"config":      config,
		"results":     results,
		"startedAt":   lt.StartedAt,
		"completedAt": lt.CompletedAt,
		"createdAt":   lt.CreatedAt,
		"updatedAt":   lt.UpdatedAt,
	}
}

func serializeLoadTests(tests []models.LoadTest) []gin.H {
	out := make([]gin.H, len(tests))
	for i, t := range tests {
		out[i] = serializeLoadTest(t)
	}
	return out
}

// ---------------------------------------------------------------------------
// POST /api/load-tests  — Create and start a load test
// ---------------------------------------------------------------------------

type createLoadTestInput struct {
	Name        string          `json:"name" binding:"required"`
	Description string          `json:"description"`
	Config      json.RawMessage `json:"config" binding:"required"`
}

func (h *LoadTestHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")

	var input createLoadTestInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Parse and validate config
	var cfg loadTestConfig
	if err := json.Unmarshal(input.Config, &cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid config JSON: " + err.Error()})
		return
	}

	if err := validateConfig(&cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	configJSON, _ := json.Marshal(cfg)

	now := time.Now()
	lt := models.LoadTest{
		ID:          uuid.New().String(),
		UserID:      userID,
		Name:        input.Name,
		Description: input.Description,
		Status:      "running",
		Config:      string(configJSON),
		Results:     "{}",
		StartedAt:   &now,
	}

	if err := h.db.Create(&lt).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create load test"})
		return
	}

	// Set up cancellable context and start execution
	ctx, cancelFn := context.WithCancel(context.Background())
	h.mu.Lock()
	h.cancel[lt.ID] = cancelFn
	h.mu.Unlock()

	go h.runLoadTest(ctx, lt.ID, cfg)

	c.JSON(http.StatusCreated, gin.H{"data": serializeLoadTest(lt)})
}

// ---------------------------------------------------------------------------
// GET /api/load-tests  — List load tests for the user
// ---------------------------------------------------------------------------

func (h *LoadTestHandler) List(c *gin.Context) {
	userID := c.GetString("userID")

	var tests []models.LoadTest
	if err := h.db.Where("user_id = ?", userID).Order("created_at DESC").Find(&tests).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list load tests"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": serializeLoadTests(tests)})
}

// ---------------------------------------------------------------------------
// GET /api/load-tests/:id  — Get load test details
// ---------------------------------------------------------------------------

func (h *LoadTestHandler) Get(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var lt models.LoadTest
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&lt).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Load test not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": serializeLoadTest(lt)})
}

// ---------------------------------------------------------------------------
// GET /api/load-tests/:id/live  — SSE endpoint for live results
// ---------------------------------------------------------------------------

func (h *LoadTestHandler) LiveResults(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var lt models.LoadTest
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&lt).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Load test not found"})
		return
	}

	// Set SSE headers
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	// If test is already done, send the final results and close
	if lt.Status != "running" && lt.Status != "pending" {
		writeSSEEvent(c.Writer, "result", lt.Results)
		writeSSEEvent(c.Writer, "done", fmt.Sprintf(`{"status":"%s"}`, lt.Status))
		c.Writer.Flush()
		return
	}

	// Subscribe to live updates
	ch := make(chan loadTestResults, 16)
	h.liveMu.Lock()
	h.live[id] = append(h.live[id], ch)
	h.liveMu.Unlock()

	defer func() {
		h.liveMu.Lock()
		subs := h.live[id]
		for i, sub := range subs {
			if sub == ch {
				h.live[id] = append(subs[:i], subs[i+1:]...)
				break
			}
		}
		h.liveMu.Unlock()
		// Drain channel
		for range ch {
		}
	}()

	clientGone := c.Request.Context().Done()

	// Send initial event
	writeSSEEvent(c.Writer, "connected", fmt.Sprintf(`{"testId":"%s","status":"%s"}`, id, lt.Status))
	c.Writer.Flush()

	for {
		select {
		case <-clientGone:
			return
		case results, ok := <-ch:
			if !ok {
				// Channel closed — test is done
				// Re-read final status from DB
				var final models.LoadTest
				h.db.First(&final, "id = ?", id)
				writeSSEEvent(c.Writer, "done", fmt.Sprintf(`{"status":"%s"}`, final.Status))
				c.Writer.Flush()
				return
			}
			data, _ := json.Marshal(results)
			writeSSEEvent(c.Writer, "result", string(data))
			c.Writer.Flush()
		}
	}
}

// ---------------------------------------------------------------------------
// POST /api/load-tests/:id/cancel  — Cancel a running test
// ---------------------------------------------------------------------------

func (h *LoadTestHandler) Cancel(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var lt models.LoadTest
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&lt).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Load test not found"})
		return
	}

	if lt.Status != "running" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Load test is not running"})
		return
	}

	h.mu.Lock()
	cancelFn, ok := h.cancel[id]
	h.mu.Unlock()

	if ok {
		cancelFn()
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "Cancellation requested"}})
}

// ---------------------------------------------------------------------------
// DELETE /api/load-tests/:id  — Delete a load test
// ---------------------------------------------------------------------------

func (h *LoadTestHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var lt models.LoadTest
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&lt).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Load test not found"})
		return
	}

	// If running, cancel first
	if lt.Status == "running" {
		h.mu.Lock()
		cancelFn, ok := h.cancel[id]
		h.mu.Unlock()
		if ok {
			cancelFn()
		}
	}

	if err := h.db.Delete(&lt).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete load test"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "Load test deleted"}})
}

// ---------------------------------------------------------------------------
// Config validation
// ---------------------------------------------------------------------------

func validateConfig(cfg *loadTestConfig) error {
	validProfiles := map[string]bool{
		"constant": true,
		"ramp_up":  true,
		"spike":    true,
		"soak":     true,
		"stress":   true,
	}
	if !validProfiles[cfg.Profile] {
		return fmt.Errorf("invalid profile: %s (must be constant, ramp_up, spike, soak, or stress)", cfg.Profile)
	}

	if cfg.Target.URL == "" {
		return fmt.Errorf("target URL is required")
	}
	if cfg.Target.Method == "" {
		cfg.Target.Method = "GET"
	}
	cfg.Target.Method = strings.ToUpper(cfg.Target.Method)

	p := &cfg.Parameters
	if p.VirtualUsers < 1 {
		p.VirtualUsers = 1
	}
	if p.VirtualUsers > maxVirtualUsers {
		return fmt.Errorf("virtualUsers cannot exceed %d", maxVirtualUsers)
	}
	if p.Duration < 1 {
		p.Duration = 10
	}
	if p.Duration > maxDurationSec {
		return fmt.Errorf("duration cannot exceed %d seconds", maxDurationSec)
	}

	switch cfg.Profile {
	case "ramp_up":
		if p.RampUpTime < 1 {
			p.RampUpTime = p.Duration / 2
			if p.RampUpTime < 1 {
				p.RampUpTime = 1
			}
		}
		if p.RampUpUsers < 1 {
			p.RampUpUsers = p.VirtualUsers
		}
		if p.RampUpUsers > maxVirtualUsers {
			return fmt.Errorf("rampUpUsers cannot exceed %d", maxVirtualUsers)
		}
	case "spike":
		if p.SpikeUsers < 1 {
			p.SpikeUsers = p.VirtualUsers * 5
		}
		if p.SpikeUsers > maxVirtualUsers {
			return fmt.Errorf("spikeUsers cannot exceed %d", maxVirtualUsers)
		}
		if p.SpikeDuration < 1 {
			p.SpikeDuration = 5
		}
	case "stress":
		// Stress starts at 1 and ramps until failure or duration
		// virtualUsers is the ceiling
		if p.VirtualUsers < 2 {
			p.VirtualUsers = maxVirtualUsers
		}
	}

	if cfg.DataFeed != nil {
		if len(cfg.DataFeed.Rows) == 0 {
			return fmt.Errorf("data feed requires at least one row")
		}
		if len(cfg.DataFeed.Rows) > maxFeedRows {
			return fmt.Errorf("data feed cannot exceed %d rows", maxFeedRows)
		}
		if cfg.DataFeed.Strategy == "" {
			cfg.DataFeed.Strategy = "circular"
		}
		validStrategies := map[string]bool{
			"circular":   true,
			"sequential": true,
			"random":     true,
			"shuffle":    true,
			"once":       true,
		}
		if !validStrategies[cfg.DataFeed.Strategy] {
			return fmt.Errorf("invalid data feed strategy: %s (must be circular, sequential, random, shuffle, or once)", cfg.DataFeed.Strategy)
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// Execution engine
// ---------------------------------------------------------------------------

func (h *LoadTestHandler) runLoadTest(ctx context.Context, testID string, cfg loadTestConfig) {
	collector := newResultsCollector(time.Now())
	httpClient := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
			MaxIdleConnsPerHost: maxVirtualUsers,
			MaxIdleConns:        maxVirtualUsers,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true,
		},
	}

	// WaitGroup for all VU goroutines
	var wg sync.WaitGroup

	// Channel to signal VUs to stop
	done := make(chan struct{})

	// Track active VU count for dynamic scaling
	var activeVUs int64

	thinkTime := time.Duration(cfg.Parameters.ThinkTime) * time.Millisecond

	// Build the shared data-feed cursor once (nil when no feed is configured).
	var cursor *feedCursor
	if cfg.DataFeed != nil {
		cursor = newFeedCursor(cfg.DataFeed)
	}

	// Start a VU goroutine. It runs requests in a loop until done or ctx cancelled.
	startVU := func() {
		wg.Add(1)
		atomic.AddInt64(&activeVUs, 1)
		collector.setActiveUsers(atomic.LoadInt64(&activeVUs))
		go func() {
			defer wg.Done()
			defer func() {
				atomic.AddInt64(&activeVUs, -1)
				collector.setActiveUsers(atomic.LoadInt64(&activeVUs))
			}()
			for {
				select {
				case <-done:
					return
				case <-ctx.Done():
					return
				default:
					target := &cfg.Target
					if cursor != nil {
						row, ok := cursor.next()
						if !ok {
							// Feed depleted (sequential/once) — stop this VU.
							return
						}
						target = resolveFeedTarget(&cfg.Target, row)
					}
					sample := executeLoadRequest(ctx, httpClient, target)
					if sample != nil {
						collector.addSample(*sample)
					}
					// Think time between requests
					if thinkTime > 0 {
						select {
						case <-done:
							return
						case <-ctx.Done():
							return
						case <-time.After(thinkTime):
						}
					} else {
						select {
						case <-done:
							return
						case <-ctx.Done():
							return
						default:
						}
					}
				}
			}
		}()
	}

	// Stop N VUs by signalling (we use a separate stopper channel per VU for ramp-down)
	// For simplicity with spike profile, we use a slice of per-VU stop channels.
	type vuHandle struct {
		stop chan struct{}
	}
	var vuHandles []vuHandle
	var vuHandlesMu sync.Mutex

	startTrackedVU := func() {
		wg.Add(1)
		atomic.AddInt64(&activeVUs, 1)
		collector.setActiveUsers(atomic.LoadInt64(&activeVUs))
		stopCh := make(chan struct{})
		vuHandlesMu.Lock()
		vuHandles = append(vuHandles, vuHandle{stop: stopCh})
		vuHandlesMu.Unlock()
		go func() {
			defer wg.Done()
			defer func() {
				atomic.AddInt64(&activeVUs, -1)
				collector.setActiveUsers(atomic.LoadInt64(&activeVUs))
			}()
			for {
				select {
				case <-done:
					return
				case <-ctx.Done():
					return
				case <-stopCh:
					return
				default:
					target := &cfg.Target
					if cursor != nil {
						row, ok := cursor.next()
						if !ok {
							// Feed depleted (sequential/once) — stop this VU.
							return
						}
						target = resolveFeedTarget(&cfg.Target, row)
					}
					sample := executeLoadRequest(ctx, httpClient, target)
					if sample != nil {
						collector.addSample(*sample)
					}
					// Think time between requests
					if thinkTime > 0 {
						select {
						case <-done:
							return
						case <-ctx.Done():
							return
						case <-stopCh:
							return
						case <-time.After(thinkTime):
						}
					} else {
						select {
						case <-done:
							return
						case <-ctx.Done():
							return
						case <-stopCh:
							return
						default:
						}
					}
				}
			}
		}()
	}

	stopTrackedVUs := func(n int) {
		vuHandlesMu.Lock()
		defer vuHandlesMu.Unlock()
		toStop := n
		if toStop > len(vuHandles) {
			toStop = len(vuHandles)
		}
		for i := 0; i < toStop; i++ {
			close(vuHandles[i].stop)
		}
		vuHandles = vuHandles[toStop:]
	}

	// Periodic DB save and SSE broadcast
	saveTicker := time.NewTicker(dbSaveInterval)
	defer saveTicker.Stop()

	saveResults := func() {
		snap := collector.snapshot()
		data, _ := json.Marshal(snap)
		h.db.Model(&models.LoadTest{}).Where("id = ?", testID).Update("results", string(data))
		// Broadcast to SSE subscribers
		h.liveMu.Lock()
		subs := h.live[testID]
		for _, ch := range subs {
			select {
			case ch <- snap:
			default:
				// Subscriber too slow, skip
			}
		}
		h.liveMu.Unlock()
	}

	// Periodic VU history recorder (1s granularity for accurate charts)
	vuTicker := time.NewTicker(1 * time.Second)

	// Run the periodic saver in background
	saveCtx, saveCancel := context.WithCancel(context.Background())
	var saveWg sync.WaitGroup
	saveWg.Add(1)
	go func() {
		defer saveWg.Done()
		defer vuTicker.Stop()
		for {
			select {
			case <-saveCtx.Done():
				return
			case <-saveTicker.C:
				saveResults()
			case <-vuTicker.C:
				// Record current active users for this second
				collector.setActiveUsers(atomic.LoadInt64(&activeVUs))
			}
		}
	}()

	// Profile-specific orchestration
	totalDuration := time.Duration(cfg.Parameters.Duration) * time.Second

	switch cfg.Profile {
	case "constant", "soak":
		// Spawn all VUs immediately, run for duration
		for i := 0; i < cfg.Parameters.VirtualUsers; i++ {
			startVU()
		}
		select {
		case <-time.After(totalDuration):
		case <-ctx.Done():
		}

	case "ramp_up":
		// Linearly increase VUs from 0 to rampUpUsers over rampUpTime,
		// then hold until duration expires.
		rampUpTime := time.Duration(cfg.Parameters.RampUpTime) * time.Second
		targetUsers := cfg.Parameters.RampUpUsers
		if targetUsers < 1 {
			targetUsers = cfg.Parameters.VirtualUsers
		}

		rampInterval := rampUpTime / time.Duration(targetUsers)
		if rampInterval < 50*time.Millisecond {
			rampInterval = 50 * time.Millisecond
		}

		rampTicker := time.NewTicker(rampInterval)
		launched := 0
		rampDone := false

		timer := time.NewTimer(totalDuration)
	rampLoop:
		for {
			select {
			case <-ctx.Done():
				break rampLoop
			case <-timer.C:
				break rampLoop
			case <-rampTicker.C:
				if !rampDone && launched < targetUsers {
					startVU()
					launched++
					if launched >= targetUsers {
						rampDone = true
						rampTicker.Stop()
					}
				}
			}
		}
		timer.Stop()
		rampTicker.Stop()

	case "spike":
		// Phase 1: Normal load for (duration - spikeDuration) / 2
		// Phase 2: Spike to spikeUsers for spikeDuration
		// Phase 3: Back to normal for remaining time
		normalUsers := cfg.Parameters.VirtualUsers
		spikeUsers := cfg.Parameters.SpikeUsers
		spikeDur := time.Duration(cfg.Parameters.SpikeDuration) * time.Second

		// Calculate phase durations
		remaining := totalDuration - spikeDur
		if remaining < 2*time.Second {
			remaining = 2 * time.Second
		}
		phase1Dur := remaining / 2
		phase3Dur := remaining - phase1Dur

		// Phase 1: Normal load
		for i := 0; i < normalUsers; i++ {
			startTrackedVU()
		}
		select {
		case <-time.After(phase1Dur):
		case <-ctx.Done():
			goto spikeCleanup
		}

		// Phase 2: Spike — add extra VUs
		{
			extra := spikeUsers - normalUsers
			if extra > 0 {
				for i := 0; i < extra; i++ {
					startTrackedVU()
				}
			}
		}
		select {
		case <-time.After(spikeDur):
		case <-ctx.Done():
			goto spikeCleanup
		}

		// Phase 3: Ramp back down to normal
		{
			extra := spikeUsers - normalUsers
			if extra > 0 {
				stopTrackedVUs(extra)
			}
		}
		select {
		case <-time.After(phase3Dur):
		case <-ctx.Done():
		}

	spikeCleanup:
		// Stop remaining tracked VUs
		stopTrackedVUs(int(atomic.LoadInt64(&activeVUs)) + maxVirtualUsers)

	case "stress":
		// Start at 1 VU, add 1 every 2 seconds.
		// Stop when error rate exceeds threshold or VU limit or duration reached.
		maxUsers := cfg.Parameters.VirtualUsers
		errorThreshold := cfg.Parameters.Thresholds.MaxErrorRate
		if errorThreshold <= 0 {
			errorThreshold = 50.0 // Default: stop at 50% error rate
		}

		startVU()
		launched := 1

		addTicker := time.NewTicker(2 * time.Second)
		timer := time.NewTimer(totalDuration)
		checkTicker := time.NewTicker(1 * time.Second)

	stressLoop:
		for {
			select {
			case <-ctx.Done():
				break stressLoop
			case <-timer.C:
				break stressLoop
			case <-addTicker.C:
				if launched < maxUsers {
					startVU()
					launched++
				}
			case <-checkTicker.C:
				snap := collector.snapshot()
				if snap.TotalRequests > 10 && snap.ErrorRate > errorThreshold {
					break stressLoop
				}
			}
		}
		timer.Stop()
		addTicker.Stop()
		checkTicker.Stop()
	}

	// Signal all VUs to stop
	close(done)
	wg.Wait()

	// Stop the periodic saver
	saveCancel()
	saveWg.Wait()

	// Final results
	finalSnap := collector.snapshot()

	// Evaluate thresholds
	thresholds := cfg.Parameters.Thresholds
	thresholdResults := make(map[string]loadTestThresholdResult)

	if thresholds.MaxResponseTime > 0 {
		thresholdResults["maxResponseTime"] = loadTestThresholdResult{
			Passed:    finalSnap.MaxResponseTime <= int64(thresholds.MaxResponseTime),
			Threshold: float64(thresholds.MaxResponseTime),
			Actual:    float64(finalSnap.MaxResponseTime),
		}
	}
	if thresholds.MaxErrorRate > 0 {
		thresholdResults["maxErrorRate"] = loadTestThresholdResult{
			Passed:    finalSnap.ErrorRate <= thresholds.MaxErrorRate,
			Threshold: thresholds.MaxErrorRate,
			Actual:    finalSnap.ErrorRate,
		}
	}
	if thresholds.P95ResponseTime > 0 {
		thresholdResults["p95ResponseTime"] = loadTestThresholdResult{
			Passed:    finalSnap.P95ResponseTime <= int64(thresholds.P95ResponseTime),
			Threshold: float64(thresholds.P95ResponseTime),
			Actual:    float64(finalSnap.P95ResponseTime),
		}
	}

	if len(thresholdResults) > 0 {
		finalSnap.ThresholdResults = thresholdResults
	}

	// Determine final status
	finalStatus := "completed"
	select {
	case <-ctx.Done():
		finalStatus = "cancelled"
	default:
	}

	// Check if any threshold failed — mark as failed
	if finalStatus == "completed" {
		for _, tr := range thresholdResults {
			if !tr.Passed {
				finalStatus = "failed"
				break
			}
		}
	}

	// Persist final results
	now := time.Now()
	resultsJSON, _ := json.Marshal(finalSnap)
	h.db.Model(&models.LoadTest{}).Where("id = ?", testID).Updates(map[string]interface{}{
		"status":       finalStatus,
		"results":      string(resultsJSON),
		"completed_at": &now,
	})

	// Close SSE subscriber channels
	h.liveMu.Lock()
	subs := h.live[testID]
	for _, ch := range subs {
		close(ch)
	}
	delete(h.live, testID)
	h.liveMu.Unlock()

	// Clean up cancel function
	h.mu.Lock()
	delete(h.cancel, testID)
	h.mu.Unlock()
}

// executeLoadRequest sends a single HTTP request and returns a sample.
func executeLoadRequest(ctx context.Context, client *http.Client, target *loadTestTarget) *requestSample {
	start := time.Now()

	var bodyReader io.Reader
	if target.Body != "" {
		bodyReader = strings.NewReader(target.Body)
	}

	req, err := http.NewRequestWithContext(ctx, target.Method, target.URL, bodyReader)
	if err != nil {
		// Context cancelled
		if ctx.Err() != nil {
			return nil
		}
		return &requestSample{
			timestamp: start,
			duration:  time.Since(start).Milliseconds(),
			err:       err.Error(),
		}
	}

	for k, v := range target.Headers {
		req.Header.Set(k, v)
	}

	if req.Header.Get("Accept-Encoding") == "" {
		req.Header.Set("Accept-Encoding", services.DefaultAcceptEncoding)
	}

	// Auto-set Content-Type for JSON bodies
	if target.Body != "" && req.Header.Get("Content-Type") == "" {
		trimmed := strings.TrimSpace(target.Body)
		if (strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}")) ||
			(strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]")) {
			req.Header.Set("Content-Type", "application/json")
		}
	}

	resp, err := client.Do(req)
	duration := time.Since(start).Milliseconds()

	if err != nil {
		// Context cancelled — don't record as error
		if ctx.Err() != nil {
			return nil
		}
		// Simplify error message for aggregation
		errMsg := simplifyError(err.Error())
		return &requestSample{
			timestamp: start,
			duration:  duration,
			err:       errMsg,
		}
	}
	defer resp.Body.Close()

	// Decode body (gzip/deflate/br/zstd) so responseSize reflects the
	// real payload size and downstream consumers see plain text.
	body, _ := services.DecodeResponseBody(resp, 0)
	responseSize := int64(len(body))

	sample := requestSample{
		timestamp:    start,
		duration:     duration,
		statusCode:   resp.StatusCode,
		responseSize: responseSize,
	}

	// Treat 5xx as errors for error rate tracking
	if resp.StatusCode >= 500 {
		sample.err = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}

	return &sample
}

// simplifyError reduces verbose Go error messages into shorter aggregatable strings.
func simplifyError(msg string) string {
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "connection refused"):
		return "connection refused"
	case strings.Contains(lower, "timeout"):
		return "request timeout"
	case strings.Contains(lower, "no such host"):
		return "DNS resolution failed"
	case strings.Contains(lower, "tls"):
		return "TLS error"
	case strings.Contains(lower, "eof"):
		return "connection closed (EOF)"
	case strings.Contains(lower, "connection reset"):
		return "connection reset"
	case strings.Contains(lower, "too many open files"):
		return "too many open files"
	default:
		// Truncate long errors
		if len(msg) > 80 {
			return msg[:80] + "..."
		}
		return msg
	}
}
