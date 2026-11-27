package models

import "time"

// MRReview is one AI review of an MR at one head SHA (CR_QA_RFC P1). The
// review is written by a person's own agent over MCP (the server never
// calls an LLM); findings reach GitLab / GitHub only when a person approves
// and posts them with their own credentials. Unique per (link, iid, SHA).
type MRReview struct {
	ID              string  `gorm:"type:varchar(36);primaryKey" json:"id"`
	WorkspaceID     *string `gorm:"type:varchar(36);index" json:"workspaceId,omitempty"`
	LinkID          string  `gorm:"type:varchar(36);uniqueIndex:idx_mr_review_sha;not null" json:"linkId"`
	GitConnectionID string  `gorm:"type:varchar(36)" json:"gitConnectionId"`
	Provider        string  `gorm:"type:varchar(20)" json:"provider"`
	Project         string  `gorm:"type:varchar(500)" json:"project"`
	IID             int64   `gorm:"column:iid;uniqueIndex:idx_mr_review_sha;not null" json:"iid"`
	HeadSHA         string  `gorm:"type:varchar(64);uniqueIndex:idx_mr_review_sha;not null" json:"headSha"`
	Title           string  `gorm:"type:varchar(500)" json:"title"`
	WebURL          string  `gorm:"type:text" json:"webUrl"`
	// Status: claimed | draft | posted | discarded | stale.
	Status    string `gorm:"type:varchar(20);index;not null" json:"status"`
	ClaimedBy string `gorm:"type:varchar(36);index" json:"claimedBy"`
	Agent     string `gorm:"type:varchar(60)" json:"agent"`
	Model     string `gorm:"type:varchar(100)" json:"model"`
	Summary   string `gorm:"type:text" json:"summary"`
	// FindingsJSON: []ReviewFinding.
	FindingsJSON string `gorm:"type:text" json:"-"`
	// MR people at context time (O3 approvers): author + reviewers usernames.
	AuthorUsername    string     `gorm:"type:varchar(200)" json:"authorUsername"`
	ReviewerUsernames string     `gorm:"type:text" json:"-"` // comma-separated
	SummaryNoteID     string     `gorm:"type:varchar(100)" json:"summaryNoteId,omitempty"`
	PostedAt          *time.Time `json:"postedAt,omitempty"`
	PostedBy          string     `gorm:"type:varchar(36)" json:"postedBy,omitempty"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
}

func (r MRReview) QAOwner() (string, *string) { return r.ClaimedBy, r.WorkspaceID }

// ReviewFinding is one finding of an MRReview.
type ReviewFinding struct {
	ID         string `json:"id"`
	Severity   string `json:"severity"` // blocker | major | minor | nit
	File       string `json:"file,omitempty"`
	Line       int    `json:"line,omitempty"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	Suggestion string `json:"suggestion,omitempty"`
	// Status: pending | approved | rejected | posted.
	Status    string `json:"status"`
	NoteID    string `json:"noteId,omitempty"`
	Inline    bool   `json:"inline,omitempty"` // posted on the diff line
	DecidedBy string `json:"decidedBy,omitempty"`
	PostError string `json:"postError,omitempty"`
	// Fix verification (P4.a): the latest verdict on a posted finding —
	// fixed | not_fixed | partly | obsolete — at commit VerifiedSHA.
	Verdict     string `json:"verdict,omitempty"`
	VerifiedSHA string `json:"verifiedSha,omitempty"`
	// Resolved: the GitLab thread was resolved from PatchMan after a verified fix.
	Resolved bool `json:"resolved,omitempty"`
}

// FindingOpen: a posted finding still waiting for a fix to be verified.
func (f ReviewFinding) FindingOpen() bool {
	return f.Status == "posted" && !f.Resolved && f.Verdict != "fixed" && f.Verdict != "obsolete"
}

// MRReviewAudit records who sent which MR to which agent / model and who
// posted what (CR_QA_RFC expert panel: security).
type MRReviewAudit struct {
	ID       string    `gorm:"type:varchar(36);primaryKey" json:"id"`
	ReviewID string    `gorm:"type:varchar(36);index" json:"reviewId,omitempty"`
	UserID   string    `gorm:"type:varchar(36);index;not null" json:"userId"`
	LinkID   string    `gorm:"type:varchar(36);index" json:"linkId"`
	IID      int64     `gorm:"column:iid" json:"iid"`
	HeadSHA  string    `gorm:"type:varchar(64)" json:"headSha"`
	Action   string    `gorm:"type:varchar(30);not null" json:"action"` // context | claim | submit | decide | post
	Agent    string    `gorm:"type:varchar(60)" json:"agent,omitempty"`
	Model    string    `gorm:"type:varchar(100)" json:"model,omitempty"`
	Detail   string    `gorm:"type:text" json:"detail,omitempty"`
	At       time.Time `gorm:"index" json:"at"`
}

// MRVerification is one fix check (P4.a): a person's agent compares the
// posted findings of an MRReview with the changes since that review and
// gives a verdict per finding. Unique per (review, head SHA). Replies and
// thread resolution reach GitLab only on a person's click.
type MRVerification struct {
	ID          string  `gorm:"type:varchar(36);primaryKey" json:"id"`
	WorkspaceID *string `gorm:"type:varchar(36);index" json:"workspaceId,omitempty"`
	LinkID      string  `gorm:"type:varchar(36);index;not null" json:"linkId"`
	IID         int64   `gorm:"column:iid;not null" json:"iid"`
	ReviewID    string  `gorm:"type:varchar(36);uniqueIndex:idx_mr_verify_sha;not null" json:"reviewId"`
	FromSHA     string  `gorm:"type:varchar(64)" json:"fromSha"`
	HeadSHA     string  `gorm:"type:varchar(64);uniqueIndex:idx_mr_verify_sha;not null" json:"headSha"`
	// Status: claimed | submitted | applied.
	Status    string `gorm:"type:varchar(20);not null" json:"status"`
	ClaimedBy string `gorm:"type:varchar(36);index" json:"claimedBy"`
	Agent     string `gorm:"type:varchar(60)" json:"agent"`
	Model     string `gorm:"type:varchar(100)" json:"model"`
	Summary   string `gorm:"type:text" json:"summary"`
	// VerdictsJSON: []FindingVerdict.
	VerdictsJSON string    `gorm:"type:text" json:"-"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

func (v MRVerification) QAOwner() (string, *string) { return v.ClaimedBy, v.WorkspaceID }

// FindingVerdict is the agent's verdict on one finding.
type FindingVerdict struct {
	FindingID string `json:"findingId"`
	Verdict   string `json:"verdict"` // fixed | not_fixed | partly | obsolete
	Evidence  string `json:"evidence"`
	// Applied: resolved | replied | "" (what a person sent to GitLab).
	Applied    string `json:"applied,omitempty"`
	ApplyError string `json:"applyError,omitempty"`
}
