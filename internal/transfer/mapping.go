package transfer

import (
	"net/url"
	"regexp"
	"strings"
)

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

// iconNameRe is a Dashboard Icons / selfh.st name with an optional extension.
var iconNameRe = regexp.MustCompile(`^([a-z0-9][a-z0-9._-]*?)(?:\.(png|svg|webp))?$`)

// schemeRe is a URL scheme prefix ("data:", "ftp://", …).
var schemeRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)

// MapIcon maps a Homepage `icon:` value (§4.1 icon rows, OQ-8 a). Homepage
// resolves the same names to the same CDN URLs (resolvedicon.jsx), so the
// tile looks the same.
func MapIcon(v string) IconResult {
	v = strings.TrimSpace(v)
	lower := strings.ToLower(v)
	switch {
	case v == "":
		return IconResult{}
	case IsHTTPURL(v):
		return IconResult{URL: v}
	case strings.HasPrefix(lower, "mdi-") || strings.HasPrefix(lower, "si-"):
		return IconResult{Warning: WarnIconGlyph}
	case schemeRe.MatchString(v):
		return IconResult{Warning: WarnIconUnsupported}
	case strings.Contains(v, "/"):
		return IconResult{Warning: WarnIconLocal}
	}
	base, name := dashboardIconsBase, lower
	if rest, ok := strings.CutPrefix(lower, "sh-"); ok {
		base, name = selfhstIconsBase, rest
	}
	m := iconNameRe.FindStringSubmatch(name)
	if m == nil {
		return IconResult{Warning: WarnIconUnsupported}
	}
	ext := m[2]
	if ext == "" {
		ext = "png"
	}
	return IconResult{URL: base + "/" + ext + "/" + m[1] + "." + ext}
}

// IsHTTPURL reports whether v is an absolute http or https URL with a host.
func IsHTTPURL(v string) bool {
	u, err := url.Parse(strings.TrimSpace(v))
	if err != nil || u.Host == "" {
		return false
	}
	s := strings.ToLower(u.Scheme)
	return s == "http" || s == "https"
}

// NormalizeLink is the form two links are compared in (§2.4): trimmed, no
// trailing slash, scheme and host lowercased. Path and query keep their case.
func NormalizeLink(v string) string {
	v = strings.TrimSpace(v)
	if i := strings.Index(v, "://"); i > 0 {
		rest := v[i+3:]
		host, path := rest, ""
		if j := strings.IndexAny(rest, "/?#"); j >= 0 {
			host, path = rest[:j], rest[j:]
		}
		v = strings.ToLower(v[:i]) + "://" + strings.ToLower(host) + path
	}
	return strings.TrimRight(v, "/")
}

// Slugify is the §4.1 slug rule: lowercase, every run of non a-z0-9 becomes
// "-", trimmed of "-". It matches storage's slugify for add-from-library.
func Slugify(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if b.Len() > 0 && !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// gatusReplacer is Gatus's sanitize: / _ . , space # + & become "-".
var gatusReplacer = strings.NewReplacer("/", "-", "_", "-", ".", "-", ",", "-", " ", "-", "#", "-", "+", "-", "&", "-")

// GatusKey is Gatus's own key rule (config/key/key.go,
// ConvertGroupAndNameToKey): sanitize(group) + "_" + sanitize(name), where
// sanitize lowercases, trims and replaces / _ . , space # + & with "-".
func GatusKey(group, name string) string {
	san := func(s string) string { return gatusReplacer.Replace(strings.ToLower(strings.TrimSpace(s))) }
	return san(group) + "_" + san(name)
}

// ClickActionFor maps a Homepage target (§4.1 target row): the entry's own
// target, else settings.yaml's, else new_tab.
func ClickActionFor(target, fallback string) string {
	t := strings.TrimSpace(target)
	if t == "" {
		t = strings.TrimSpace(fallback)
	}
	switch t {
	case "_self", "_top":
		return "same_tab"
	default:
		return "new_tab"
	}
}

// hasHomepageVariable reports {{HOMEPAGE_VAR_…}}, {{HOMEPAGE_FILE_…}}, any
// other {{…}} placeholder, or ${…}: values Homepad can't fill in.
func hasHomepageVariable(v string) bool {
	return strings.Contains(v, "{{") || strings.Contains(v, "${")
}
