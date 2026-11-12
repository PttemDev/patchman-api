package middleware

import (
	"net/http"
	"strings"
	"time"

	"patchman-be/internal/authtoken"
	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"gorm.io/gorm"
)

// RefreshedTokenHeader carries a freshly-minted session token back to the
// client when the presented one is past its half-life. The FE swaps it into
// storage transparently, so an actively-used session never expires. Must be
// listed in the CORS ExposeHeaders so browser JS can read it.
const RefreshedTokenHeader = "X-Refreshed-Token"

// LocalUserID is the synthetic user id used by the embedded desktop backend.
// All records created in embedded mode are scoped to this id.
const LocalUserID = "local-user"

// LocalUserAuth replaces JWT validation in embedded mode. It sets a fixed
// userID on the context so downstream handlers work unchanged.
func LocalUserAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("userID", LocalUserID)
		c.Next()
	}
}

// JWTAuth validates the bearer/query token and, when it is more than halfway
// to expiry, slides the session forward by minting a fresh token (same user,
// full ttl) and returning it in the RefreshedTokenHeader. ttl must match the
// value used to issue tokens (config.SessionTTL) so the refresh window is
// computed against the right lifetime.
func JWTAuth(secret string, ttl time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		var tokenStr string

		// First try Authorization header
		authHeader := c.GetHeader("Authorization")
		if authHeader != "" {
			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) == 2 && strings.ToLower(parts[0]) == "bearer" {
				tokenStr = parts[1]
			}
		}

		// Fallback to query param (needed for SSE/EventSource which can't set headers)
		if tokenStr == "" {
			tokenStr = c.Query("token")
		}

		if tokenStr == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authorization required"})
			c.Abort()
			return
		}

		token, err := jwt.Parse(tokenStr, func(token *jwt.Token) (interface{}, error) {
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return []byte(secret), nil
		})

		if err != nil || !token.Valid {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired token"})
			c.Abort()
			return
		}

		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid token claims"})
			c.Abort()
			return
		}

		userID, ok := claims["user_id"].(string)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid token: missing user_id"})
			c.Abort()
			return
		}

		// Sliding session: once the presented token is past its half-life,
		// hand back a fresh one so anyone still using the app never hits the
		// expiry wall. Set before c.Next() so the header rides on the response
		// regardless of the handler's status code. Best-effort — a mint
		// failure just means no refresh this round, not a failed request.
		maybeRefresh(c, claims, secret, userID, ttl)

		c.Set("userID", userID)
		c.Next()
	}
}

// maybeRefresh sets the RefreshedTokenHeader when the token is past its
// half-life. No-op when the TTL is unknown, the exp claim is missing, or the
// token still has more than half its life left.
func maybeRefresh(c *gin.Context, claims jwt.MapClaims, secret, userID string, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	exp, err := claims.GetExpirationTime()
	if err != nil || exp == nil {
		return
	}
	if time.Until(exp.Time) >= ttl/2 {
		return
	}
	if fresh, err := authtoken.Mint(secret, userID, ttl); err == nil {
		c.Header(RefreshedTokenHeader, fresh)
	}
}

// ActAsUser lets an admin operate as another user via the X-Act-As-User
// request header. Runs AFTER JWTAuth so userID is already on the context.
//
// Use case: the admin dashboard's "Manage as user" mode browses and edits
// a target user's collections/environments/mocks via the regular CRUD
// endpoints — without us having to clone every endpoint into an admin
// flavour. The middleware silently no-ops when the header is missing or
// the requester isn't an admin, so accidental header forwarding from a
// non-admin is harmless.
func ActAsUser(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Admin endpoints always run as the real admin — they need to
		// list users, set roles, etc. on behalf of the actual signed-in
		// admin. Impersonation only applies to the regular /api/* CRUD
		// routes that operate on user-owned data.
		if strings.HasPrefix(c.Request.URL.Path, "/api/admin") {
			c.Next()
			return
		}
		target := c.GetHeader("X-Act-As-User")
		if target == "" {
			c.Next()
			return
		}
		realID := c.GetString("userID")
		if realID == "" {
			c.Next()
			return
		}
		var requester models.User
		if err := db.Select("role").First(&requester, "id = ?", realID).Error; err != nil {
			c.Next()
			return
		}
		if requester.Role != models.RoleAdmin {
			// Refuse silently — this is a "you didn't have permission"
			// situation, but returning 403 here would also block legit
			// proxied calls if a stray header is forwarded. Audit log
			// catches the attempt for review.
			c.Next()
			return
		}
		c.Set("realUserID", realID)
		c.Set("userID", target)
		c.Set("impersonating", true)
		c.Next()
	}
}

// RequireAdmin loads the user and refuses non-admins. Wrap admin-only
// routes with this AFTER JWTAuth so userID is already set on the context.
func RequireAdmin(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString("userID")
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authorization required"})
			c.Abort()
			return
		}
		var user models.User
		if err := db.Select("role").First(&user, "id = ?", userID).Error; err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid user"})
			c.Abort()
			return
		}
		if user.Role != models.RoleAdmin {
			c.JSON(http.StatusForbidden, gin.H{"error": "Admin access required"})
			c.Abort()
			return
		}
		c.Next()
	}
}
