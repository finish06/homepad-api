package transfer

import (
	"time"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// SPEC-backup-import §2.3 and §3 — the backup file, schema v1.

const (
	// FormatName is the value of every backup's "format" key.
	FormatName = "homepad-backup"
	// SchemaVersion is the newest schema this server reads and the one it
	// writes. It goes up only for a breaking change (§2.3).
	SchemaVersion = 1
)

// Count limits per file (AC-026).
const (
	MaxApps     = 1000
	MaxSections = 200
	MaxOffers   = 1000
	MaxKids     = 100
)

// File is a backup, schema v1. Field order here IS the key order on disk
// (§3: "Key order is fixed"), so don't reorder fields.
type File struct {
	Format         string    `json:"format"`
	SchemaVersion  int       `json:"schemaVersion"`
	ExportedAt     string    `json:"exportedAt"`
	HomepadVersion string    `json:"homepadVersion"`
	IncludesPeople bool      `json:"includesPeople"`
	Settings       Settings  `json:"settings"`
	Sections       []Section `json:"sections"`
	Apps           []App     `json:"apps"`
	Library        []Offer   `json:"library"`
	People         *People   `json:"people,omitempty"`
}

// Settings is System settings.
type Settings struct {
	ShowUptimeDisplay bool `json:"showUptimeDisplay"`
	StatusDegradedMs  *int `json:"statusDegradedMs"`
}

// Section is one section; its array position is its order.
type Section struct {
	Name      string `json:"name"`
	GridWidth int    `json:"gridWidth"`
}

// App is one catalog app. Section nil is Uncategorized.
type App struct {
	Slug          string        `json:"slug"`
	Name          string        `json:"name"`
	Description   string        `json:"description"`
	URL           string        `json:"url"`
	Icon          string        `json:"icon"`
	Section       *string       `json:"section"`
	ClickAction   string        `json:"clickAction"`
	GatusKey      *string       `json:"gatusKey"`
	UploadedIcons UploadedIcons `json:"uploadedIcons"`
}

// UploadedIcons holds up to two variants.
type UploadedIcons struct {
	Light *IconData `json:"light,omitempty"`
	Dark  *IconData `json:"dark,omitempty"`
}

// IconData is base64 PNG bytes and their SHA-256 (hex, = service_icons.etag).
type IconData struct {
	PNG    string `json:"png"`
	SHA256 string `json:"sha256"`
}

// Offer is one App Library offer; its array position is its order.
type Offer struct {
	Name              string `json:"name"`
	URL               string `json:"url"`
	Icon              string `json:"icon"`
	Description       string `json:"description"`
	SuggestedCategory string `json:"suggestedCategory"`
}

// People is present only when the backup includes people.
type People struct {
	Adults []Adult `json:"adults"`
	Kids   []Kid   `json:"kids"`
}

// Prefs are a person's appearance settings. Pointers, so a hand-edited file
// that leaves one out leaves that setting alone.
type Prefs struct {
	Theme             *string `json:"theme"`
	Density           *string `json:"density"`
	ShowHealthBar     *bool   `json:"showHealthBar"`
	ShowUptimeDisplay *bool   `json:"showUptimeDisplay"`
}

// Adult is matched by email on restore and never created.
type Adult struct {
	Email             string   `json:"email"`
	DisplayName       *string  `json:"displayName"`
	AvatarColor       *string  `json:"avatarColor"`
	Prefs             Prefs    `json:"prefs"`
	Favorites         []string `json:"favorites"`
	Order             []string `json:"order"`
	CollapsedSections []string `json:"collapsedSections"`
}

// Kid is matched by name (case-insensitive) and created when missing.
type Kid struct {
	Name        string   `json:"name"`
	AvatarColor *string  `json:"avatarColor"`
	Prefs       Prefs    `json:"prefs"`
	AllowedApps []string `json:"allowedApps"`
	Favorites   []string `json:"favorites"`
}

// BuildFile makes a backup from the install's state. It copies an explicit
// allow-list of fields (§9): nothing reaches the file by default.
func BuildFile(st *storage.TransferState, includePeople bool, version string, now time.Time) *File {
	return nil
}

// Encode writes the file as canonical JSON: fixed key order, two-space
// indent, no HTML escaping, a trailing newline (§3, AC-005).
func Encode(f *File) []byte {
	return nil
}

// Decoded is a backup read back in, upgraded to SchemaVersion.
type Decoded struct {
	File *File
	// UnknownFields counts keys this schema version doesn't use (AC-018).
	UnknownFields int
}

// DecodeBackup reads a backup: it checks the format and version, runs the
// upgrade chain, counts unknown fields and enforces the count limits.
func DecodeBackup(body []byte) (*Decoded, *Error) {
	return nil, errorf(500, "todo", "not implemented")
}
