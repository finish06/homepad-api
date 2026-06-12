package api

import (
	"strconv"
	"strings"
)

// ParseCookieSecure resolves the session cookie's Secure attribute from the raw
// COOKIE_SECURE env value. It defaults to true (secure on) so production — which
// sets nothing and runs behind Pangolin TLS — is safe by default; unset, empty,
// or unparseable values all stay secure. The plain-HTTP nip.io staging/dev setup
// must explicitly set COOKIE_SECURE=false (or 0) to turn it off (#14).
func ParseCookieSecure(raw string) bool {
	b, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return true
	}
	return b
}
