package transfer_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/png"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/transfer"
)

// SPEC-backup-import §2.2 — restore plans (AC-007, AC-010…AC-015, AC-024).

func restorePlan(t *testing.T, st *storage.TransferState, d *transfer.Decoded, mode string) *transfer.Plan {
	t.Helper()
	p := transfer.PlanRestore(st, d, mode)
	require.NotNil(t, p)
	assert.Equal(t, mode, p.Mode)
	assert.Len(t, p.PlanHash, 64)
	assertEverySkipHasReason(t, p)
	return p
}

// decodeFile round-trips a File through the decoder, as an upload would.
func decodeFile(t *testing.T, f *transfer.File) *transfer.Decoded {
	t.Helper()
	b, err := json.Marshal(f)
	require.NoError(t, err)
	d, derr := transfer.DecodeBackup(b)
	require.Nil(t, derr, "%v", derr)
	return d
}

func pngData(t *testing.T, w, h int) *transfer.IconData {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))))
	sum := sha256.Sum256(buf.Bytes())
	return &transfer.IconData{PNG: base64.StdEncoding.EncodeToString(buf.Bytes()), SHA256: hex.EncodeToString(sum[:])}
}

// AC-014 / TC-003 — a backup restored onto a fresh install reproduces the
// sections, apps, icons, offers, settings and kids; the matched adult's
// favorites, order and folded sections come back. Exporting the result gives
// back the file (minus the adult with no account).
func TestRestore_OntoAFreshInstall(t *testing.T) {
	d := decodeFixture(t)
	st := freshState()
	p := restorePlan(t, st, d, transfer.ModeMerge)
	assert.Equal(t, transfer.CreateCounts{Sections: 3, Apps: 5, Icons: 3, Offers: 2, Kids: 1}, p.Summary.Create)
	assert.Equal(t, transfer.UpdateCounts{People: 1, Settings: 1}, p.Summary.Update)
	assert.Equal(t, transfer.DeleteCounts{}, p.Summary.Delete)
	assert.Equal(t, 1, p.Summary.Skipped)
	sam := item(t, p, transfer.KindAdult, "sam@example.com")
	assert.Equal(t, transfer.ReasonAdultNoAccount, sam.Reason)
	maya := item(t, p, transfer.KindKid, "Maya")
	assert.Equal(t, transfer.ActionCreate, maya.Action)
	assert.Equal(t, []string{transfer.WarnKidNeedsDevice}, maya.Warnings)
	alex := item(t, p, transfer.KindAdult, "alex@example.com")
	assert.Equal(t, transfer.ActionUpdate, alex.Action)
	assert.ElementsMatch(t, []string{"displayName", "prefs", "favorites", "order", "collapsedSections"}, alex.Fields)

	after := apply(t, newMem(st), p)
	out := transfer.BuildFile(after, true, "17.1.0", exportTime)
	want := d.File
	assert.Equal(t, want.Settings, out.Settings)
	assert.Equal(t, want.Sections, out.Sections)
	assert.Equal(t, want.Apps, out.Apps, "every field and every icon")
	assert.Equal(t, want.Library, out.Library)
	assert.Equal(t, want.People.Kids, out.People.Kids)
	gotAlex := *outAdult(t, out, "alex@example.com")
	gotAlex.Email = "alex@example.com"
	assert.Equal(t, want.People.Adults[0], gotAlex)
	assert.Nil(t, outAdultOrNil(out, "sam@example.com"), "restore never creates an adult")

	g := appBySlug(after, "grafana")
	require.NotNil(t, g.SourceLibraryID, "re-linked to the offer with the same name and URL")
	for _, o := range after.Offers {
		if o.Name == "Grafana" {
			assert.Equal(t, o.ID, *g.SourceLibraryID)
		}
	}
	assert.Nil(t, appBySlug(after, "jellyfin").SourceLibraryID, "no offer has its name and URL")
}

func outAdultOrNil(f *transfer.File, email string) *transfer.Adult {
	for i := range f.People.Adults {
		if strings.EqualFold(f.People.Adults[i].Email, email) {
			return &f.People.Adults[i]
		}
	}
	return nil
}

func outAdult(t *testing.T, f *transfer.File, email string) *transfer.Adult {
	t.Helper()
	a := outAdultOrNil(f, email)
	require.NotNil(t, a, email)
	return a
}

// AC-013 / TC-004 — restoring the same file twice in Merge mode leaves the
// same state, and the second preview has nothing to change.
func TestRestore_TwiceIsIdempotent(t *testing.T) {
	d := decodeFixture(t)
	st := freshState()
	m := newMem(st)
	apply(t, m, restorePlan(t, st, d, transfer.ModeMerge))
	snap1, _ := json.Marshal(m.state())

	again := restorePlan(t, m.state(), d, transfer.ModeMerge)
	assert.True(t, zeroChanges(again.Summary), "%+v", again.Summary)
	assert.Greater(t, again.Summary.Unchanged, 10)
	assert.Equal(t, 1, again.Summary.Skipped, "sam still has no account")
	apply(t, m, again)
	snap2, _ := json.Marshal(m.state())
	assert.JSONEq(t, string(snap1), string(snap2))
}

// AC-010 — Merge adds new items, updates matches (sections by name, apps by
// slug, offers by name and URL), and leaves everything else alone.
func TestRestore_MergeUpdatesMatchesAndLeavesTheRest(t *testing.T) {
	d := decodeFixture(t)
	st := freshState()
	old, media := "sec-old", "sec-media"
	st.Sections = []storage.TransferSection{
		{ID: old, Name: "Old", SortIndex: 0, GridWidth: 4},
		{ID: media, Name: "Media", SortIndex: 5, GridWidth: 3},
	}
	st.Apps = []storage.TransferApp{
		{ID: "app-j", Slug: "jellyfin", Name: "Jelly", Description: "Movies and TV", URL: "https://jellyfin.example.com",
			Icon: "https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons/png/jellyfin.png", GatusKey: "media_jellyfin",
			ClickAction: "new_tab", SectionID: &old},
		{ID: "app-old", Slug: "old-thing", Name: "Old thing", URL: "https://old.example.com", ClickAction: "new_tab", SectionID: &old},
	}
	st.Offers = []storage.TransferOffer{
		{ID: "off-legacy", Name: "Legacy", URL: "https://legacy.example.com", SortIndex: 0},
		{ID: "off-immich", Name: "Immich", URL: "https://immich.app", Description: "old words", SortIndex: 1},
	}
	before := struct {
		old    storage.TransferApp
		oldSec storage.TransferSection
		offer  storage.TransferOffer
	}{st.Apps[1], st.Sections[0], st.Offers[0]}

	p := restorePlan(t, st, d, transfer.ModeMerge)
	j := item(t, p, transfer.KindApp, "Jellyfin")
	assert.Equal(t, transfer.ActionUpdate, j.Action)
	assert.ElementsMatch(t, []string{"name", "section"}, j.Fields)
	mediaItem := item(t, p, transfer.KindSection, "Media")
	assert.Equal(t, transfer.ActionUpdate, mediaItem.Action)
	assert.Equal(t, []string{"gridWidth"}, mediaItem.Fields)
	imm := item(t, p, transfer.KindOffer, "Immich")
	assert.Equal(t, transfer.ActionUpdate, imm.Action)
	assert.Contains(t, imm.Fields, "description")
	assert.Empty(t, items(p, "", transfer.ActionDelete), "Merge never deletes")
	for _, it := range p.Items {
		assert.NotEqual(t, "Old thing", it.Name)
		assert.NotEqual(t, "Legacy", it.Name)
		assert.NotEqual(t, "Old", it.Name)
	}

	after := apply(t, newMem(st), p)
	assert.Equal(t, before.old, *appBySlug(after, "old-thing"))
	assert.Equal(t, before.oldSec, after.Sections[0])
	assert.Equal(t, before.offer, after.Offers[0])
	jj := appBySlug(after, "jellyfin")
	assert.Equal(t, "Jellyfin", jj.Name)
	assert.Equal(t, media, *jj.SectionID)
	assert.Equal(t, "app-j", jj.ID, "updated in place, not re-created")
	assert.Equal(t, []string{"Old", "Media", "Network", "Tools"}, sectionNames(after))
	assert.Equal(t, 6, after.Sections[1].GridWidth)
}

// §3 "Order is the array order" under Merge: matched sections take the file's
// order among the slots they already hold; others keep their slot; new ones
// go last.
func TestRestore_MergeSectionOrder(t *testing.T) {
	st := freshState()
	st.Sections = []storage.TransferSection{
		{ID: "s-a", Name: "A", SortIndex: 0, GridWidth: 6},
		{ID: "s-x", Name: "X", SortIndex: 1, GridWidth: 6},
		{ID: "s-b", Name: "B", SortIndex: 2, GridWidth: 6},
	}
	f := decodeFixture(t).File
	f.Sections = []transfer.Section{{Name: "B", GridWidth: 6}, {Name: "A", GridWidth: 6}, {Name: "C", GridWidth: 6}}
	f.Apps, f.Library, f.People = nil, nil, nil
	f.IncludesPeople = false
	p := restorePlan(t, st, decodeFile(t, f), transfer.ModeMerge)
	assert.Equal(t, []string{"order"}, item(t, p, transfer.KindSection, "B").Fields)
	after := apply(t, newMem(st), p)
	assert.Equal(t, []string{"B", "X", "A", "C"}, sectionNames(after))
	again := restorePlan(t, after, decodeFile(t, f), transfer.ModeMerge)
	assert.True(t, zeroChanges(again.Summary), "%+v", again.Summary)
}

// AC-011 / TC-005 — Replace deletes every app, section and offer, lists the
// deletions (first), and then loads the file.
func TestRestore_ReplaceDeletesEverythingFirst(t *testing.T) {
	d := decodeFixture(t)
	st := freshState()
	old := "sec-old"
	st.Sections = []storage.TransferSection{{ID: old, Name: "Infra", SortIndex: 0, GridWidth: 4}, {ID: "sec-m", Name: "Media", SortIndex: 1, GridWidth: 6}}
	st.Apps = []storage.TransferApp{
		{ID: "app-old", Slug: "old-thing", Name: "Old thing", URL: "https://old.example.com", ClickAction: "new_tab", SectionID: &old},
		{ID: "app-j", Slug: "jellyfin", Name: "Jellyfin", URL: "https://jellyfin.example.com", ClickAction: "new_tab"},
	}
	st.Offers = []storage.TransferOffer{{ID: "off-legacy", Name: "Legacy", URL: "https://legacy.example.com"}}
	st.People[1].Favorites = []string{"app-old", "app-j"}

	p := restorePlan(t, st, d, transfer.ModeReplace)
	assert.Equal(t, transfer.DeleteCounts{Sections: 2, Apps: 2, Offers: 1}, p.Summary.Delete)
	assert.Equal(t, transfer.CreateCounts{Sections: 3, Apps: 5, Icons: 3, Offers: 2, Kids: 1}, p.Summary.Create)
	dels := items(p, "", transfer.ActionDelete)
	assert.ElementsMatch(t, []string{"Infra", "Media", "Old thing", "Jellyfin", "Legacy"}, names(dels))
	for i := range dels {
		assert.Equal(t, transfer.ActionDelete, p.Items[i].Action, "deletions come first")
	}
	assert.Equal(t, strp("Infra"), item(t, p, transfer.KindApp, "Old thing").Section)

	after := apply(t, newMem(st), p)
	assert.Nil(t, appBySlug(after, "old-thing"))
	assert.Equal(t, []string{"Media", "Network", "Tools"}, sectionNames(after))
	require.Len(t, after.Offers, 2)
	for _, slug := range []string{"grafana", "jellyfin", "router", "sonarr", "unifi"} {
		assert.NotNil(t, appBySlug(after, slug), slug)
	}
	alex := personByEmail(after, "alex@example.com")
	assert.Len(t, alex.Favorites, 1, "the deleted apps' favorites went with them; the file's came back")
	assert.Equal(t, "jellyfin", appByID(after, alex.Favorites[0]).Slug)

	again := restorePlan(t, after, d, transfer.ModeMerge)
	assert.True(t, zeroChanges(again.Summary), "%+v", again.Summary)
}

// AC-012 — adults are never created or deleted; kids match by name
// (case-insensitive) or are created; favorites are added, order and folded
// sections replaced; a reference to something missing from the file is
// dropped with a warning.
func TestRestore_People(t *testing.T) {
	st := freshState()
	st.Apps = []storage.TransferApp{{ID: "app-keep", Slug: "keep", Name: "Keep", URL: "https://keep.example.com", ClickAction: "new_tab"}}
	st.People[1].Favorites = []string{"app-keep"}
	st.People[1].Order = []string{"app-keep"}
	st.People = append(st.People, storage.TransferPerson{ID: "kid-m", Role: "kid", DisplayName: "maya", AvatarColor: "red",
		ThemePref: "system", DensityPref: "large", ShowUptimeDisplay: true, Allowed: []string{"app-keep"}, Favorites: []string{}},
		storage.TransferPerson{ID: "kid-other", Role: "kid", DisplayName: "Leo", AvatarColor: "blue", ThemePref: "system", DensityPref: "large"})

	f := decodeFixture(t).File
	f.People.Adults[0].Favorites = []string{"jellyfin", "ghost"}
	f.People.Adults[0].CollapsedSections = []string{"Network", "Nowhere"}
	d := decodeFile(t, f)
	p := restorePlan(t, st, d, transfer.ModeMerge)
	assert.Equal(t, 0, p.Summary.Create.Kids, "maya matches Maya")
	assert.Equal(t, transfer.ActionUpdate, item(t, p, transfer.KindKid, "Maya").Action)
	assert.Contains(t, p.Warnings, transfer.PlanWarning{Code: transfer.WarnRefMissing, Count: 2})
	assert.Contains(t, item(t, p, transfer.KindAdult, "alex@example.com").Warnings, transfer.WarnRefMissing)
	for _, it := range p.Items {
		assert.False(t, it.Kind == transfer.KindAdult && it.Action == transfer.ActionCreate, "no adult is ever created")
		assert.NotEqual(t, "Leo", it.Name, "a kid not in the file is left alone")
	}

	m := newMem(st)
	after := apply(t, m, p)
	for _, c := range m.calls {
		assert.NotContains(t, c, "kid-other")
	}
	alex := personByEmail(after, "alex@example.com")
	var favs []string
	for _, id := range alex.Favorites {
		favs = append(favs, appByID(after, id).Slug)
	}
	assert.ElementsMatch(t, []string{"keep", "jellyfin"}, favs, "favorites are added to")
	var order []string
	for _, id := range alex.Order {
		order = append(order, appByID(after, id).Slug)
	}
	assert.Equal(t, []string{"jellyfin", "sonarr", "router"}, order, "order is replaced by the file's")
	assert.Equal(t, "dark", alex.ThemePref)
	assert.Equal(t, "Alex", alex.DisplayName)
	maya := after.People[2]
	assert.Equal(t, "maya", maya.DisplayName, "a matched kid keeps its name")
	assert.Equal(t, "teal", maya.AvatarColor)
	var allowed []string
	for _, id := range maya.Allowed {
		allowed = append(allowed, appByID(after, id).Slug)
	}
	assert.ElementsMatch(t, []string{"jellyfin", "sonarr"}, allowed)
	for _, x := range after.People {
		assert.NotEqual(t, "sam@example.com", x.Email)
	}
}

// AC-015 — icon bytes are checked against their sha256 and the upload rules.
// A bad icon skips that icon with a warning; the app still restores.
func TestRestore_IconChecks(t *testing.T) {
	good := pngData(t, 32, 32)
	badHash := *good
	badHash.SHA256 = strings.Repeat("0", 64)
	notPNG := &transfer.IconData{PNG: base64.StdEncoding.EncodeToString([]byte("GIF89a not a png")), SHA256: sha(t, []byte("GIF89a not a png"))}
	tiny := pngData(t, 8, 8)
	huge := pngData(t, 600, 600)
	notB64 := &transfer.IconData{PNG: "!!!", SHA256: good.SHA256}
	cases := map[string]*transfer.IconData{"bad hash": &badHash, "not a PNG": notPNG, "too small": tiny, "too wide": huge, "not base64": notB64}
	for name, ic := range cases {
		t.Run(name, func(t *testing.T) {
			f := decodeFixture(t).File
			f.Apps[2].UploadedIcons = transfer.UploadedIcons{Light: ic, Dark: good}
			st := freshState()
			p := restorePlan(t, st, decodeFile(t, f), transfer.ModeMerge)
			var light, dark *transfer.Item
			for i, it := range items(p, transfer.KindIcon, "") {
				if it.Name == "Router" && it.Variant == "light" {
					light = &items(p, transfer.KindIcon, "")[i]
				}
				if it.Name == "Router" && it.Variant == "dark" {
					dark = &items(p, transfer.KindIcon, "")[i]
				}
			}
			require.NotNil(t, light)
			require.NotNil(t, dark)
			assert.Equal(t, transfer.ActionSkip, light.Action)
			assert.Equal(t, transfer.ReasonIconInvalid, light.Reason)
			assert.Equal(t, transfer.ActionCreate, dark.Action)
			r := item(t, p, transfer.KindApp, "Router")
			assert.Equal(t, transfer.ActionCreate, r.Action)
			assert.Contains(t, r.Warnings, transfer.WarnIconInvalid)
			after := apply(t, newMem(st), p)
			icons := appBySlug(after, "router").Icons
			require.Len(t, icons, 1)
			assert.Equal(t, "dark", icons[0].Variant)
			assert.Equal(t, good.SHA256, icons[0].ETag)
		})
	}
	big := bytes.Repeat([]byte{0}, 256*1024+1)
	copy(big, []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A})
	f := decodeFixture(t).File
	f.Apps[2].UploadedIcons = transfer.UploadedIcons{Light: &transfer.IconData{PNG: base64.StdEncoding.EncodeToString(big), SHA256: sha(t, big)}}
	p := restorePlan(t, freshState(), decodeFile(t, f), transfer.ModeMerge)
	assert.Equal(t, transfer.ReasonIconInvalid, items(p, transfer.KindIcon, transfer.ActionSkip)[0].Reason, "over 256 KB")
}

func sha(t *testing.T, b []byte) string {
	t.Helper()
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// AC-024 — every url and icon written by restore is http(s) or empty. A bad
// app or offer link skips that item. A bad icon value is dropped (the app
// keeps its initials badge), with a warning.
func TestRestore_URLAllowList(t *testing.T) {
	f := decodeFixture(t).File
	f.Apps[0].URL = "javascript:alert(1)"
	f.Apps[1].URL = "  "
	f.Apps[2].Icon = "ftp://files.home.arpa/router.png"
	f.Apps[3].URL = "HTTPS://Sonarr.Home.Arpa"
	f.Library[0].URL = "data:text/html,hi"
	f.Library[1].Icon = "file:///etc/passwd"
	st := freshState()
	p := restorePlan(t, st, decodeFile(t, f), transfer.ModeMerge)
	assert.Equal(t, transfer.ReasonBadScheme, item(t, p, transfer.KindApp, "Grafana").Reason)
	assert.Equal(t, transfer.ReasonNoLink, item(t, p, transfer.KindApp, "Jellyfin").Reason)
	r := item(t, p, transfer.KindApp, "Router")
	assert.Equal(t, transfer.ActionCreate, r.Action)
	assert.Equal(t, []string{transfer.WarnIconUnsupported}, r.Warnings)
	assert.Equal(t, transfer.ActionCreate, item(t, p, transfer.KindApp, "Sonarr").Action)
	assert.Equal(t, transfer.ReasonBadScheme, item(t, p, transfer.KindOffer, "Immich").Reason)
	assert.Equal(t, []string{transfer.WarnIconUnsupported}, item(t, p, transfer.KindOffer, "Grafana").Warnings)

	after := apply(t, newMem(st), p)
	for _, a := range after.Apps {
		assert.True(t, transfer.IsHTTPURL(a.URL), a.URL)
		assert.True(t, a.Icon == "" || transfer.IsHTTPURL(a.Icon), a.Icon)
	}
	for _, o := range after.Offers {
		assert.True(t, transfer.IsHTTPURL(o.URL), o.URL)
		assert.True(t, o.Icon == "" || transfer.IsHTTPURL(o.Icon), o.Icon)
	}
	assert.Nil(t, appBySlug(after, "grafana"))
}

// AC-007 — items the planner can't use are skipped, each with a reason.
func TestRestore_InvalidItemsAreSkippedWithAReason(t *testing.T) {
	f := decodeFixture(t).File
	f.Sections = append(f.Sections, transfer.Section{Name: "Wide", GridWidth: 5}, transfer.Section{Name: "Media", GridWidth: 6}, transfer.Section{Name: " ", GridWidth: 6})
	f.Apps = append(f.Apps,
		transfer.App{Slug: "jellyfin", Name: "Again", URL: "https://again.example.com", ClickAction: "new_tab"},
		transfer.App{Slug: "nul", Name: "Bad\x00name", URL: "https://nul.example.com", ClickAction: "new_tab"},
		transfer.App{Slug: "click", Name: "Click", URL: "https://click.example.com", ClickAction: "popup"},
		transfer.App{Slug: "", Name: "No slug", URL: "https://noslug.example.com", ClickAction: "new_tab"},
		transfer.App{Slug: "lost", Name: "Lost", URL: "https://lost.example.com", ClickAction: "", Section: strp("Nowhere")},
	)
	f.People.Kids = append(f.People.Kids, transfer.Kid{Name: strings.Repeat("k", 41)})
	f.People.Adults[0].Prefs.Theme = strp("neon")
	big := 999999
	f.Settings.StatusDegradedMs = &big
	st := freshState()
	p := restorePlan(t, st, decodeFile(t, f), transfer.ModeMerge)

	reasons := map[string]string{}
	for _, it := range items(p, "", transfer.ActionSkip) {
		reasons[it.Kind+":"+it.Name] = it.Reason
	}
	assert.Equal(t, transfer.ReasonInvalidItem, reasons["section:Wide"])
	assert.Equal(t, transfer.ReasonDuplicate, reasons["section:Media"])
	assert.Equal(t, transfer.ReasonNoName, reasons["section: "])
	assert.Equal(t, transfer.ReasonDuplicate, reasons["app:Again"])
	assert.Equal(t, transfer.ReasonInvalidItem, reasons["app:Bad\x00name"])
	assert.Equal(t, transfer.ReasonInvalidItem, reasons["app:Click"])
	assert.Equal(t, transfer.ReasonInvalidItem, reasons["app:No slug"])
	assert.Equal(t, transfer.ReasonInvalidItem, reasons["kid:"+strings.Repeat("k", 41)])
	assert.Equal(t, transfer.ReasonInvalidItem, reasons["adult:alex@example.com"])
	assert.Equal(t, transfer.ReasonInvalidItem, reasons["settings:System settings"])
	lost := item(t, p, transfer.KindApp, "Lost")
	assert.Equal(t, transfer.ActionCreate, lost.Action)
	assert.Equal(t, []string{transfer.WarnSectionMissing}, lost.Warnings)
	assert.Nil(t, lost.Section, "it lands in Uncategorized")

	after := apply(t, newMem(st), p)
	assert.Equal(t, "new_tab", appBySlug(after, "lost").ClickAction, "an empty clickAction is new_tab")
	assert.Nil(t, appBySlug(after, "lost").SectionID)
	assert.Equal(t, "Jellyfin", appBySlug(after, "jellyfin").Name)
	assert.Nil(t, after.Settings.StatusDegradedMs, "invalid settings are left alone")
}

// AC-007 — a preview changes nothing: planning only reads the state.
func TestRestore_PlanningDoesNotTouchTheState(t *testing.T) {
	st := populatedState()
	before, _ := json.Marshal(st)
	transfer.PlanRestore(st, decodeFixture(t), transfer.ModeReplace)
	transfer.PlanRestore(st, decodeFixture(t), transfer.ModeMerge)
	transfer.PlanImport(st, parse(t, fixtureInput(t)), nil)
	after, _ := json.Marshal(st)
	assert.JSONEq(t, string(before), string(after))
}

// AC-008 (pure half) — TC-006: an app added with a slug in the file moves the
// plan from create to update, so the hash changes.
func TestRestore_PlanHashTracksTheCatalog(t *testing.T) {
	d := decodeFixture(t)
	a := restorePlan(t, freshState(), d, transfer.ModeMerge)
	assert.Equal(t, a.PlanHash, restorePlan(t, freshState(), d, transfer.ModeMerge).PlanHash)
	assert.NotEqual(t, a.PlanHash, restorePlan(t, freshState(), d, transfer.ModeReplace).PlanHash)
	st := freshState()
	st.Apps = []storage.TransferApp{{ID: "app-x", Slug: "sonarr", Name: "Sonarr", URL: "https://elsewhere.example.com", ClickAction: "new_tab"}}
	assert.NotEqual(t, a.PlanHash, restorePlan(t, st, d, transfer.ModeMerge).PlanHash)
}
