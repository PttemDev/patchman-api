package models

import "time"

// SavedResponse is a request/response pair someone kept from a request tab
// ("Save response"), filed under a collection (or folder). Everyone who can
// see the collection can see it — like Postman's examples — and re-open the
// request in a new tab with the saved parameters.
//
//   - RequestJSON is the tab's request as edited (method, URL, params,
//     headers, body, auth, scripts — {{variables}} unresolved) and is what a
//     replay opens.
//   - SentJSON is what actually went on the wire (variables resolved).
type SavedResponse struct {
	ID           string `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID       string `gorm:"type:varchar(36);index;not null" json:"userId"`
	CollectionID string `gorm:"type:varchar(36);index;not null" json:"collectionId"`
	// The saved request the tab was opened from, if any.
	RequestID       string    `gorm:"type:varchar(36);index" json:"requestId,omitempty"`
	Name            string    `gorm:"type:varchar(255);not null" json:"name"`
	Method          string    `gorm:"type:varchar(16);not null" json:"method"`
	URL             string    `gorm:"type:text;not null" json:"url"`
	RequestJSON     string    `gorm:"type:text" json:"-"`
	SentJSON        string    `gorm:"type:text" json:"-"`
	StatusCode      int       `json:"statusCode"`
	StatusText      string    `gorm:"type:varchar(120)" json:"statusText"`
	DurationMs      int64     `json:"durationMs"`
	ResponseSize    int64     `json:"responseSize"`
	ResponseHeaders string    `gorm:"type:text" json:"-"`
	ResponseBody    string    `gorm:"type:text" json:"-"`
	BodyBase64      bool      `json:"bodyBase64"`
	Truncated       bool      `json:"truncated"`
	CreatedAt       time.Time `json:"createdAt"`
}

func (SavedResponse) TableName() string { return "saved_responses" }
