package transfer

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
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
// allow-list of fields (§9): a column added later can't reach the file by
// default, and no id, hash, session or secret is ever on this list.
func BuildFile(st *storage.TransferState, includePeople bool, version string, now time.Time) *File {
	f := &File{
		Format:         FormatName,
		SchemaVersion:  SchemaVersion,
		ExportedAt:     now.UTC().Format(time.RFC3339),
		HomepadVersion: version,
		IncludesPeople: includePeople,
		Settings:       Settings{ShowUptimeDisplay: st.Settings.ShowUptimeDisplay, StatusDegradedMs: copyInt(st.Settings.StatusDegradedMs)},
		Sections:       []Section{},
		Apps:           []App{},
		Library:        []Offer{},
	}

	secs := append([]storage.TransferSection(nil), st.Sections...)
	sort.SliceStable(secs, func(i, j int) bool { return secs[i].SortIndex < secs[j].SortIndex })
	secName := map[string]string{}
	secPos := map[string]int{}
	for i, s := range secs {
		f.Sections = append(f.Sections, Section{Name: s.Name, GridWidth: s.GridWidth})
		secName[s.ID] = s.Name
		secPos[s.ID] = i
	}

	apps := append([]storage.TransferApp(nil), st.Apps...)
	sort.SliceStable(apps, func(i, j int) bool { return apps[i].Slug < apps[j].Slug })
	slugOf := map[string]string{}
	for _, a := range apps {
		slugOf[a.ID] = a.Slug
		out := App{
			Slug: a.Slug, Name: a.Name, Description: a.Description, URL: a.URL, Icon: a.Icon,
			ClickAction: a.ClickAction,
		}
		if out.ClickAction == "" {
			out.ClickAction = "new_tab"
		}
		if a.SectionID != nil {
			if n, ok := secName[*a.SectionID]; ok {
				out.Section = &n
			}
		}
		if a.GatusKey != "" {
			k := a.GatusKey
			out.GatusKey = &k
		}
		for _, ic := range a.Icons {
			d := &IconData{PNG: base64.StdEncoding.EncodeToString(ic.Bytes), SHA256: ic.ETag}
			switch ic.Variant {
			case "light":
				out.UploadedIcons.Light = d
			case "dark":
				out.UploadedIcons.Dark = d
			}
		}
		f.Apps = append(f.Apps, out)
	}

	offers := append([]storage.TransferOffer(nil), st.Offers...)
	sort.SliceStable(offers, func(i, j int) bool { return offers[i].SortIndex < offers[j].SortIndex })
	for _, o := range offers {
		f.Library = append(f.Library, Offer{Name: o.Name, URL: o.URL, Icon: o.Icon, Description: o.Description, SuggestedCategory: o.SuggestedCategory})
	}

	if !includePeople {
		return f
	}
	slugs := func(ids []string, keepOrder bool) []string {
		out := []string{}
		for _, id := range ids {
			if s, ok := slugOf[id]; ok {
				out = append(out, s)
			}
		}
		if !keepOrder {
			sort.Strings(out)
		}
		return out
	}
	f.People = &People{Adults: []Adult{}, Kids: []Kid{}}
	people := append([]storage.TransferPerson(nil), st.People...)
	sort.SliceStable(people, func(i, j int) bool {
		return strings.ToLower(people[i].Email+people[i].DisplayName) < strings.ToLower(people[j].Email+people[j].DisplayName)
	})
	for _, p := range people {
		prefs := Prefs{Theme: strPtr(p.ThemePref), Density: strPtr(p.DensityPref),
			ShowHealthBar: boolPtr(p.ShowHealthBar), ShowUptimeDisplay: boolPtr(p.ShowUptimeDisplay)}
		if p.Role == storage.RoleKid {
			f.People.Kids = append(f.People.Kids, Kid{
				Name: p.DisplayName, AvatarColor: optional(p.AvatarColor), Prefs: prefs,
				AllowedApps: slugs(p.Allowed, false), Favorites: slugs(p.Favorites, false),
			})
			continue
		}
		collapsed := append([]string(nil), p.Collapsed...)
		sort.SliceStable(collapsed, func(i, j int) bool { return secPos[collapsed[i]] < secPos[collapsed[j]] })
		names := []string{}
		for _, id := range collapsed {
			if n, ok := secName[id]; ok {
				names = append(names, n)
			}
		}
		f.People.Adults = append(f.People.Adults, Adult{
			Email: p.Email, DisplayName: optional(p.DisplayName), AvatarColor: optional(p.AvatarColor), Prefs: prefs,
			Favorites: slugs(p.Favorites, false), Order: slugs(p.Order, true), CollapsedSections: names,
		})
	}
	return f
}

func copyInt(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

// optional is "" → nil.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Encode writes the file as canonical JSON: fixed key order, two-space
// indent, no HTML escaping, a trailing newline (§3, AC-005).
func Encode(f *File) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(f); err != nil {
		// A File holds only strings, numbers, bools and slices of them.
		panic("transfer: encode backup: " + err.Error())
	}
	return b.Bytes()
}

// Decoded is a backup read back in, upgraded to SchemaVersion.
type Decoded struct {
	File *File
	// UnknownFields counts keys this schema version doesn't use (AC-018).
	UnknownFields int
}

// upgraders[v] turns a version-v file into a version v+1 file. One per
// version step, run in order before planning (§2.3, AC-017). Every released
// version keeps its fixture in testdata/backups and its step here.
var upgraders = map[int]func(map[string]any) (map[string]any, error){}

const (
	msgNotABackup  = "This isn't a Homepad backup file."
	msgSchemaNewer = "This backup is from a newer Homepad. Update Homepad, then try again."
)

// DecodeBackup reads a backup: it checks the format and version, runs the
// upgrade chain, counts unknown fields and enforces the count limits.
func DecodeBackup(body []byte) (*Decoded, *Error) {
	var raw any
	if err := json.Unmarshal(body, &raw); err != nil {
		e := errorf(http.StatusBadRequest, CodeInvalidJSON, "This file isn't valid JSON.")
		var se *json.SyntaxError
		if errors.As(err, &se) {
			e.Line = 1 + bytes.Count(body[:min(int(se.Offset), len(body))], []byte("\n"))
			e.Message = fmt.Sprintf("This file isn't valid JSON (line %d).", e.Line)
		}
		return nil, e
	}
	obj, ok := raw.(map[string]any)
	if !ok || obj["format"] != FormatName {
		return nil, errorf(http.StatusUnprocessableEntity, CodeNotABackup, msgNotABackup)
	}
	vf, ok := obj["schemaVersion"].(float64)
	if !ok || vf < 1 || vf != math.Trunc(vf) {
		return nil, errorf(http.StatusUnprocessableEntity, CodeNotABackup, msgNotABackup)
	}
	if vf > SchemaVersion {
		return nil, errorf(http.StatusUnprocessableEntity, CodeSchemaNewer, msgSchemaNewer)
	}
	for v := int(vf); v < SchemaVersion; v++ {
		up, ok := upgraders[v]
		if !ok {
			return nil, errorf(http.StatusInternalServerError, CodeNotABackup, fmt.Sprintf("no upgrade from schema %d", v))
		}
		next, err := up(obj)
		if err != nil {
			return nil, errorf(http.StatusUnprocessableEntity, CodeNotABackup, msgNotABackup)
		}
		obj = next
		obj["schemaVersion"] = float64(v + 1)
	}

	unknown := fileShape.unknown(obj)
	norm, _ := json.Marshal(obj)
	var f File
	if err := json.Unmarshal(norm, &f); err != nil {
		msg := msgNotABackup
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) && te.Field != "" {
			msg = fmt.Sprintf("This isn't a Homepad backup file: %s has the wrong type.", te.Field)
		}
		return nil, errorf(http.StatusUnprocessableEntity, CodeNotABackup, msg)
	}
	if f.People != nil {
		f.IncludesPeople = true
	}
	for _, c := range []struct {
		things    string
		got, max_ int
	}{
		{"apps", len(f.Apps), MaxApps},
		{"sections", len(f.Sections), MaxSections},
		{"offers", len(f.Library), MaxOffers},
		{"kids", kidCount(f.People), MaxKids},
	} {
		if c.got > c.max_ {
			return nil, tooMany(c.things, c.got, c.max_)
		}
	}
	return &Decoded{File: &f, UnknownFields: unknown}, nil
}

func kidCount(p *People) int {
	if p == nil {
		return 0
	}
	return len(p.Kids)
}

// shape is the set of keys schema v1 knows, per object, for counting the
// ones it doesn't (AC-018).
type shape struct {
	keys map[string]*shape // object: known keys (nil value = a leaf)
	elem *shape            // array: the element shape
}

func obj(keys map[string]*shape) *shape { return &shape{keys: keys} }
func arr(elem *shape) *shape            { return &shape{elem: elem} }

func (s *shape) unknown(v any) int {
	if s == nil {
		return 0
	}
	n := 0
	switch x := v.(type) {
	case map[string]any:
		if s.keys == nil {
			return 0
		}
		for k, child := range x {
			sub, known := s.keys[k]
			if !known {
				n++
				continue
			}
			n += sub.unknown(child)
		}
	case []any:
		for _, e := range x {
			n += s.elem.unknown(e)
		}
	}
	return n
}

var (
	prefsShape = obj(map[string]*shape{"theme": nil, "density": nil, "showHealthBar": nil, "showUptimeDisplay": nil})
	iconShape  = obj(map[string]*shape{"png": nil, "sha256": nil})
	fileShape  = obj(map[string]*shape{
		"format": nil, "schemaVersion": nil, "exportedAt": nil, "homepadVersion": nil, "includesPeople": nil,
		"settings": obj(map[string]*shape{"showUptimeDisplay": nil, "statusDegradedMs": nil}),
		"sections": arr(obj(map[string]*shape{"name": nil, "gridWidth": nil})),
		"apps": arr(obj(map[string]*shape{
			"slug": nil, "name": nil, "description": nil, "url": nil, "icon": nil, "section": nil,
			"clickAction": nil, "gatusKey": nil,
			"uploadedIcons": obj(map[string]*shape{"light": iconShape, "dark": iconShape}),
		})),
		"library": arr(obj(map[string]*shape{"name": nil, "url": nil, "icon": nil, "description": nil, "suggestedCategory": nil})),
		"people": obj(map[string]*shape{
			"adults": arr(obj(map[string]*shape{
				"email": nil, "displayName": nil, "avatarColor": nil, "prefs": prefsShape,
				"favorites": nil, "order": nil, "collapsedSections": nil,
			})),
			"kids": arr(obj(map[string]*shape{
				"name": nil, "avatarColor": nil, "prefs": prefsShape, "allowedApps": nil, "favorites": nil,
			})),
		}),
	})
)
