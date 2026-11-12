package handlers

import (
	"strings"

	"patchman-be/internal/config"
)

// localOrigins are the dev hosts we accept unconditionally so a fresh
// clone works without configuring ALLOWED_REDIRECT_ORIGINS. Production
// must extend this set via env — they are not assumed safe in any
// trust sense, they're just "convenient for `npm run dev`".
var localOrigins = map[string]bool{
	"http://localhost:3000": true,
	"http://localhost:5173": true,
	"http://127.0.0.1:3000": true,
	"http://127.0.0.1:5173": true,
}

// originAllowed reports whether `origin` may be handed back as an OAuth
// redirect URI host. Logic:
//   - dev localhost hosts always pass (so `npm run dev` works);
//   - anything in cfg.AllowedRedirectOrigins passes;
//   - everything else is rejected.
//
// We intentionally do NOT auto-allow every origin when the env is empty
// — the OAuth callback URL is reflected into the user's browser, so an
// unrestricted allow-list would let any attacker-controlled host become
// a callback target.
func originAllowed(cfg *config.Config, origin string) bool {
	if origin == "" {
		return false
	}
	if localOrigins[origin] {
		return true
	}
	for _, o := range cfg.AllowedRedirectOrigins {
		if strings.EqualFold(o, origin) {
			return true
		}
	}
	return false
}
