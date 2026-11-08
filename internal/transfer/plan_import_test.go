package transfer_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/transfer"
)

// SPEC-backup-import §2.4, §4.3, §4.4 — Homepage import plans.

func tc008(t *testing.T) transfer.HomepageInput {
	return svc(string(readFixture(t, "homepage/tc008-services.yaml")))
}

// TC-008 — the expected preview.
func TestImport_TC008Preview(t *testing.T) {
	p := importPlan(t, freshState(), tc008(t), nil)
	assert.Equal(t, transfer.ModeImport, p.Mode)
	assert.NotEmpty(t, p.PlanHash)
	assert.Equal(t, []string{"Media", "Infra", "Infra / Core"}, names(items(p, transfer.KindSection, transfer.ActionCreate)))
	assert.Equal(t, transfer.CreateCounts{Sections: 3, Apps: 4}, p.Summary.Create)
	assert.Equal(t, 0, p.Summary.Skipped)

	sonarr := item(t, p, transfer.KindApp, "Sonarr")
	assert.Equal(t, "https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons/png/sonarr.png", sonarr.Icon)
	assert.Equal(t, []string{transfer.WarnWidgetDropped}, sonarr.Warnings)
	plex := item(t, p, transfer.KindApp, "Plex")
	assert.Equal(t, []string{transfer.WarnIconGlyph}, plex.Warnings)
	assert.Equal(t, "mdi-plex", plex.Detail)
	assert.Equal(t, strp("Infra / Core"), item(t, p, transfer.KindApp, "Proxmox").Section)

	st := freshState()
	after := apply(t, newMem(st), importPlan(t, st, tc008(t), nil))
	assert.Equal(t, "same_tab", appBySlug(after, "router").ClickAction, "Router opens in the same tab")

	var snip struct {
		Endpoints []struct {
			Name, Group, URL, Interval string
			Conditions                 []string
		} `yaml:"endpoints"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(p.GatusSnippet), &snip), "the snippet is valid YAML:\n%s", p.GatusSnippet)
	require.Len(t, snip.Endpoints, 2)
	assert.Equal(t, "Sonarr", snip.Endpoints[0].Name)
	assert.Equal(t, "Media", snip.Endpoints[0].Group)
	assert.Equal(t, "http://sonarr.home.arpa/", snip.Endpoints[0].URL)
	assert.Equal(t, "60s", snip.Endpoints[0].Interval)
	assert.Equal(t, []string{"[STATUS] < 400"}, snip.Endpoints[0].Conditions)
	assert.Equal(t, "Router", snip.Endpoints[1].Name)
	assert.Equal(t, "Infra", snip.Endpoints[1].Group)
	assert.Equal(t, "icmp://192.0.2.1", snip.Endpoints[1].URL)
	assert.Equal(t, []string{"[CONNECTED] == true"}, snip.Endpoints[1].Conditions)
}

// AC-022 — a widget key and providers never reach the plan or the writes.
func TestImport_WidgetKeyNeverReachesPlanOrWrites(t *testing.T) {
	for _, in := range []transfer.HomepageInput{tc008(t), fixtureInput(t)} {
		st := freshState()
		p := importPlan(t, st, in, nil)
		b, _ := json.Marshal(p)
		m := newMem(st)
		after := apply(t, m, p)
		stb, _ := json.Marshal(after)
		for _, secret := range []string{"abc123", "fixture-widget-key-7c1d", "fixture-provider-key-55ab"} {
			assert.NotContains(t, string(b), secret)
			assert.NotContains(t, string(stb), secret)
			assert.NotContains(t, m.calls, secret)
		}
	}
}

// AC-021 — import never deletes, renames or reorders existing sections or
// apps, and an app whose normalized link exists is "Already on your
// dashboard". Re-running the same import adds nothing.
func TestImport_MergesWithoutTouchingWhatExists(t *testing.T) {
	st := freshState()
	media := "sec-media"
	st.Sections = []storage.TransferSection{{ID: "sec-z", Name: "Zeta", SortIndex: 0, GridWidth: 12}, {ID: media, Name: "Media", SortIndex: 1, GridWidth: 3}}
	st.Apps = []storage.TransferApp{
		{ID: "app-s", Slug: "my-sonarr", Name: "My Sonarr", Description: "mine", URL: "HTTP://Sonarr.Home.Arpa", Icon: "https://i.example.com/s.png", ClickAction: "iframe", SectionID: &media},
		{ID: "app-k", Slug: "keep", Name: "Keep", URL: "https://keep.example.com", ClickAction: "new_tab"},
	}
	before, _ := json.Marshal(st.Apps)
	m := newMem(st)
	p := importPlan(t, st, tc008(t), nil)
	s := item(t, p, transfer.KindApp, "Sonarr")
	assert.Equal(t, transfer.ActionUnchanged, s.Action)
	assert.Equal(t, transfer.ReasonAlreadyHere, s.Reason)
	assert.Equal(t, 1, p.Summary.Unchanged)
	assert.Equal(t, 3, p.Summary.Create.Apps)
	assert.Equal(t, []string{"Infra", "Infra / Core"}, names(items(p, transfer.KindSection, transfer.ActionCreate)))
	for _, it := range p.Items {
		assert.NotEqual(t, transfer.ActionDelete, it.Action)
	}

	after := apply(t, m, p)
	got, _ := json.Marshal([]storage.TransferApp{*appBySlug(after, "my-sonarr"), *appBySlug(after, "keep")})
	assert.JSONEq(t, string(before), string(got), "existing apps unchanged")
	assert.Equal(t, []string{"Zeta", "Media", "Infra", "Infra / Core"}, sectionNames(after))
	assert.Equal(t, 0, after.Sections[0].SortIndex)
	assert.Equal(t, 3, after.Sections[1].GridWidth)
	for _, c := range m.calls {
		assert.NotContains(t, c, "UpdateApp")
		assert.NotContains(t, c, "UpdateSection")
		assert.NotContains(t, c, "DeleteCatalog")
	}

	again := importPlan(t, after, tc008(t), nil)
	assert.True(t, zeroChanges(again.Summary), "%+v", again.Summary)
	assert.Equal(t, 4, again.Summary.Unchanged)
	for _, it := range items(again, transfer.KindApp, "") {
		assert.Equal(t, transfer.ReasonAlreadyHere, it.Reason, it.Name)
	}
}

// §2.4 — the same link twice in the files imports once.
func TestImport_DuplicateLinkInTheFiles(t *testing.T) {
	in := transfer.HomepageInput{
		Services:  "- Media:\n    - Jellyfin:\n        href: https://jellyfin.home.arpa\n",
		Bookmarks: "- Links:\n    - Jellyfin web:\n        - href: https://JELLYFIN.home.arpa/\n",
	}
	p := importPlan(t, freshState(), in, nil)
	assert.Equal(t, transfer.ReasonDuplicate, item(t, p, transfer.KindApp, "Jellyfin web").Reason)
	assert.Equal(t, 1, p.Summary.Create.Apps)
}

// AC-023 / TC-009 — re-importing after Gatus learns the key fills an existing
// app's EMPTY gatus_key, and nothing else; an app that already has a key is
// left alone.
func TestImport_ReimportTurnsOnStatus(t *testing.T) {
	st := freshState()
	m := newMem(st)
	first := importPlan(t, st, tc008(t), nil)
	after := apply(t, m, first)
	assert.Equal(t, "", appBySlug(after, "sonarr").GatusKey)

	// The admin edits Sonarr's name; Proxmox already has a key of its own.
	appBySlug(after, "sonarr").Name = "Sonarr (renamed)"
	appBySlug(after, "proxmox").GatusKey = "custom_key"

	keys := map[string]bool{"media_sonarr": true, "infra_router": true, "infra---core_proxmox": true}
	second := importPlan(t, after, tc008(t), keys)
	s := item(t, second, transfer.KindApp, "Sonarr")
	assert.Equal(t, transfer.ActionUpdate, s.Action)
	assert.Equal(t, transfer.ReasonAlreadyHere, s.Reason)
	assert.Equal(t, []string{transfer.WarnStatusOn}, s.Warnings)
	assert.Equal(t, []string{"gatusKey"}, s.Fields)
	assert.Equal(t, transfer.ActionUpdate, item(t, second, transfer.KindApp, "Router").Action)
	assert.Equal(t, transfer.ActionUnchanged, item(t, second, transfer.KindApp, "Proxmox").Action)
	assert.Equal(t, transfer.ActionUnchanged, item(t, second, transfer.KindApp, "Plex").Action)
	assert.Equal(t, 2, second.Summary.Update.Apps)
	assert.Equal(t, transfer.CreateCounts{}, second.Summary.Create)
	assert.Empty(t, second.GatusSnippet, "every monitored app is matched now")

	final := apply(t, m, second)
	assert.Equal(t, "media_sonarr", appBySlug(final, "sonarr").GatusKey)
	assert.Equal(t, "Sonarr (renamed)", appBySlug(final, "sonarr").Name, "only the key changes")
	assert.Equal(t, "infra_router", appBySlug(final, "router").GatusKey)
	assert.Equal(t, "custom_key", appBySlug(final, "proxmox").GatusKey)
}

// AC-023 — a monitored app whose key Gatus doesn't have stays unmonitored and
// is in the snippet; an existing app with an empty key is in it too.
func TestImport_SnippetListsEveryUnmatchedMonitor(t *testing.T) {
	st := freshState()
	st.Apps = []storage.TransferApp{{ID: "app-r", Slug: "router", Name: "Router", URL: "http://192.0.2.1", ClickAction: "new_tab"}}
	p := importPlan(t, st, tc008(t), map[string]bool{"media_sonarr": true})
	assert.NotContains(t, p.GatusSnippet, "Sonarr")
	assert.Contains(t, p.GatusSnippet, "Router", "already here, still unmonitored")
	assert.Equal(t, transfer.ActionUnchanged, item(t, p, transfer.KindApp, "Router").Action)

	none := importPlan(t, freshState(), svc("- G:\n    - A:\n        href: https://a.example.com\n"), nil)
	assert.Empty(t, none.GatusSnippet, "no snippet when nothing is monitored")
}

// AC-029 / §4.4 — imported apps are appended to the importing admin's order
// in Homepage order; an admin with no saved order first gets one saved from
// the current view, so nothing they see moves. Other people's order is left
// alone.
func TestImport_OrderForTheImportingAdmin(t *testing.T) {
	st := freshState()
	st.CallerID = "u-alex" // a second admin runs the import
	st.People[1].Role = "admin"
	st.Apps = []storage.TransferApp{
		{ID: "app-b", Slug: "b", Name: "Bravo", URL: "https://b.example.com", ClickAction: "new_tab"},
		{ID: "app-a", Slug: "a", Name: "Alpha", URL: "https://a.example.com", ClickAction: "new_tab"},
	}
	st.People[0].Order = []string{"app-b"}
	m := newMem(st)
	m.state()
	require.Equal(t, []string{"app-a", "app-b"}, st.CallerOrder, "Alex's view is A–Z")

	after := apply(t, m, importPlan(t, st, tc008(t), nil))
	var got []string
	for _, id := range personByEmail(after, "alex@example.com").Order {
		got = append(got, appByID(after, id).Name)
	}
	assert.Equal(t, []string{"Alpha", "Bravo", "Sonarr", "Plex", "Router", "Proxmox"}, got)
	assert.Equal(t, []string{"app-b"}, personByEmail(after, "admin@example.com").Order, "the owner's own order is untouched")
	assert.Equal(t, 1, importPlan(t, freshState(), tc008(t), nil).OtherAccounts)
}

func appByID(st *storage.TransferState, id string) *storage.TransferApp {
	for i := range st.Apps {
		if st.Apps[i].ID == id {
			return &st.Apps[i]
		}
	}
	return nil
}

// AC-008 (pure half) — the hash is stable for the same inputs and moves when
// the catalog does (TC-006's "another tab added an app").
func TestImport_PlanHashTracksTheCatalog(t *testing.T) {
	a := importPlan(t, freshState(), tc008(t), nil)
	b := importPlan(t, freshState(), tc008(t), nil)
	assert.Equal(t, a.PlanHash, b.PlanHash)
	assert.Len(t, a.PlanHash, 64)

	st := freshState()
	st.Apps = []storage.TransferApp{{ID: "app-p", Slug: "plex", Name: "Plex", URL: "http://plex.home.arpa", ClickAction: "new_tab"}}
	c := importPlan(t, st, tc008(t), nil)
	assert.NotEqual(t, a.PlanHash, c.PlanHash)

	d := importPlan(t, freshState(), tc008(t), map[string]bool{"media_sonarr": true})
	assert.NotEqual(t, a.PlanHash, d.PlanHash, "a Gatus change moves the plan too")

	e := freshState()
	e.OtherAccounts = 7
	assert.Equal(t, a.PlanHash, importPlan(t, e, tc008(t), nil).PlanHash, "a new sign-up doesn't stale a preview")

	r := a.Result()
	assert.Empty(t, r.PlanHash)
	assert.True(t, r.Applied)
	assert.NotEmpty(t, a.PlanHash, "Result doesn't change the plan")
}

// The full fixture set: every skip has a reason, ignored settings are listed,
// dropped categories are named.
func TestImport_FixtureSet(t *testing.T) {
	p := importPlan(t, freshState(), fixtureInput(t), map[string]bool{})
	skipped := map[string]string{}
	for _, it := range items(p, transfer.KindApp, transfer.ActionSkip) {
		skipped[it.Name] = it.Reason
	}
	assert.Equal(t, map[string]string{
		"Grafana":     transfer.ReasonHrefVariable,
		"Uptime Kuma": transfer.ReasonBadScheme,
		"Broken":      transfer.ReasonNoLink,
	}, skipped)
	assert.Equal(t, 3, p.Summary.Skipped)
	assert.Equal(t, 9, p.Summary.Create.Apps)
	assert.Equal(t, []string{"widgets", "docker", "kubernetes"}, p.Ignored.Dropped)
	assert.Equal(t, []string{"Network", "Network / DNS", "Media", "Developer"},
		names(items(p, transfer.KindSection, transfer.ActionCreate)),
		"layout order, then file order; Monitoring gets no app, so it isn't created")
	assert.Equal(t, []string{transfer.WarnIconLocal}, item(t, p, transfer.KindApp, "AdGuard").Warnings)
}
