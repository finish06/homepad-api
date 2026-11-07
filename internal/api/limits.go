package api

// DefaultRegistration is the HOMEPAD_REGISTRATION value used when it is unset.
const DefaultRegistration = "open"

// RegistrationFromEnv resolves HOMEPAD_REGISTRATION.
func RegistrationFromEnv(raw string) (RegistrationMode, bool) {
	return ParseRegistrationMode(raw)
}

// ParseMaxAccounts parses HOMEPAD_MAX_ACCOUNTS.
func ParseMaxAccounts(raw string) (int, error) { return 0, nil }
