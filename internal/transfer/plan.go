package transfer

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image/png"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// SPEC-backup-import §2.2, §2.4, §7, §8 — the plan.

// Modes.
const (
	ModeMerge   = "merge"
	ModeReplace = "replace"
	ModeImport  = "import"
)

// Actions.
const (
	ActionCreate    = "create"
	ActionUpdate    = "update"
	ActionUnchanged = "unchanged"
	ActionDelete    = "delete"
	ActionSkip      = "skip"
)

// Kinds.
const (
	KindSection  = "section"
	KindApp      = "app"
	KindIcon     = "icon"
	KindOffer    = "offer"
	KindSettings = "settings"
	KindAdult    = "adult"
	KindKid      = "kid"
)

// Plan is what a preview returns and what an apply carries out (§8).
type Plan struct {
	PlanHash      string        `json:"planHash,omitempty"`
	Mode          string        `json:"mode"`
	Summary       Summary       `json:"summary"`
	Items         []Item        `json:"items"`
	Warnings      []PlanWarning `json:"warnings"`
	Ignored       Ignored       `json:"ignored"`
	GatusSnippet  string        `json:"gatusSnippet,omitempty"`
	OtherAccounts int           `json:"otherAccounts"`
	Applied       bool          `json:"applied,omitempty"`

	ops *ops
}

// Summary is the counts line.
type Summary struct {
	Create    CreateCounts `json:"create"`
	Update    UpdateCounts `json:"update"`
	Delete    DeleteCounts `json:"delete"`
	Unchanged int          `json:"unchanged"`
	Skipped   int          `json:"skipped"`
}

// CreateCounts counts creates.
type CreateCounts struct {
	Sections int `json:"sections"`
	Apps     int `json:"apps"`
	Icons    int `json:"icons"`
	Offers   int `json:"offers"`
	Kids     int `json:"kids"`
}

// UpdateCounts counts updates.
type UpdateCounts struct {
	Sections int `json:"sections"`
	Apps     int `json:"apps"`
	Icons    int `json:"icons"`
	Offers   int `json:"offers"`
	People   int `json:"people"`
	Settings int `json:"settings"`
}

// DeleteCounts counts Replace's deletions.
type DeleteCounts struct {
	Sections int `json:"sections"`
	Apps     int `json:"apps"`
	Offers   int `json:"offers"`
}

// Item is one row of the preview.
type Item struct {
	Kind     string   `json:"kind"`
	Name     string   `json:"name"`
	Action   string   `json:"action"`
	Section  *string  `json:"section,omitempty"`
	Variant  string   `json:"variant,omitempty"`
	Fields   []string `json:"fields,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	Detail   string   `json:"detail,omitempty"`
	Icon     string   `json:"icon,omitempty"`
}

// PlanWarning is a warning that belongs to no one row.
type PlanWarning struct {
	Code  string `json:"code"`
	Count int    `json:"count"`
}

// Ignored is what the input carried that the plan doesn't use.
type Ignored struct {
	Fields   int      `json:"fields"`
	Settings []string `json:"settings"`
	Dropped  []string `json:"dropped"`
}

// ops is the write list behind a plan. It is never serialized: the hash is
// over what the admin saw, and apply re-plans from the live state anyway.
type ops struct {
	replace bool
	owner   string
	caller  string
	// sectionIDs and appIDs name the rows that exist before the apply (empty
	// after a Replace's delete); Apply adds the ones it creates.
	sectionIDs   map[string]string
	appIDs       map[string]string
	sections     []sectionOp
	offers       []offerOp
	apps         []appOp
	link         []string // slugs of created apps to re-link to their offer
	icons        []iconOp
	settings     *storage.SystemSettings
	gatus        []gatusOp
	people       []personOp
	callerLayout []appRef // nil = leave the caller's order alone
}

type sectionOp struct {
	id        string // "" = create
	name      string
	sortIndex int
	gridWidth int
}

type offerOp struct {
	id  string // "" = create
	row storage.TransferOfferRow
}

type appOp struct {
	id      string // "" = create
	row     storage.TransferAppRow
	section *string // section name; resolved to an id at apply time
}

type iconOp struct {
	slug, variant string
	png           []byte
	width, height int
	etag          string
}

type gatusOp struct{ id, key string }

type personOp struct {
	id        string // "" = create a kid named kidName
	kidName   string
	kidColor  string
	set       *storage.TransferPersonRow
	addFavs   []string  // slugs
	order     *[]string // slugs; nil = unchanged
	collapsed *[]string // section names; nil = unchanged
	allowed   *[]string // slugs; nil = unchanged
}

// appRef is an app that exists (id) or that this apply creates (slug).
type appRef struct{ id, slug string }

func newPlan(mode string, st *storage.TransferState) (*Plan, *ops) {
	o := &ops{owner: st.OwnerID, caller: st.CallerID, sectionIDs: map[string]string{}, appIDs: map[string]string{}}
	return &Plan{
		Mode:          mode,
		Items:         []Item{},
		Warnings:      []PlanWarning{},
		Ignored:       Ignored{Settings: []string{}, Dropped: []string{}},
		OtherAccounts: st.OtherAccounts,
		ops:           o,
	}, o
}

// finish counts the summary from the items and seals the plan with its hash.
func (p *Plan) finish() *Plan {
	var s Summary
	for _, it := range p.Items {
		switch it.Action {
		case ActionCreate:
			switch it.Kind {
			case KindSection:
				s.Create.Sections++
			case KindApp:
				s.Create.Apps++
			case KindIcon:
				s.Create.Icons++
			case KindOffer:
				s.Create.Offers++
			case KindKid:
				s.Create.Kids++
			}
		case ActionUpdate:
			switch it.Kind {
			case KindSection:
				s.Update.Sections++
			case KindApp:
				s.Update.Apps++
			case KindIcon:
				s.Update.Icons++
			case KindOffer:
				s.Update.Offers++
			case KindAdult, KindKid:
				s.Update.People++
			case KindSettings:
				s.Update.Settings++
			}
		case ActionDelete:
			switch it.Kind {
			case KindSection:
				s.Delete.Sections++
			case KindApp:
				s.Delete.Apps++
			case KindOffer:
				s.Delete.Offers++
			}
		case ActionUnchanged:
			s.Unchanged++
		case ActionSkip:
			s.Skipped++
		}
	}
	p.Summary = s
	p.PlanHash = p.hash()
	return p
}

// hash is the SHA-256 of the plan's canonical JSON (§7). OtherAccounts is
// left out: a sign-up between preview and apply doesn't change what lands.
func (p *Plan) hash() string {
	c := *p
	c.PlanHash, c.Applied, c.OtherAccounts = "", false, 0
	b, err := json.Marshal(&c)
	if err != nil {
		panic("transfer: hash plan: " + err.Error())
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Result is the plan as an apply's response: no hash, applied true.
func (p *Plan) Result() *Plan {
	r := *p
	r.PlanHash = ""
	r.Applied = true
	return &r
}

func (p *Plan) add(it Item) { p.Items = append(p.Items, it) }

func skip(kind, name, reason string) Item {
	return Item{Kind: kind, Name: name, Action: ActionSkip, Reason: reason}
}

// cleanText refuses what Postgres TEXT can't store (NUL, invalid UTF-8).
func cleanText(vals ...string) bool {
	for _, v := range vals {
		if !utf8.ValidString(v) || strings.ContainsRune(v, 0) {
			return false
		}
	}
	return true
}

var (
	validWidths  = map[int]bool{3: true, 4: true, 6: true, 12: true}
	validThemes  = map[string]bool{"system": true, "light": true, "dark": true}
	validDensity = map[string]bool{"large": true, "compact": true, "list": true}
	avatarColors = map[string]bool{"blue": true, "teal": true, "green": true, "yellow": true,
		"orange": true, "red": true, "pink": true, "purple": true}
	clickActions = map[string]bool{"new_tab": true, "same_tab": true, "iframe": true}
)

const maxKidNameLen = 40

func validPrefs(p Prefs) bool {
	return (p.Theme == nil || validThemes[*p.Theme]) && (p.Density == nil || validDensity[*p.Density])
}

func validColor(c *string) bool { return c == nil || avatarColors[*c] }

// kidName is the Accounts API's rule: 1–40 characters, no control characters.
func kidName(raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	n := utf8.RuneCountInString(name)
	if !utf8.ValidString(name) || n < 1 || n > maxKidNameLen {
		return "", false
	}
	for _, c := range name {
		if unicode.IsControl(c) {
			return "", false
		}
	}
	return name, true
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// slots assigns sort indexes (§3 "Order is the array order") without
// moving anything the file doesn't name: the matched rows take the file's
// order among the slots they already hold, and new rows go after the last.
func slots(curSort []int, matched []bool, maxSort int) []int {
	var held []int
	for i, m := range matched {
		if m {
			held = append(held, curSort[i])
		}
	}
	sort.Ints(held)
	out := make([]int, len(matched))
	k := 0
	for i, m := range matched {
		if m {
			out[i] = held[k]
			k++
		} else {
			maxSort++
			out[i] = maxSort
		}
	}
	return out
}

// Icon upload rules, the same as PUT /api/services/{id}/icon/{variant}
// (internal/api/icons.go).
const (
	iconMaxBytes = 256 * 1024
	iconMaxDim   = 512
	iconMinDim   = 16
)

var pngMagic = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}

// checkIcon decodes and validates an icon from a backup (AC-015).
func checkIcon(d *IconData) (raw []byte, w, h int, ok bool) {
	raw, err := base64.StdEncoding.DecodeString(d.PNG)
	if err != nil || len(raw) > iconMaxBytes || len(raw) < len(pngMagic) || !bytes.Equal(raw[:len(pngMagic)], pngMagic) {
		return nil, 0, 0, false
	}
	sum := sha256.Sum256(raw)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), d.SHA256) {
		return nil, 0, 0, false
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width > iconMaxDim || cfg.Height > iconMaxDim || cfg.Width < iconMinDim || cfg.Height < iconMinDim {
		return nil, 0, 0, false
	}
	return raw, cfg.Width, cfg.Height, true
}

// refs keeps the references that name something in valid, once each, in
// order, and counts the rest (§3: "dropped, with a warning").
func refs(list []string, valid map[string]bool) ([]string, int) {
	out := []string{}
	seen := map[string]bool{}
	missing := 0
	for _, r := range list {
		switch {
		case !valid[r]:
			missing++
		case !seen[r]:
			seen[r] = true
			out = append(out, r)
		}
	}
	return out, missing
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	return slices.Equal(x, y)
}

// PlanRestore works out a restore of d onto st in mode merge or replace
// (§2.2). People are merged in both modes.
func PlanRestore(st *storage.TransferState, d *Decoded, mode string) *Plan {
	f := d.File
	p, o := newPlan(mode, st)
	o.replace = mode == ModeReplace

	curSecs, curApps, curOffers := st.Sections, st.Apps, st.Offers
	if o.replace {
		secName := map[string]string{}
		for _, s := range curSecs {
			secName[s.ID] = s.Name
		}
		for _, s := range curSecs {
			p.add(Item{Kind: KindSection, Name: s.Name, Action: ActionDelete})
		}
		for _, a := range curApps {
			it := Item{Kind: KindApp, Name: a.Name, Action: ActionDelete}
			if a.SectionID != nil {
				n := secName[*a.SectionID]
				it.Section = &n
			}
			p.add(it)
		}
		for _, of := range curOffers {
			p.add(Item{Kind: KindOffer, Name: of.Name, Action: ActionDelete})
		}
		curSecs, curApps, curOffers = nil, nil, nil
	}

	// Sections.
	curSecByName := map[string]storage.TransferSection{}
	secNameByID := map[string]string{}
	maxSort := -1
	for _, s := range curSecs {
		curSecByName[s.Name] = s
		secNameByID[s.ID] = s.Name
		o.sectionIDs[s.Name] = s.ID
		maxSort = max(maxSort, s.SortIndex)
	}
	var fileSecs []Section
	seenSec := map[string]bool{}
	for _, s := range f.Sections {
		switch {
		case strings.TrimSpace(s.Name) == "":
			p.add(skip(KindSection, s.Name, ReasonNoName))
		case seenSec[s.Name]:
			p.add(skip(KindSection, s.Name, ReasonDuplicate))
		case !validWidths[s.GridWidth] || !cleanText(s.Name):
			p.add(skip(KindSection, s.Name, ReasonInvalidItem))
		default:
			seenSec[s.Name] = true
			fileSecs = append(fileSecs, s)
		}
	}
	matched := make([]bool, len(fileSecs))
	curSort := make([]int, len(fileSecs))
	for i, s := range fileSecs {
		if c, ok := curSecByName[s.Name]; ok {
			matched[i], curSort[i] = true, c.SortIndex
		}
	}
	targets := slots(curSort, matched, maxSort)
	sectionExists := map[string]bool{}
	for name := range curSecByName {
		sectionExists[name] = true
	}
	validSecNames := map[string]bool{}
	for i, s := range fileSecs {
		sectionExists[s.Name] = true
		validSecNames[s.Name] = true
		cur, ok := curSecByName[s.Name]
		if !ok {
			p.add(Item{Kind: KindSection, Name: s.Name, Action: ActionCreate})
			o.sections = append(o.sections, sectionOp{name: s.Name, sortIndex: targets[i], gridWidth: s.GridWidth})
			continue
		}
		var fields []string
		if cur.GridWidth != s.GridWidth {
			fields = append(fields, "gridWidth")
		}
		if cur.SortIndex != targets[i] {
			fields = append(fields, "order")
		}
		if len(fields) == 0 {
			p.add(Item{Kind: KindSection, Name: s.Name, Action: ActionUnchanged})
			continue
		}
		p.add(Item{Kind: KindSection, Name: s.Name, Action: ActionUpdate, Fields: fields})
		o.sections = append(o.sections, sectionOp{id: cur.ID, name: s.Name, sortIndex: targets[i], gridWidth: s.GridWidth})
	}

	// Apps and their uploaded icons.
	curBySlug := map[string]storage.TransferApp{}
	slugByID := map[string]string{}
	for _, a := range curApps {
		curBySlug[a.Slug] = a
		slugByID[a.ID] = a.Slug
		o.appIDs[a.Slug] = a.ID
	}
	validSlugs := map[string]bool{}
	for _, a := range f.Apps {
		slug, name := strings.TrimSpace(a.Slug), strings.TrimSpace(a.Name)
		url, icon := strings.TrimSpace(a.URL), strings.TrimSpace(a.Icon)
		gatus := strings.TrimSpace(deref(a.GatusKey))
		click := a.ClickAction
		if click == "" {
			click = "new_tab"
		}
		switch {
		case slug == "" || !cleanText(slug, a.Name, a.Description, a.URL, a.Icon, gatus, deref(a.Section)) || !clickActions[click]:
			p.add(skip(KindApp, a.Name, ReasonInvalidItem))
			continue
		case name == "":
			p.add(skip(KindApp, a.Name, ReasonNoName))
			continue
		case validSlugs[slug]:
			p.add(skip(KindApp, a.Name, ReasonDuplicate))
			continue
		case url == "":
			p.add(skip(KindApp, name, ReasonNoLink))
			continue
		case !IsHTTPURL(url):
			p.add(skip(KindApp, name, ReasonBadScheme))
			continue
		}
		validSlugs[slug] = true
		it := Item{Kind: KindApp, Name: name}
		if icon != "" && !IsHTTPURL(icon) {
			icon = ""
			it.Warnings = append(it.Warnings, WarnIconUnsupported)
		}
		var section *string
		if a.Section != nil {
			if sectionExists[*a.Section] {
				n := *a.Section
				section = &n
			} else {
				it.Warnings = append(it.Warnings, WarnSectionMissing)
			}
		}
		it.Section = section
		row := storage.TransferAppRow{Slug: slug, Name: name, Description: a.Description, URL: url, Icon: icon,
			GatusKey: gatus, ClickAction: click}

		cur, exists := curBySlug[slug]
		var iconItems []Item
		for _, v := range []struct {
			variant string
			data    *IconData
		}{{"light", a.UploadedIcons.Light}, {"dark", a.UploadedIcons.Dark}} {
			if v.data == nil {
				continue
			}
			ii := Item{Kind: KindIcon, Name: name, Variant: v.variant}
			raw, w, h, ok := checkIcon(v.data)
			if !ok {
				ii.Action, ii.Reason = ActionSkip, ReasonIconInvalid
				it.Warnings = append(it.Warnings, WarnIconInvalid)
				iconItems = append(iconItems, ii)
				continue
			}
			etag := strings.ToLower(v.data.SHA256)
			curETag, had := "", false
			for _, ic := range cur.Icons {
				if exists && ic.Variant == v.variant {
					curETag, had = ic.ETag, true
				}
			}
			switch {
			case had && curETag == etag:
				ii.Action = ActionUnchanged
			case had:
				ii.Action = ActionUpdate
			default:
				ii.Action = ActionCreate
			}
			if ii.Action != ActionUnchanged {
				o.icons = append(o.icons, iconOp{slug: slug, variant: v.variant, png: raw, width: w, height: h, etag: etag})
			}
			iconItems = append(iconItems, ii)
		}

		if !exists {
			it.Action = ActionCreate
			o.apps = append(o.apps, appOp{row: row, section: section})
			o.link = append(o.link, slug)
		} else {
			curSec := (*string)(nil)
			if cur.SectionID != nil {
				if n, ok := secNameByID[*cur.SectionID]; ok {
					curSec = &n
				}
			}
			var fields []string
			for _, c := range []struct {
				name string
				diff bool
			}{
				{"name", cur.Name != row.Name},
				{"description", cur.Description != row.Description},
				{"url", cur.URL != row.URL},
				{"icon", cur.Icon != row.Icon},
				{"section", deref(curSec) != deref(section) || (curSec == nil) != (section == nil)},
				{"clickAction", cur.ClickAction != row.ClickAction},
				{"gatusKey", cur.GatusKey != row.GatusKey},
			} {
				if c.diff {
					fields = append(fields, c.name)
				}
			}
			if len(fields) == 0 {
				it.Action = ActionUnchanged
			} else {
				it.Action, it.Fields = ActionUpdate, fields
				o.apps = append(o.apps, appOp{id: cur.ID, row: row, section: section})
			}
		}
		p.add(it)
		for _, ii := range iconItems {
			p.add(ii)
		}
	}

	// App Library offers, matched by name and URL.
	type offerKey struct{ name, url string }
	curOffer := map[offerKey]storage.TransferOffer{}
	maxOffer := -1
	for _, of := range curOffers {
		curOffer[offerKey{of.Name, of.URL}] = of
		maxOffer = max(maxOffer, of.SortIndex)
	}
	type pendingOffer struct {
		it  Item
		row storage.TransferOfferRow
		cur *storage.TransferOffer
	}
	var pend []pendingOffer
	seenOffer := map[offerKey]bool{}
	for _, of := range f.Library {
		name, url, icon := strings.TrimSpace(of.Name), strings.TrimSpace(of.URL), strings.TrimSpace(of.Icon)
		key := offerKey{name, url}
		switch {
		case !cleanText(of.Name, of.URL, of.Icon, of.Description, of.SuggestedCategory):
			p.add(skip(KindOffer, of.Name, ReasonInvalidItem))
			continue
		case name == "":
			p.add(skip(KindOffer, of.Name, ReasonNoName))
			continue
		case url == "":
			p.add(skip(KindOffer, name, ReasonNoLink))
			continue
		case !IsHTTPURL(url):
			p.add(skip(KindOffer, name, ReasonBadScheme))
			continue
		case seenOffer[key]:
			p.add(skip(KindOffer, name, ReasonDuplicate))
			continue
		}
		seenOffer[key] = true
		it := Item{Kind: KindOffer, Name: name}
		if icon != "" && !IsHTTPURL(icon) {
			icon = ""
			it.Warnings = append(it.Warnings, WarnIconUnsupported)
		}
		po := pendingOffer{it: it, row: storage.TransferOfferRow{Name: name, URL: url, Icon: icon,
			Description: of.Description, SuggestedCategory: of.SuggestedCategory}}
		if c, ok := curOffer[key]; ok {
			po.cur = &c
		}
		pend = append(pend, po)
	}
	om := make([]bool, len(pend))
	os := make([]int, len(pend))
	for i, po := range pend {
		if po.cur != nil {
			om[i], os[i] = true, po.cur.SortIndex
		}
	}
	otargets := slots(os, om, maxOffer)
	for i, po := range pend {
		po.row.SortIndex = otargets[i]
		if po.cur == nil {
			po.it.Action = ActionCreate
			o.offers = append(o.offers, offerOp{row: po.row})
			p.add(po.it)
			continue
		}
		var fields []string
		if po.cur.Icon != po.row.Icon {
			fields = append(fields, "icon")
		}
		if po.cur.Description != po.row.Description {
			fields = append(fields, "description")
		}
		if po.cur.SuggestedCategory != po.row.SuggestedCategory {
			fields = append(fields, "suggestedCategory")
		}
		if po.cur.SortIndex != po.row.SortIndex {
			fields = append(fields, "order")
		}
		if len(fields) == 0 {
			po.it.Action = ActionUnchanged
		} else {
			po.it.Action, po.it.Fields = ActionUpdate, fields
			o.offers = append(o.offers, offerOp{id: po.cur.ID, row: po.row})
		}
		p.add(po.it)
	}

	// System settings.
	finalSettings := st.Settings
	if s := f.Settings; s.StatusDegradedMs != nil && (*s.StatusDegradedMs < 0 || *s.StatusDegradedMs > 600000) {
		p.add(skip(KindSettings, "System settings", ReasonInvalidItem))
	} else {
		want := storage.SystemSettings{ShowUptimeDisplay: s.ShowUptimeDisplay, StatusDegradedMs: copyInt(s.StatusDegradedMs)}
		var fields []string
		if want.ShowUptimeDisplay != st.Settings.ShowUptimeDisplay {
			fields = append(fields, "showUptimeDisplay")
		}
		if !equalIntPtr(want.StatusDegradedMs, st.Settings.StatusDegradedMs) {
			fields = append(fields, "statusDegradedMs")
		}
		if len(fields) == 0 {
			p.add(Item{Kind: KindSettings, Name: "System settings", Action: ActionUnchanged})
		} else {
			p.add(Item{Kind: KindSettings, Name: "System settings", Action: ActionUpdate, Fields: fields})
			o.settings = &want
			finalSettings = want
		}
	}

	// People: always merged (§2.2).
	refMissing := 0
	if f.People != nil {
		refMissing = planPeople(p, o, st, f.People, curApps, curSecs, validSlugs, validSecNames, finalSettings)
	}
	if refMissing > 0 {
		p.Warnings = append(p.Warnings, PlanWarning{Code: WarnRefMissing, Count: refMissing})
	}
	if d.UnknownFields > 0 {
		p.Warnings = append(p.Warnings, PlanWarning{Code: WarnUnknownFields, Count: d.UnknownFields})
		p.Ignored.Fields = d.UnknownFields
	}
	return p.finish()
}

func equalIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// planPeople matches adults by email and kids by name and works out what
// changes for each (§2.2). It returns how many references it dropped.
func planPeople(p *Plan, o *ops, st *storage.TransferState, ppl *People, curApps []storage.TransferApp,
	curSecs []storage.TransferSection, validSlugs, validSecNames map[string]bool, settings storage.SystemSettings) int {
	slugByID := map[string]string{}
	for _, a := range curApps {
		slugByID[a.ID] = a.Slug
	}
	secByID := map[string]string{}
	for _, s := range curSecs {
		secByID[s.ID] = s.Name
	}
	toSlugs := func(ids []string) []string {
		out := []string{}
		for _, id := range ids {
			if s, ok := slugByID[id]; ok {
				out = append(out, s)
			}
		}
		return out
	}
	adults := map[string]storage.TransferPerson{}
	kids := map[string]storage.TransferPerson{}
	for _, x := range st.People {
		if x.Role == storage.RoleKid {
			kids[strings.ToLower(x.DisplayName)] = x
		} else {
			adults[strings.ToLower(x.Email)] = x
		}
	}
	prefsOf := func(cur storage.TransferPerson, f Prefs) storage.TransferPersonRow {
		r := storage.TransferPersonRow{ThemePref: cur.ThemePref, DensityPref: cur.DensityPref,
			ShowHealthBar: cur.ShowHealthBar, ShowUptimeDisplay: cur.ShowUptimeDisplay}
		if f.Theme != nil {
			r.ThemePref = *f.Theme
		}
		if f.Density != nil {
			r.DensityPref = *f.Density
		}
		if f.ShowHealthBar != nil {
			r.ShowHealthBar = *f.ShowHealthBar
		}
		if f.ShowUptimeDisplay != nil {
			r.ShowUptimeDisplay = *f.ShowUptimeDisplay
		}
		return r
	}
	samePrefs := func(cur storage.TransferPerson, r storage.TransferPersonRow) bool {
		return cur.ThemePref == r.ThemePref && cur.DensityPref == r.DensityPref &&
			cur.ShowHealthBar == r.ShowHealthBar && cur.ShowUptimeDisplay == r.ShowUptimeDisplay
	}
	addedFavs := func(have []string, want []string) []string {
		h := map[string]bool{}
		for _, s := range have {
			h[s] = true
		}
		var add []string
		for _, s := range want {
			if !h[s] {
				add = append(add, s)
			}
		}
		return add
	}

	total := 0
	seen := map[string]bool{}
	for _, a := range ppl.Adults {
		email := strings.TrimSpace(a.Email)
		key := strings.ToLower(email)
		switch {
		case email == "" || !cleanText(email, deref(a.DisplayName)) || !validPrefs(a.Prefs) || !validColor(a.AvatarColor):
			p.add(skip(KindAdult, a.Email, ReasonInvalidItem))
			continue
		case seen[key]:
			p.add(skip(KindAdult, email, ReasonDuplicate))
			continue
		}
		seen[key] = true
		cur, ok := adults[key]
		if !ok {
			p.add(skip(KindAdult, email, ReasonAdultNoAccount))
			continue
		}
		it := Item{Kind: KindAdult, Name: email}
		favs, m1 := refs(a.Favorites, validSlugs)
		order, m2 := refs(a.Order, validSlugs)
		coll, m3 := refs(a.CollapsedSections, validSecNames)
		if m := m1 + m2 + m3; m > 0 {
			total += m
			it.Warnings = append(it.Warnings, WarnRefMissing)
		}
		op := personOp{id: cur.ID}
		row := prefsOf(cur, a.Prefs)
		row.DisplayName, row.AvatarColor = optional(deref(a.DisplayName)), optional(deref(a.AvatarColor))
		var fields []string
		if cur.DisplayName != deref(a.DisplayName) {
			fields = append(fields, "displayName")
		}
		if cur.AvatarColor != deref(a.AvatarColor) {
			fields = append(fields, "avatarColor")
		}
		if !samePrefs(cur, row) {
			fields = append(fields, "prefs")
		}
		if len(fields) > 0 {
			op.set = &row
		}
		if add := addedFavs(toSlugs(cur.Favorites), favs); len(add) > 0 {
			fields = append(fields, "favorites")
			op.addFavs = add
		}
		if !slices.Equal(toSlugs(cur.Order), order) {
			fields = append(fields, "order")
			op.order = &order
		}
		var curColl []string
		for _, id := range cur.Collapsed {
			if n, ok := secByID[id]; ok {
				curColl = append(curColl, n)
			}
		}
		if !sameSet(curColl, coll) {
			fields = append(fields, "collapsedSections")
			op.collapsed = &coll
		}
		if len(fields) == 0 {
			it.Action = ActionUnchanged
		} else {
			it.Action, it.Fields = ActionUpdate, fields
			o.people = append(o.people, op)
		}
		p.add(it)
	}

	seenKid := map[string]bool{}
	for _, k := range ppl.Kids {
		name, ok := kidName(k.Name)
		key := strings.ToLower(name)
		switch {
		case !ok || !validPrefs(k.Prefs) || !validColor(k.AvatarColor):
			p.add(skip(KindKid, k.Name, ReasonInvalidItem))
			continue
		case seenKid[key]:
			p.add(skip(KindKid, name, ReasonDuplicate))
			continue
		}
		seenKid[key] = true
		it := Item{Kind: KindKid, Name: name}
		allowed, m1 := refs(k.AllowedApps, validSlugs)
		allowedSet := map[string]bool{}
		for _, s := range allowed {
			allowedSet[s] = true
		}
		favs, m2 := refs(k.Favorites, validSlugs)
		favs, _ = refs(favs, allowedSet) // a kid's favorites are always allowed apps
		if m := m1 + m2; m > 0 {
			total += m
			it.Warnings = append(it.Warnings, WarnRefMissing)
		}
		cur, exists := kids[key]
		if !exists {
			color := deref(k.AvatarColor)
			if color == "" {
				color = "blue"
			}
			base := storage.TransferPerson{ThemePref: "system", DensityPref: "large", ShowHealthBar: false,
				ShowUptimeDisplay: settings.ShowUptimeDisplay}
			row := prefsOf(base, k.Prefs)
			row.AvatarColor = &color
			it.Action = ActionCreate
			it.Warnings = append(it.Warnings, WarnKidNeedsDevice)
			o.people = append(o.people, personOp{kidName: name, kidColor: color, set: &row, addFavs: favs, allowed: &allowed})
			p.add(it)
			continue
		}
		op := personOp{id: cur.ID}
		row := prefsOf(cur, k.Prefs)
		row.AvatarColor = optional(cur.AvatarColor)
		var fields []string
		if k.AvatarColor != nil && *k.AvatarColor != cur.AvatarColor {
			row.AvatarColor = optional(*k.AvatarColor)
			fields = append(fields, "avatarColor")
		}
		if !samePrefs(cur, row) {
			fields = append(fields, "prefs")
		}
		if len(fields) > 0 {
			op.set = &row
		}
		if !sameSet(toSlugs(cur.Allowed), allowed) {
			fields = append(fields, "allowedApps")
			op.allowed = &allowed
		}
		if add := addedFavs(toSlugs(cur.Favorites), favs); len(add) > 0 {
			fields = append(fields, "favorites")
			op.addFavs = add
		}
		if len(fields) == 0 {
			it.Action = ActionUnchanged
		} else {
			it.Action, it.Fields = ActionUpdate, fields
			o.people = append(o.people, op)
		}
		p.add(it)
	}
	return total
}

// PlanImport works out a Homepage import onto st (§2.4, §4). It is always a
// merge: nothing existing is deleted, renamed or moved. gatusKeys is the set
// of keys the current Gatus snapshot has (§4.3).
func PlanImport(st *storage.TransferState, hp *Homepage, gatusKeys map[string]bool) *Plan {
	p, o := newPlan(ModeImport, st)
	p.Ignored.Settings = append(p.Ignored.Settings, hp.IgnoredSettings...)

	curSec := map[string]bool{}
	maxSort := -1
	for _, s := range st.Sections {
		curSec[s.Name] = true
		o.sectionIDs[s.Name] = s.ID
		maxSort = max(maxSort, s.SortIndex)
	}
	curLinks := map[string]storage.TransferApp{}
	usedSlugs := map[string]bool{}
	for _, a := range st.Apps {
		o.appIDs[a.Slug] = a.ID
		usedSlugs[a.Slug] = true
		if _, ok := curLinks[NormalizeLink(a.URL)]; !ok {
			curLinks[NormalizeLink(a.URL)] = a
		}
	}

	// New sections go after the existing ones: settings.yaml layout order
	// first, then file order (§4.1).
	layoutIdx := map[string]int{}
	for i, name := range hp.Layout {
		if _, ok := layoutIdx[name]; !ok {
			layoutIdx[name] = i
		}
	}
	secs := append([]HomepageSection(nil), hp.Sections...)
	rank := func(s HomepageSection) int {
		if i, ok := layoutIdx[s.Root]; ok {
			return i
		}
		return len(hp.Layout)
	}
	sort.SliceStable(secs, func(i, j int) bool { return rank(secs[i]) < rank(secs[j]) })
	bySection := map[string][]HomepageEntry{}
	for _, e := range hp.Entries {
		bySection[e.Section] = append(bySection[e.Section], e)
	}

	var snippet []gatusEndpoint
	var created []string
	fileLinks := map[string]bool{}
	dropped := map[string]bool{}
	for _, sec := range secs {
		var sectionPtr *string
		if sec.Name != "" {
			n := sec.Name
			sectionPtr = &n
		}
		var appItems []Item
		creates := 0
		for _, e := range bySection[sec.Name] {
			p.Ignored.Fields += e.IgnoredKeys
			if e.Widget {
				dropped["widgets"] = true
			}
			if e.Docker {
				dropped["docker"] = true
			}
			if e.Kubernetes {
				dropped["kubernetes"] = true
			}
			it := Item{Kind: KindApp, Name: e.Name, Section: sectionPtr}
			href := strings.TrimSpace(e.Href)
			reason := ""
			switch {
			case e.Name == "":
				reason = ReasonNoName
			case e.NoFields || href == "":
				reason = ReasonNoLink
			case hasHomepageVariable(href):
				reason = ReasonHrefVariable
			case !IsHTTPURL(href):
				reason = ReasonBadScheme
			case !cleanText(e.Name, e.Description, href, e.Icon, sec.Name):
				reason = ReasonInvalidItem
			}
			if reason != "" {
				it.Action, it.Reason = ActionSkip, reason
				appItems = append(appItems, it)
				continue
			}
			norm := NormalizeLink(href)
			key, monitor := "", monitorFor(e)
			if monitor != nil {
				key = GatusKey(sec.Name, e.Name)
				monitor.Name, monitor.Group = e.Name, sec.Name
			}
			if cur, ok := curLinks[norm]; ok {
				it.Reason = ReasonAlreadyHere
				it.Action = ActionUnchanged
				if monitor != nil && cur.GatusKey == "" {
					if gatusKeys[key] {
						it.Action, it.Fields, it.Warnings = ActionUpdate, []string{"gatusKey"}, []string{WarnStatusOn}
						o.gatus = append(o.gatus, gatusOp{id: cur.ID, key: key})
					} else {
						snippet = append(snippet, *monitor)
					}
				}
				appItems = append(appItems, it)
				continue
			}
			if fileLinks[norm] {
				it.Action, it.Reason = ActionSkip, ReasonDuplicate
				appItems = append(appItems, it)
				continue
			}
			fileLinks[norm] = true
			if e.Widget {
				it.Warnings = append(it.Warnings, WarnWidgetDropped)
			}
			icon := MapIcon(e.Icon)
			it.Icon = icon.URL
			if icon.Warning != "" {
				it.Warnings = append(it.Warnings, icon.Warning)
				if icon.Warning == WarnIconGlyph {
					it.Detail = strings.TrimSpace(e.Icon)
				}
			}
			if hasHomepageVariable(e.Description) {
				it.Warnings = append(it.Warnings, WarnDescriptionVariable)
			}
			gatus := ""
			if monitor != nil {
				if gatusKeys[key] {
					gatus = key
				} else {
					snippet = append(snippet, *monitor)
				}
			}
			slug := uniqueSlug(Slugify(e.Name), usedSlugs)
			it.Action = ActionCreate
			o.apps = append(o.apps, appOp{section: sectionPtr, row: storage.TransferAppRow{
				Slug: slug, Name: e.Name, Description: e.Description, URL: href, Icon: icon.URL,
				GatusKey: gatus, ClickAction: ClickActionFor(e.Target, hp.Target),
			}})
			created = append(created, slug)
			creates++
			appItems = append(appItems, it)
		}
		if creates > 0 && sectionPtr != nil && !curSec[sec.Name] {
			maxSort++
			p.add(Item{Kind: KindSection, Name: sec.Name, Action: ActionCreate})
			o.sections = append(o.sections, sectionOp{name: sec.Name, sortIndex: maxSort, gridWidth: 6})
			curSec[sec.Name] = true
		}
		for _, it := range appItems {
			p.add(it)
		}
	}
	for _, d := range []string{"widgets", "docker", "kubernetes"} {
		if dropped[d] {
			p.Ignored.Dropped = append(p.Ignored.Dropped, d)
		}
	}
	p.GatusSnippet = gatusSnippet(snippet)

	// §4.4 — the importing admin's own order: their current view, then the
	// imported apps in Homepage order.
	if len(created) > 0 {
		for _, id := range st.CallerOrder {
			o.callerLayout = append(o.callerLayout, appRef{id: id})
		}
		for _, slug := range created {
			o.callerLayout = append(o.callerLayout, appRef{slug: slug})
		}
	}
	return p.finish()
}

func uniqueSlug(base string, used map[string]bool) string {
	if base == "" {
		base = "app"
	}
	slug := base
	for i := 2; used[slug]; i++ {
		slug = fmt.Sprintf("%s-%d", base, i)
	}
	used[slug] = true
	return slug
}
