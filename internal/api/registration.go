package api

import "strings"

// RegistrationMode is the normalized self-registration policy, resolved once at
// startup from HOMEPAD_REGISTRATION. The zero value is RegistrationClosed so any
// unset or garbage configuration fails closed (registration disabled).
type RegistrationMode int

const (
	// RegistrationClosed disables self-registration (invite-only). It is the
	// zero value on purpose: an unrecognized config must fail closed, never open.
	RegistrationClosed RegistrationMode = iota
	// RegistrationOpen allows anyone to self-register.
	RegistrationOpen
)

// ParseRegistrationMode normalizes a raw HOMEPAD_REGISTRATION value (TrimSpace +
// ToLower) into a typed mode. Only "open" opens registration; "invite_only" and
// "invite-only" (hyphen) are recognized closed values. Any other value is
// unrecognized and fails closed (recognized=false) so the caller can warn at
// startup naming the bad value. This is the single normalization point — the
// handler compares the typed mode, it never re-parses the raw string.
func ParseRegistrationMode(raw string) (mode RegistrationMode, recognized bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "open":
		return RegistrationOpen, true
	case "invite_only", "invite-only":
		return RegistrationClosed, true
	default:
		return RegistrationClosed, false
	}
}
