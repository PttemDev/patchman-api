package models

import "time"

// UserSession is one signed-in device for the hosted BE (DESKTOP_SECURITY_RFC
// L8). The session JWT carries its ID as the `sid` claim; revoking the row
// ends that device's access on its next request, whatever the token's exp.
type UserSession struct {
	ID            string     `gorm:"type:varchar(36);primaryKey" json:"id"`
	UserID        string     `gorm:"type:varchar(36);index;not null" json:"userId"`
	UserAgent     string     `gorm:"type:varchar(500)" json:"userAgent"`
	IPAddress     string     `gorm:"type:varchar(45)" json:"ipAddress"`
	ClientVersion string     `gorm:"type:varchar(40)" json:"clientVersion"`
	CreatedAt     time.Time  `json:"createdAt"`
	LastSeenAt    time.Time  `json:"lastSeenAt"`
	RevokedAt     *time.Time `gorm:"index" json:"revokedAt,omitempty"`
}

// SessionCutoff ends every pre-session token (no `sid` claim) a user was
// issued before RevokedBefore — such tokens have no row to revoke. Set by
// "sign out everywhere" / "sign out other devices".
type SessionCutoff struct {
	UserID        string    `gorm:"type:varchar(36);primaryKey" json:"userId"`
	RevokedBefore time.Time `json:"revokedBefore"`
}
