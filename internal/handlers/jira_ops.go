package handlers

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"patchman-be/internal/jira"
	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Jira read surface: fields, JQL search, one issue. Any access may read —
// PatchMan never changes Jira in P0 (invariant 4). Search and issue reads
// are audited (who read what through whose connection); the field list is
// not.

func (h *JiraConnectionHandler) access(c *gin.Context) (*models.JiraConnection, string, bool) {
	conn, role, err := h.loadAccessible(c.Param("id"), c.GetString("userID"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return nil, "", false
	}
	return conn, role, true
}

// jiraFail answers a Jira failure. Auth problems on the Jira side map to
// 502 (never 401/403, which the FE treats as its own session failing).
func jiraFail(c *gin.Context, err error) {
	if ae, ok := jira.AsAPIError(err); ok {
		status := http.StatusBadGateway
		switch ae.Code {
		case "bad_request", "bad_query":
			status = http.StatusBadRequest
		case "not_found":
			status = http.StatusNotFound
		case "unreachable":
			status = http.StatusGatewayTimeout
		case "rate_limited":
			status = http.StatusTooManyRequests
		}
		c.JSON(status, gin.H{"error": ae.Message, "code": ae.Code})
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "Jira did not answer in time", "code": "unreachable"})
		return
	}
	c.JSON(http.StatusBadGateway, gin.H{"error": "Jira request failed"})
}

func (h *JiraConnectionHandler) audit(c *gin.Context, conn *models.JiraConnection, role, action, target string, actErr error) {
	var delegated *string
	if role != "owner" {
		owner := conn.UserID
		delegated = &owner
	}
	errText := ""
	if actErr != nil {
		errText = actErr.Error()
	}
	if len(target) > 1024 {
		target = target[:1024]
	}
	row := models.JiraActivityHistory{
		ID: uuid.New().String(), UserID: c.GetString("userID"), DelegatedOwner: delegated,
		ConnectionID: conn.ID, Action: action, Target: target, ErrorText: errText, ExecutedAt: time.Now(),
	}
	go func() { _ = h.db.Create(&row).Error }()
}

// Fields lists every field the connection's Jira user can see (the AC
// field picker filters to custom ones). GET /jira-connections/:id/fields
func (h *JiraConnectionHandler) Fields(c *gin.Context) {
	conn, _, ok := h.access(c)
	if !ok {
		return
	}
	cl, err := h.client(*conn)
	if err != nil {
		jiraFail(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	fields, err := cl.Fields(ctx)
	if err != nil {
		jiraFail(c, err)
		return
	}
	if fields == nil {
		fields = []jira.Field{}
	}
	c.JSON(http.StatusOK, gin.H{"data": fields})
}

// Search runs one page of a JQL search.
// GET /jira-connections/:id/search?jql=&next=&limit=  (jql defaults to the
// connection's defaultJql). next is the cursor the previous page returned.
func (h *JiraConnectionHandler) Search(c *gin.Context) {
	conn, role, ok := h.access(c)
	if !ok {
		return
	}
	jql := strings.TrimSpace(c.Query("jql"))
	if jql == "" {
		jql = jiraOpt(*conn, "defaultJql")
	}
	if jql == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "jql is required (this connection has no default JQL)", "code": "bad_request"})
		return
	}
	if len(jql) > 4000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "jql is too long (max 4000 characters)", "code": "bad_request"})
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	cl, err := h.client(*conn)
	if err != nil {
		jiraFail(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 45*time.Second)
	defer cancel()
	res, err := cl.Search(ctx, jira.SearchRequest{JQL: jql, Limit: limit, Next: c.Query("next")})
	h.audit(c, conn, role, "search", jql, err)
	if err != nil {
		jiraFail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"jql": jql, "issues": res.Issues, "total": res.Total, "next": res.Next, "isLast": res.IsLast,
		"flavor": cl.Flavor(), "baseUrl": cl.Base(),
	}})
}

// Issue reads one issue with the connection's acceptance-criteria field,
// status, people, labels, sprint, links and subtasks.
// GET /jira-connections/:id/issue/:key
func (h *JiraConnectionHandler) Issue(c *gin.Context) {
	conn, role, ok := h.access(c)
	if !ok {
		return
	}
	key := strings.ToUpper(strings.TrimSpace(c.Param("key")))
	if !jira.ValidKey(key) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid issue key (expected e.g. ABC-123)", "code": "bad_request"})
		return
	}
	cl, err := h.client(*conn)
	if err != nil {
		jiraFail(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	opt := jira.IssueOptions{AcceptanceFieldID: jiraOpt(*conn, "acFieldId"), SprintFieldID: h.sprintField(ctx, cl, *conn)}
	is, err := cl.Issue(ctx, key, opt)
	h.audit(c, conn, role, "issue", key, err)
	if err != nil {
		jiraFail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": is})
}

// auditRadar records one radar lookup (one row per connection per radar
// compute, not per issue).
func (h *JiraConnectionHandler) auditRadar(conn *models.JiraConnection, userID, role string, keys int) {
	var delegated *string
	if role != "owner" {
		owner := conn.UserID
		delegated = &owner
	}
	row := models.JiraActivityHistory{
		ID: uuid.New().String(), UserID: userID, DelegatedOwner: delegated,
		ConnectionID: conn.ID, Action: "qa_radar", Target: strconv.Itoa(keys) + " keys", ExecutedAt: time.Now(),
	}
	go func() { _ = h.db.Create(&row).Error }()
}

// auditWrite records a Jira write made from the CR + QA Hub.
func (h *JiraConnectionHandler) auditWrite(conn *models.JiraConnection, userID, role, action, target string) {
	var delegated *string
	if role != "owner" {
		owner := conn.UserID
		delegated = &owner
	}
	row := models.JiraActivityHistory{
		ID: uuid.New().String(), UserID: userID, DelegatedOwner: delegated,
		ConnectionID: conn.ID, Action: action, Target: target, ExecutedAt: time.Now(),
	}
	go func() { _ = h.db.Create(&row).Error }()
}

// Projects lists the projects the connection can see.
// GET /jira-connections/:id/projects?q=
func (h *JiraConnectionHandler) Projects(c *gin.Context) {
	conn, _, ok := h.access(c)
	if !ok {
		return
	}
	cl, err := h.client(*conn)
	if err != nil {
		jiraFail(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	ps, err := cl.Projects(ctx, c.Query("q"))
	if err != nil {
		jiraFail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": ps})
}
