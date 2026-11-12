package models

import "time"

// SystemSetting holds singleton, app-wide configuration that admins toggle
// at runtime. It is implemented as a single-row table (ID always 1) so the
// CRUD becomes a flat upsert and we don't have to reason about partial
// state. Add new toggles as columns; never write a second row.
type SystemSetting struct {
	ID uint `gorm:"primaryKey" json:"id"`

	// Crash reporting → Slack toggles. Each one independently controls
	// whether the corresponding FE error source is forwarded to Slack:
	//   - Boundary: React render-time crashes (caught by ErrorBoundary)
	//   - Rejection: window.onunhandledrejection (a Promise without .catch)
	//   - WindowError: window.onerror (sync error in an event handler)
	//
	// Default: only the boundary path is on. Rejection and window.error
	// can be noisy (browser extensions, third-party scripts), so admins
	// opt in deliberately.
	ErrorReportingBoundaryEnabled   bool `gorm:"not null;default:true" json:"errorReportingBoundaryEnabled"`
	ErrorReportingRejectionEnabled  bool `gorm:"not null;default:false" json:"errorReportingRejectionEnabled"`
	ErrorReportingWindowEnabled     bool `gorm:"not null;default:false" json:"errorReportingWindowEnabled"`

	UpdatedAt time.Time `json:"updatedAt"`
}

// SystemSettingSingletonID is the fixed primary key for the single row.
// Every read/write goes through this constant — never insert a row with a
// different ID.
const SystemSettingSingletonID uint = 1
