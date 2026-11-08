package transfer_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport/schemacheck"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/transfer"
)

// SPEC-backup-import §2.1, §2.3, §3 — the backup file.

var exportTime = time.Date(2026, 11, 7, 18, 4, 11, 0, time.UTC)

var (
	etagA = strings.Repeat("a", 64)
	etagB = strings.Repeat("b", 64)
)

// populatedState is a catalog with every field set, uploaded icons, offers,
// settings, adults and a kid. Every id is a UUID, so a test can grep the file
// for ids.
func populatedState() *storage.TransferState {
	media := "0b7f6c0e-1111-4c1e-9d1a-000000000001"
	network := "0b7f6c0e-1111-4c1e-9d1a-000000000002"
	offer := "0b7f6c0e-3333-4c1e-9d1a-000000000001"
	jelly, router, sonarr := "0b7f6c0e-2222-4c1e-9d1a-000000000001", "0b7f6c0e-2222-4c1e-9d1a-000000000002", "0b7f6c0e-2222-4c1e-9d1a-000000000003"
	degraded := 1500
	return &storage.TransferState{
		OwnerID:  "0b7f6c0e-0000-4c1e-9d1a-00000000000a",
		CallerID: "0b7f6c0e-0000-4c1e-9d1a-00000000000a",
		Sections: []storage.TransferSection{
			{ID: media, Name: "Media", SortIndex: 0, GridWidth: 6},
			{ID: network, Name: "Network", SortIndex: 1, GridWidth: 3},
		},
		Apps: []storage.TransferApp{
			{ID: jelly, Slug: "jellyfin", Name: "Jellyfin", Description: "Movies and TV", URL: "https://jellyfin.example.com",
				Icon: "https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons/png/jellyfin.png", GatusKey: "media_jellyfin",
				ClickAction: "new_tab", SectionID: &media, SourceLibraryID: &offer,
				Icons: []storage.TransferIcon{{Variant: "light", ETag: etagA, Bytes: []byte("light-png")}, {Variant: "dark", ETag: etagB, Bytes: []byte("dark-png")}}},
			{ID: router, Slug: "router", Name: "Router", URL: "http://192.0.2.1", ClickAction: "same_tab"},
			{ID: sonarr, Slug: "sonarr", Name: "Sonarr", URL: "https://sonarr.home.arpa", ClickAction: "iframe", SectionID: &media},
		},
		Offers: []storage.TransferOffer{
			{ID: offer, Name: "Jellyfin", URL: "https://jellyfin.example.com", Description: "Movies", SuggestedCategory: "Media", SortIndex: 0},
			{ID: "0b7f6c0e-3333-4c1e-9d1a-000000000002", Name: "Immich", URL: "https://immich.app", Icon: "https://immich.app/i.png", SortIndex: 1},
		},
		Settings: storage.SystemSettings{ShowUptimeDisplay: false, StatusDegradedMs: &degraded},
		People: []storage.TransferPerson{
			{ID: "0b7f6c0e-0000-4c1e-9d1a-00000000000a", Role: "admin", Email: "admin@example.com", ThemePref: "dark", DensityPref: "compact",
				ShowHealthBar: true, ShowUptimeDisplay: true, Favorites: []string{sonarr, jelly}, Order: []string{sonarr, jelly, router}, Collapsed: []string{network}},
			{ID: "0b7f6c0e-0000-4c1e-9d1a-00000000000b", Role: "user", Email: "alex@example.com", DisplayName: "Alex", AvatarColor: "green",
				ThemePref: "light", DensityPref: "list", ShowUptimeDisplay: true},
			{ID: "0b7f6c0e-0000-4c1e-9d1a-00000000000c", Role: "kid", DisplayName: "Maya", AvatarColor: "teal", ThemePref: "light",
				DensityPref: "large", ShowUptimeDisplay: true, Allowed: []string{sonarr, jelly}, Favorites: []string{jelly}},
		},
	}
}

func schemaBytes(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "schema", "homepad-backup-v1.json"))
	require.NoError(t, err)
	return b
}

// AC-001 — the file matches docs/schema/homepad-backup-v1.json, with and
// without people. The fixture matches it too.
func TestBackup_MatchesJSONSchema(t *testing.T) {
	schema := schemaBytes(t)
	for _, people := range []bool{true, false} {
		out := transfer.Encode(transfer.BuildFile(populatedState(), people, "17.1.0", exportTime))
		require.NotEmpty(t, out)
		assert.Empty(t, schemacheck.Validate(schema, out), "people=%v", people)
	}
	assert.Empty(t, schemacheck.Validate(schema, readFixture(t, "backups/v1.json")))
}

// The checker can fail: a file with an extra key, a wrong width and a missing
// key is refused (negative control for AC-001).
func TestBackup_SchemaCheckerRejectsBadFiles(t *testing.T) {
	bad := `{"format":"homepad-backup","schemaVersion":1,"exportedAt":"2026-11-07T18:04:11Z","homepadVersion":"x",
	  "includesPeople":false,"settings":{"showUptimeDisplay":true,"statusDegradedMs":null},
	  "sections":[{"name":"A","gridWidth":5}],"apps":[],"library":[],"passwordHash":"x"}`
	errs := schemacheck.Validate(schemaBytes(t), []byte(bad))
	assert.Len(t, errs, 2, "%v", errs)
}

// AC-002 — every section (order, width), every app with every §3 field,
// every icon variant, every offer in order, and System settings.
func TestBackup_HoldsTheWholeCatalog(t *testing.T) {
	f := transfer.BuildFile(populatedState(), true, "17.1.0", exportTime)
	require.NotNil(t, f)
	assert.Equal(t, "homepad-backup", f.Format)
	assert.Equal(t, 1, f.SchemaVersion)
	assert.Equal(t, "2026-11-07T18:04:11Z", f.ExportedAt)
	assert.Equal(t, "17.1.0", f.HomepadVersion)
	assert.Equal(t, []transfer.Section{{Name: "Media", GridWidth: 6}, {Name: "Network", GridWidth: 3}}, f.Sections)

	require.Len(t, f.Apps, 3)
	assert.Equal(t, []string{"jellyfin", "router", "sonarr"}, []string{f.Apps[0].Slug, f.Apps[1].Slug, f.Apps[2].Slug}, "apps by slug")
	j := f.Apps[0]
	assert.Equal(t, "Jellyfin", j.Name)
	assert.Equal(t, "Movies and TV", j.Description)
	assert.Equal(t, "https://jellyfin.example.com", j.URL)
	assert.Equal(t, "https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons/png/jellyfin.png", j.Icon)
	assert.Equal(t, strp("Media"), j.Section)
	assert.Equal(t, "new_tab", j.ClickAction)
	assert.Equal(t, strp("media_jellyfin"), j.GatusKey)
	require.NotNil(t, j.UploadedIcons.Light)
	require.NotNil(t, j.UploadedIcons.Dark)
	assert.Equal(t, "bGlnaHQtcG5n", j.UploadedIcons.Light.PNG, "base64 of the bytes")
	assert.Equal(t, etagA, j.UploadedIcons.Light.SHA256)
	assert.Equal(t, etagB, j.UploadedIcons.Dark.SHA256)
	r := f.Apps[1]
	assert.Nil(t, r.Section, "Uncategorized is null")
	assert.Nil(t, r.GatusKey, "unmonitored is null")
	assert.Equal(t, transfer.UploadedIcons{}, r.UploadedIcons)

	assert.Equal(t, []transfer.Offer{
		{Name: "Jellyfin", URL: "https://jellyfin.example.com", Description: "Movies", SuggestedCategory: "Media"},
		{Name: "Immich", URL: "https://immich.app", Icon: "https://immich.app/i.png"},
	}, f.Library)
	assert.False(t, f.Settings.ShowUptimeDisplay)
	require.NotNil(t, f.Settings.StatusDegradedMs)
	assert.Equal(t, 1500, *f.Settings.StatusDegradedMs)
}

// AC-003 — with people: adults by email, kids by name, prefs, favorites,
// order, folded sections, allowed apps; references are slugs and names.
func TestBackup_IncludesPeople(t *testing.T) {
	f := transfer.BuildFile(populatedState(), true, "17.1.0", exportTime)
	require.NotNil(t, f.People)
	assert.True(t, f.IncludesPeople)
	require.Len(t, f.People.Adults, 2)
	a := f.People.Adults[0]
	assert.Equal(t, "admin@example.com", a.Email)
	assert.Nil(t, a.DisplayName)
	assert.Nil(t, a.AvatarColor)
	assert.Equal(t, transfer.Prefs{Theme: strp("dark"), Density: strp("compact"), ShowHealthBar: boolp(true), ShowUptimeDisplay: boolp(true)}, a.Prefs)
	assert.Equal(t, []string{"jellyfin", "sonarr"}, a.Favorites, "favorites by slug, sorted")
	assert.Equal(t, []string{"sonarr", "jellyfin", "router"}, a.Order, "order kept")
	assert.Equal(t, []string{"Network"}, a.CollapsedSections)
	assert.Equal(t, "alex@example.com", f.People.Adults[1].Email)
	assert.Equal(t, strp("Alex"), f.People.Adults[1].DisplayName)
	assert.Equal(t, strp("green"), f.People.Adults[1].AvatarColor)

	require.Len(t, f.People.Kids, 1)
	k := f.People.Kids[0]
	assert.Equal(t, "Maya", k.Name)
	assert.Equal(t, strp("teal"), k.AvatarColor)
	assert.Equal(t, []string{"jellyfin", "sonarr"}, k.AllowedApps)
	assert.Equal(t, []string{"jellyfin"}, k.Favorites)
}

func boolp(b bool) *bool { return &b }

// AC-003 / TC-002 — without people: no `people` key and no "@" anywhere.
func TestBackup_WithoutPeopleHasNoPeopleKeyAndNoEmail(t *testing.T) {
	out := transfer.Encode(transfer.BuildFile(populatedState(), false, "17.1.0", exportTime))
	var top map[string]any
	require.NoError(t, json.Unmarshal(out, &top))
	assert.NotContains(t, top, "people")
	assert.Equal(t, false, top["includesPeople"])
	assert.NotContains(t, string(out), "@")
}

// AC-004 (pure half) — no database id of any kind, and no source_library_id.
// The DB-backed half greps a real export for hashes, tokens and secrets.
func TestBackup_HasNoIDs(t *testing.T) {
	st := populatedState()
	out := string(transfer.Encode(transfer.BuildFile(st, true, "17.1.0", exportTime)))
	require.Contains(t, out, `"slug": "jellyfin"`, "the file has the catalog")
	assert.NotContains(t, out, "0b7f6c0e", "no id of any row")
	for _, k := range []string{"sourceLibraryId", "source_library_id", `"id"`} {
		assert.NotContains(t, out, k)
	}
}

// AC-005 — two backups of the same dashboard are byte-identical apart from
// exportedAt.
func TestBackup_Deterministic(t *testing.T) {
	a := transfer.Encode(transfer.BuildFile(populatedState(), true, "17.1.0", exportTime))
	b := transfer.Encode(transfer.BuildFile(populatedState(), true, "17.1.0", exportTime.Add(26*time.Hour)))
	al, bl := strings.Split(string(a), "\n"), strings.Split(string(b), "\n")
	require.Equal(t, len(al), len(bl))
	var diff []string
	for i := range al {
		if al[i] != bl[i] {
			diff = append(diff, al[i])
		}
	}
	assert.Equal(t, []string{`  "exportedAt": "2026-11-07T18:04:11Z",`}, diff)
}

// §3 — canonical form: two-space indent, fixed key order, no HTML escaping.
// The fixture is in canonical form, so decoding and re-encoding it gives the
// same bytes.
func TestBackup_EncodeIsCanonical(t *testing.T) {
	raw := readFixture(t, "backups/v1.json")
	d := decodeFixture(t)
	assert.Equal(t, string(raw), string(transfer.Encode(d.File)))

	f := transfer.BuildFile(populatedState(), false, "17.1.0", exportTime)
	f.Apps[0].Description = "Movies & <TV>"
	out := transfer.Encode(f)
	assert.Contains(t, string(out), `"description": "Movies & <TV>"`)
	assert.True(t, bytes.HasPrefix(out, []byte("{\n  \"format\": \"homepad-backup\",\n  \"schemaVersion\": 1,\n")))
}

// AC-016 — format and version checks.
func TestDecode_RefusesNonBackupsAndNewerFiles(t *testing.T) {
	cases := []struct {
		name, body, code string
		status           int
	}{
		{"other format", `{"format":"homarr","schemaVersion":1}`, transfer.CodeNotABackup, 422},
		{"no format", `{"schemaVersion":1,"apps":[]}`, transfer.CodeNotABackup, 422},
		{"not an object", `[1,2,3]`, transfer.CodeNotABackup, 422},
		{"version zero", `{"format":"homepad-backup","schemaVersion":0}`, transfer.CodeNotABackup, 422},
		{"version not a number", `{"format":"homepad-backup","schemaVersion":"1"}`, transfer.CodeNotABackup, 422},
		{"newer", `{"format":"homepad-backup","schemaVersion":99}`, transfer.CodeSchemaNewer, 422},
		{"wrong shape", `{"format":"homepad-backup","schemaVersion":1,"apps":"none"}`, transfer.CodeNotABackup, 422},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := transfer.DecodeBackup([]byte(c.body))
			assert.Nil(t, d)
			require.NotNil(t, err)
			assert.Equal(t, c.code, err.Code)
			assert.Equal(t, c.status, err.Status)
		})
	}
	_, err := transfer.DecodeBackup([]byte(`{"format":"homepad-backup","schemaVersion":99}`))
	require.NotNil(t, err)
	assert.Equal(t, "This backup is from a newer Homepad. Update Homepad, then try again.", err.Message)
}

// AC-028 — malformed JSON is a 400 with the line.
func TestDecode_MalformedJSONIs400WithLine(t *testing.T) {
	body := "{\n  \"format\": \"homepad-backup\",\n  \"schemaVersion\": 1,\n  \"apps\": [,]\n}\n"
	_, err := transfer.DecodeBackup([]byte(body))
	require.NotNil(t, err)
	assert.Equal(t, http.StatusBadRequest, err.Status)
	assert.Equal(t, transfer.CodeInvalidJSON, err.Code)
	assert.Equal(t, 4, err.Line)
}

// AC-018 — unknown fields in a known version are ignored and counted.
func TestDecode_CountsUnknownFields(t *testing.T) {
	var m map[string]any
	require.NoError(t, json.Unmarshal(readFixture(t, "backups/v1.json"), &m))
	m["theme"] = "dark"
	m["apps"].([]any)[0].(map[string]any)["tags"] = []any{"x"}
	m["people"].(map[string]any)["kids"].([]any)[0].(map[string]any)["prefs"].(map[string]any)["sound"] = true
	b, _ := json.Marshal(m)
	d, err := transfer.DecodeBackup(b)
	require.Nil(t, err)
	assert.Equal(t, 3, d.UnknownFields)

	p := transfer.PlanRestore(freshState(), d, transfer.ModeMerge)
	assert.Equal(t, 3, p.Ignored.Fields)
	assert.Contains(t, p.Warnings, transfer.PlanWarning{Code: transfer.WarnUnknownFields, Count: 3})

	clean := decodeFixture(t)
	assert.Equal(t, 0, clean.UnknownFields)
}

// AC-026 — over a count limit is a 422 carrying the count and the limit.
func TestDecode_CountLimits(t *testing.T) {
	mk := func(key string, n int, item func(i int) any) []byte {
		var m map[string]any
		require.NoError(t, json.Unmarshal(readFixture(t, "backups/v1.json"), &m))
		list := make([]any, n)
		for i := range list {
			list[i] = item(i)
		}
		if key == "kids" {
			m["people"].(map[string]any)["kids"] = list
		} else {
			m[key] = list
		}
		b, _ := json.Marshal(m)
		return b
	}
	cases := []struct {
		key, things string
		limit       int
		item        func(i int) any
	}{
		{"apps", "apps", transfer.MaxApps, func(i int) any {
			return map[string]any{"slug": fmt.Sprint("a", i), "name": "A", "url": "https://a.example.com", "clickAction": "new_tab"}
		}},
		{"sections", "sections", transfer.MaxSections, func(i int) any { return map[string]any{"name": fmt.Sprint("S", i), "gridWidth": 6} }},
		{"library", "offers", transfer.MaxOffers, func(i int) any { return map[string]any{"name": fmt.Sprint("O", i), "url": "https://o.example.com"} }},
		{"kids", "kids", transfer.MaxKids, func(i int) any { return map[string]any{"name": fmt.Sprint("K", i)} }},
	}
	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			_, err := transfer.DecodeBackup(mk(c.key, c.limit, c.item))
			assert.Nil(t, err, "at the limit is fine")
			_, err = transfer.DecodeBackup(mk(c.key, c.limit+1, c.item))
			require.NotNil(t, err)
			assert.Equal(t, http.StatusUnprocessableEntity, err.Status)
			assert.Equal(t, transfer.CodeTooMany, err.Code)
			assert.Equal(t, c.limit+1, err.Count)
			assert.Equal(t, c.limit, err.Limit)
			assert.Equal(t, c.things, err.Things)
		})
	}
}

// AC-017 — every released schema version has a fixture, and every fixture
// decodes (through the upgrade chain) to the current version and restores
// onto a fresh install with nothing skipped but the adult who has no account.
func TestDecode_EveryReleasedSchemaVersionStaysRestorable(t *testing.T) {
	for v := 1; v <= transfer.SchemaVersion; v++ {
		t.Run(fmt.Sprintf("v%d", v), func(t *testing.T) {
			raw := readFixture(t, fmt.Sprintf("backups/v%d.json", v))
			d, err := transfer.DecodeBackup(raw)
			require.Nil(t, err)
			assert.Equal(t, transfer.SchemaVersion, d.File.SchemaVersion, "upgraded to the current version")
			p := transfer.PlanRestore(freshState(), d, transfer.ModeMerge)
			for _, it := range items(p, "", transfer.ActionSkip) {
				assert.Equal(t, transfer.ReasonAdultNoAccount, it.Reason, "%+v", it)
			}
			assert.Greater(t, p.Summary.Create.Apps, 0)
		})
	}
	files, _ := filepath.Glob(filepath.Join("testdata", "backups", "v*.json"))
	assert.Len(t, files, transfer.SchemaVersion, "one fixture per released version, and none for a future one")
}
