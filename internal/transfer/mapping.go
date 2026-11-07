package transfer

// SPEC-backup-import §4.1 — the mapping helpers.

const (
	dashboardIconsBase = "https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons"
	selfhstIconsBase   = "https://cdn.jsdelivr.net/gh/selfhst/icons@main"
)

// IconResult is a Homepage icon value mapped to a Homepad icon.
type IconResult struct {
	URL     string // "" = no icon (the initials badge)
	Warning string // "" or one of the icon_* warnings
}

// MapIcon maps a Homepage `icon:` value (§4.1 icon rows, OQ-8 a).
func MapIcon(v string) IconResult { return IconResult{} }

// IsHTTPURL reports whether v is an absolute http or https URL with a host.
func IsHTTPURL(v string) bool { return false }

// NormalizeLink is the form two links are compared in (§2.4): trimmed, no
// trailing slash, scheme and host lowercased.
func NormalizeLink(v string) string { return v }

// Slugify is the §4.1 slug rule: lowercase, every run of non a-z0-9 becomes
// "-", trimmed of "-".
func Slugify(name string) string { return name }

// GatusKey is Gatus's own key rule (config/key/key.go,
// ConvertGroupAndNameToKey): sanitize(group) + "_" + sanitize(name).
func GatusKey(group, name string) string { return "" }

// ClickActionFor maps a Homepage target (§4.1 target row).
func ClickActionFor(target, fallback string) string { return "" }

// hasHomepageVariable reports {{HOMEPAGE_VAR_…}}, {{HOMEPAGE_FILE_…}} or ${…}.
func hasHomepageVariable(v string) bool { return false }
