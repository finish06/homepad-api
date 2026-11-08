package api

import (
	"fmt"
	"strconv"
	"strings"
)

// DefaultRegistration is the HOMEPAD_REGISTRATION value the API uses when the
// variable is unset or empty (SPEC-household-basics AC-001): sign-up is
// closed, and the first account still registers and becomes admin.
const DefaultRegistration = "invite_only"

// RegistrationFromEnv resolves the raw HOMEPAD_REGISTRATION value: unset or
// blank is DefaultRegistration; anything else goes through
// ParseRegistrationMode, so junk still fails closed with recognized=false.
func RegistrationFromEnv(raw string) (mode RegistrationMode, recognized bool) {
	if strings.TrimSpace(raw) == "" {
		raw = DefaultRegistration
	}
	return ParseRegistrationMode(raw)
}

// ParseMaxAccounts parses HOMEPAD_MAX_ACCOUNTS (AC-028). Unset, blank or 0
// is no limit. Anything that isn't a non-negative whole number is an error:
// the caller stops the API, because a cap that silently switched off would
// leave the public demo open (§2.4).
func ParseMaxAccounts(raw string) (int, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("HOMEPAD_MAX_ACCOUNTS=%q is not a non-negative whole number", raw)
	}
	return n, nil
}

// accountLimitMessage is the 409 body at the cap. It starts with "account
// limit reached" (spec §7) and carries the number, so the web app can say
// "This Homepad has reached its limit of N accounts." without another call.
func accountLimitMessage(limit int) string {
	return fmt.Sprintf("account limit reached (limit %d)", limit)
}
