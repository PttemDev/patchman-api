// Package sessions tracks signed-in devices for the hosted BE so a lost or
// stolen laptop can be signed out remotely (DESKTOP_SECURITY_RFC L8).
//
// Every session JWT names its row (`sid`). JWTAuth asks Validate on each
// request; answers are cached briefly so the check costs one indexed lookup
// per session per cacheTTL, not per request. Tokens minted before this
// existed carry no sid: they stay valid and pick one up on their next
// sliding refresh.
package sessions

import (
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"patchman-be/internal/models"
)

var (
	ErrRevoked = errors.New("session revoked")
	ErrIdle    = errors.New("session expired after inactivity")
)

const (
	cacheTTL      = 30 * time.Second
	touchInterval = 5 * time.Minute
)

type entry struct {
	userID    string
	revoked   bool
	lastSeen  time.Time
	checkedAt time.Time
	touchedAt time.Time
}

type Store struct {
	db      *gorm.DB
	idleTTL time.Duration
	now     func() time.Time

	mu      sync.Mutex
	cache   map[string]*entry
	cutoffs map[string]cutoffEntry
}

type cutoffEntry struct {
	before    time.Time
	checkedAt time.Time
}

func New(db *gorm.DB, idleTTL time.Duration) *Store {
	return &Store{db: db, idleTTL: idleTTL, now: time.Now, cache: map[string]*entry{}, cutoffs: map[string]cutoffEntry{}}
}

// Default is the process-wide store (nil on the embedded BE, which has no
// JWT sessions).
var Default *Store

// Create records a new device session and returns its id.
func (s *Store) Create(userID, userAgent, ip, clientVersion string) (string, error) {
	now := s.now()
	row := models.UserSession{
		ID: uuid.New().String(), UserID: userID, UserAgent: trim(userAgent, 500), IPAddress: trim(ip, 45),
		ClientVersion: trim(clientVersion, 40), CreatedAt: now, LastSeenAt: now,
	}
	if err := s.db.Create(&row).Error; err != nil {
		return "", err
	}
	return row.ID, nil
}

// Validate reports whether sid may still be used by userID, and records
// activity (throttled).
func (s *Store) Validate(sid, userID string) error {
	now := s.now()
	s.mu.Lock()
	e := s.cache[sid]
	s.mu.Unlock()
	if e == nil || now.Sub(e.checkedAt) > cacheTTL {
		var row models.UserSession
		err := s.db.Select("id", "user_id", "last_seen_at", "revoked_at").Where("id = ?", sid).First(&row).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil // DB hiccup: don't log everyone out
		}
		fresh := &entry{checkedAt: now, userID: row.UserID}
		if err != nil || row.RevokedAt != nil {
			fresh.revoked = true
		} else {
			fresh.lastSeen = row.LastSeenAt
		}
		if e != nil {
			fresh.touchedAt = e.touchedAt
		}
		e = fresh
		s.mu.Lock()
		s.cache[sid] = e
		s.mu.Unlock()
	}
	// A sid only ever validates for the user it was issued to.
	if e.revoked || e.userID != userID {
		return ErrRevoked
	}
	if s.idleTTL > 0 && now.Sub(e.lastSeen) > s.idleTTL {
		return ErrIdle
	}
	if now.Sub(e.touchedAt) > touchInterval {
		s.mu.Lock()
		e.touchedAt, e.lastSeen = now, now
		s.mu.Unlock()
		s.db.Model(&models.UserSession{}).Where("id = ?", sid).Update("last_seen_at", now)
	}
	return nil
}

// List returns userID's live sessions, newest activity first.
func (s *Store) List(userID string) ([]models.UserSession, error) {
	var rows []models.UserSession
	q := s.db.Where("user_id = ? AND revoked_at IS NULL", userID)
	if s.idleTTL > 0 {
		q = q.Where("last_seen_at > ?", s.now().Add(-s.idleTTL))
	}
	err := q.Order("last_seen_at DESC").Find(&rows).Error
	return rows, err
}

// Revoke ends the given sessions of userID (all of them when ids is empty,
// except keep). Returns how many were revoked.
func (s *Store) Revoke(userID string, ids []string, keep string) (int64, error) {
	now := s.now()
	q := s.db.Model(&models.UserSession{}).Where("user_id = ? AND revoked_at IS NULL", userID)
	if len(ids) > 0 {
		q = q.Where("id IN ?", ids)
	}
	if keep != "" {
		q = q.Where("id <> ?", keep)
	}
	res := q.Update("revoked_at", now)
	if res.Error == nil && len(ids) == 0 {
		// "All" / "all others" also ends pre-session tokens, which have no
		// row: anything issued before now without a sid is refused. The
		// caller's own legacy token was upgraded to a sid on first use.
		cut := models.SessionCutoff{UserID: userID, RevokedBefore: now}
		if err := s.db.Save(&cut).Error; err != nil {
			return res.RowsAffected, err
		}
	}
	// Drop cached answers so revocation takes effect immediately on this
	// replica; other replicas see it within cacheTTL.
	s.mu.Lock()
	s.cache = map[string]*entry{}
	s.cutoffs = map[string]cutoffEntry{}
	s.mu.Unlock()
	return res.RowsAffected, res.Error
}

// ValidateLegacy checks a token without a sid against the user's cutoff:
// issued (iat) before the last "sign out everywhere" → ErrRevoked.
func (s *Store) ValidateLegacy(userID string, issuedAt time.Time) error {
	now := s.now()
	s.mu.Lock()
	c, ok := s.cutoffs[userID]
	s.mu.Unlock()
	if !ok || now.Sub(c.checkedAt) > cacheTTL {
		var row models.SessionCutoff
		err := s.db.Where("user_id = ?", userID).First(&row).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil // DB hiccup: don't log everyone out
		}
		c = cutoffEntry{before: row.RevokedBefore, checkedAt: now}
		s.mu.Lock()
		s.cutoffs[userID] = c
		s.mu.Unlock()
	}
	if !c.before.IsZero() && !issuedAt.After(c.before) {
		return ErrRevoked
	}
	return nil
}

// CheckToken applies the session rules to a parsed token's claims: a sid must
// name a live session of that user; a sid-less (pre-session) token must
// postdate the user's cutoff. Shared by JWTAuth and the other JWT readers
// (whiteboard collab socket).
func (s *Store) CheckToken(userID, sid string, issuedAt time.Time) error {
	if sid != "" {
		return s.Validate(sid, userID)
	}
	return s.ValidateLegacy(userID, issuedAt)
}

func trim(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
