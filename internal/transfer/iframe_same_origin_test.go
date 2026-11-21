package transfer_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport/schemacheck"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/transfer"
)

// SPEC-iframe-sandbox-optin AC-011, AC-012 — each app's iframeSameOrigin
// ("Let this app keep its sign-in") is an optional field of the schemaVersion
// 2 file. It doesn't bump the version. A file without it (any v1 file, through
// the 1→2 upgrader) leaves an existing app's value alone under Merge, and a
// new app gets false.

// optInState is populatedState with sonarr opted in.
func optInState() *storage.TransferState {
	st := populatedState()
	for i := range st.Apps {
		if st.Apps[i].Slug == "sonarr" {
			st.Apps[i].IframeSameOrigin = true
		}
	}
	return st
}

// AC-011 — the export writes the field on every app, right after clickAction,
// and the file still matches the v2 schema.
func TestOptIn_ExportCarriesTheField(t *testing.T) {
	f := transfer.BuildFile(optInState(), false, "18.0.0", exportTime)
	assert.Equal(t, 2, f.SchemaVersion, "the field doesn't bump the version")
	got := map[string]*bool{}
	for _, a := range f.Apps {
		got[a.Slug] = a.IframeSameOrigin
	}
	assert.Equal(t, map[string]*bool{"jellyfin": boolp(false), "router": boolp(false), "sonarr": boolp(true)}, got)

	out := transfer.Encode(f)
	assert.Empty(t, schemacheck.Validate(schemaBytes(t), out))
	assert.Contains(t, string(out), "\"clickAction\": \"iframe\",\n      \"iframeSameOrigin\": true,\n", "fixed key order")
}

// AC-011 — the field is known to v2 (not counted as unknown), it's optional
// in the schema, and the wrong type is refused like any other field.
func TestOptIn_DecodeKnowsTheField(t *testing.T) {
	d, perr := transfer.DecodeBackup(transfer.Encode(transfer.BuildFile(optInState(), true, "18.0.0", exportTime)))
	require.Nil(t, perr)
	assert.Zero(t, d.UnknownFields)

	v2 := readFixture(t, "backups/v2.json")
	require.NotContains(t, string(v2), "iframeSameOrigin", "the v2 fixture predates the field")
	assert.Empty(t, schemacheck.Validate(schemaBytes(t), v2), "the field is optional")

	bad := strings.Replace(string(v2), `"clickAction": "iframe",`, `"clickAction": "iframe", "iframeSameOrigin": "yes",`, 1)
	require.NotEqual(t, string(v2), bad)
	_, perr = transfer.DecodeBackup([]byte(bad))
	require.NotNil(t, perr)
	assert.Equal(t, transfer.CodeNotABackup, perr.Code)
	assert.Contains(t, perr.Message, "iframeSameOrigin")
}

// AC-011 — Merge with a file that carries the field: a different value is an
// update of that field; the same value is no change.
func TestOptIn_MergeWithTheField(t *testing.T) {
	file := transfer.BuildFile(optInState(), false, "18.0.0", exportTime)
	st := populatedState() // sonarr off here

	p := restorePlan(t, st, decodeFile(t, file), transfer.ModeMerge)
	s := item(t, p, transfer.KindApp, "Sonarr")
	assert.Equal(t, transfer.ActionUpdate, s.Action)
	assert.Equal(t, []string{"iframeSameOrigin"}, s.Fields)
	after := apply(t, newMem(st), p)
	assert.True(t, appBySlug(after, "sonarr").IframeSameOrigin)
	assert.False(t, appBySlug(after, "jellyfin").IframeSameOrigin)

	again := restorePlan(t, after, decodeFile(t, file), transfer.ModeMerge)
	assert.True(t, zeroChanges(again.Summary), "%+v", again.Summary)
}

// AC-011, TC-006 — a v1 file (17.1.0) has no field. Merge leaves an existing
// app's value alone, and a new app gets false.
func TestOptIn_MergeAFileWithoutTheField(t *testing.T) {
	d := decodeFixture(t)
	for _, a := range d.File.Apps {
		require.Nil(t, a.IframeSameOrigin, "%s: v1 has no field", a.Slug)
	}
	st := freshState()
	st.Apps = []storage.TransferApp{
		{ID: "app-s", Slug: "sonarr", Name: "Sonarr", URL: "https://sonarr.home.arpa", ClickAction: "iframe", IframeSameOrigin: true},
	}

	p := restorePlan(t, st, d, transfer.ModeMerge)
	assert.NotContains(t, item(t, p, transfer.KindApp, "Sonarr").Fields, "iframeSameOrigin", "a missing field is not a change")
	after := apply(t, newMem(st), p)
	assert.True(t, appBySlug(after, "sonarr").IframeSameOrigin, "the existing app keeps its value")
	for _, a := range after.Apps {
		if a.Slug != "sonarr" {
			assert.False(t, a.IframeSameOrigin, "%s: a new app is off", a.Slug)
		}
	}

	// Replace makes every app new, so every app is off.
	after = apply(t, newMem(st), restorePlan(t, st, d, transfer.ModeReplace))
	require.NotEmpty(t, after.Apps)
	for _, a := range after.Apps {
		assert.False(t, a.IframeSameOrigin, a.Slug)
	}
}

// AC-012 — Homepage import creates every app with the opt-in off.
func TestOptIn_HomepageImportCreatesAppsOff(t *testing.T) {
	st := freshState()
	st.Apps = []storage.TransferApp{{ID: "app-x", Slug: "existing", Name: "Existing", URL: "https://x.example.com",
		ClickAction: "iframe", IframeSameOrigin: true}}
	hp, perr := transfer.ParseHomepage(transfer.HomepageInput{Services: string(readFixture(t, "homepage/services.yaml"))})
	require.Nil(t, perr)
	p := transfer.PlanImport(st, hp, nil)
	m := newMem(st)
	after := apply(t, m, p)
	require.Greater(t, len(after.Apps), 1)
	for _, a := range after.Apps {
		if a.Slug == "existing" {
			assert.True(t, a.IframeSameOrigin, "import never touches an existing app")
			continue
		}
		assert.False(t, a.IframeSameOrigin, a.Slug)
	}
	for _, row := range m.created {
		require.NotNil(t, row.IframeSameOrigin, "%s: import says false, it doesn't leave it to a default", row.Slug)
		assert.False(t, *row.IframeSameOrigin, row.Slug)
	}
}
