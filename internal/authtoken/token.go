// Package authtoken centralizes issuing the session JWT so every mint site
// (login, register, SSO, and the middleware's sliding refresh) produces an
// identical claim shape and honours the same configurable TTL.
package authtoken

import (
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Mint issues an HS256 session token for userID, valid for ttl from now.
// The claim shape (user_id/iat/exp) is what middleware.JWTAuth validates.
func Mint(secret, userID string, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"user_id": userID,
		"iat":     now.Unix(),
		"exp":     now.Add(ttl).Unix(),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
}
