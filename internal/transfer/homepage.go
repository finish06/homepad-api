package transfer

// SPEC-backup-import §4 — reading Homepage's services.yaml, bookmarks.yaml
// and settings.yaml (gethomepage/homepage docs/configs/*.md and
// src/utils/config/service-helpers.js, 2026-11-07).

const (
	// MaxHomepageFileBytes caps each pasted or uploaded file (AC-026).
	MaxHomepageFileBytes = 1 << 20
	// MaxGroupDepth is the deepest nesting of groups accepted (§4.5).
	MaxGroupDepth = 8
)

// HomepageInput is the request body of the Homepage import routes.
type HomepageInput struct {
	Services  string `json:"services"`
	Bookmarks string `json:"bookmarks,omitempty"`
	Settings  string `json:"settings,omitempty"`
}

// Homepage is what the parser keeps from the three files. Widget blocks,
// `providers` and every other value the import doesn't use are dropped while
// parsing and never reach this struct (§9, AC-022).
type Homepage struct {
	// Sections is every group, in file order: services.yaml first, a parent
	// before its nested groups, then bookmarks.yaml groups not seen yet.
	Sections []HomepageSection
	// Entries is every service and bookmark in Homepage order: per section,
	// services by weight then file order, then the section's bookmarks.
	Entries []HomepageEntry
	// Layout is settings.yaml `layout`'s top-level group names, in order.
	Layout []string
	// Target is settings.yaml `target` ("" when unset).
	Target string
	// IgnoredSettings is every other settings.yaml key, by name only.
	IgnoredSettings []string
}

// HomepageSection is one group. Name is the full "Parent / Child" path.
type HomepageSection struct {
	Name string
	Root string
}

// HomepageEntry is one service or bookmark. Only scalar values the mapping
// uses are kept.
type HomepageEntry struct {
	Section     string
	Name        string
	Href        string
	Description string
	Icon        string
	Target      string
	SiteMonitor string
	Ping        string
	Bookmark    bool
	// NoFields is true when the entry had no field map (Homepage skips it).
	NoFields bool
	// Widget is true when the entry had `widget` or `widgets`. Their values
	// are never read.
	Widget     bool
	Docker     bool // server, container or showStats
	Kubernetes bool // app, namespace or podSelector
	// IgnoredKeys counts keys the import doesn't use (statusStyle, id, …).
	IgnoredKeys int
	Line        int
}

// ParseHomepage parses the three files. services is required.
func ParseHomepage(in HomepageInput) (*Homepage, *Error) {
	return nil, errorf(500, "todo", "not implemented")
}
