package transfer_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/transfer"
)

// AC-020 — one test (at least) per row of the §4.1 and §4.2 mapping tables.
// Each test is named after its row.

func importPlan(t *testing.T, st *storage.TransferState, in transfer.HomepageInput, gatus map[string]bool) *transfer.Plan {
	t.Helper()
	hp := parse(t, in)
	p := transfer.PlanImport(st, hp, gatus)
	require.NotNil(t, p)
	assertEverySkipHasReason(t, p)
	return p
}

func svc(yaml string) transfer.HomepageInput { return transfer.HomepageInput{Services: yaml} }

func stateWithSection(name string, sort int) *storage.TransferState {
	st := freshState()
	st.Sections = []storage.TransferSection{{ID: "sec-old", Name: name, SortIndex: sort, GridWidth: 3}}
	return st
}

// §4.1 "Group name (top level) → Section name": matches an existing section by
// exact name, or creates one with gridWidth 6.
func TestMappingRow_GroupName(t *testing.T) {
	st := stateWithSection("Media", 0)
	p := importPlan(t, st, svc("- Media:\n    - A:\n        href: https://a.example.com\n- media:\n    - B:\n        href: https://b.example.com\n"), nil)
	assert.Equal(t, []string{"media"}, names(items(p, transfer.KindSection, transfer.ActionCreate)), "exact name: Media matches, media is new")
	assert.Equal(t, strp("Media"), item(t, p, transfer.KindApp, "A").Section)
	m := newMem(st)
	after := apply(t, m, p)
	assert.Equal(t, []string{"Media", "media"}, sectionNames(after))
	assert.Equal(t, 6, after.Sections[1].GridWidth)
	assert.Equal(t, 3, after.Sections[0].GridWidth, "the existing section is untouched")
}

// §4.1 "Nested group → Section named Parent / Child", any depth (OQ-7 a).
func TestMappingRow_NestedGroup(t *testing.T) {
	p := importPlan(t, freshState(), svc("- Infra:\n    - Core:\n        - Deep:\n            - Proxmox:\n                href: https://pve.home.arpa\n"), nil)
	assert.Equal(t, []string{"Infra / Core / Deep"}, names(items(p, transfer.KindSection, transfer.ActionCreate)),
		"only a section that gets an app is created")
	assert.Equal(t, strp("Infra / Core / Deep"), item(t, p, transfer.KindApp, "Proxmox").Section)
}

// §4.1 "Group order → Section order": new sections go after the existing ones,
// in settings.yaml layout order, then file order. Existing sections never move.
func TestMappingRow_GroupOrder(t *testing.T) {
	st := stateWithSection("Home", 4)
	in := transfer.HomepageInput{
		Services: "- Media:\n    - A:\n        href: https://a.example.com\n- Infra:\n    - B:\n        href: https://b.example.com\n- Home:\n    - C:\n        href: https://c.example.com\n- Misc:\n    - D:\n        href: https://d.example.com\n",
		Settings: "layout:\n  Infra:\n  Media:\n",
	}
	p := importPlan(t, st, in, nil)
	assert.Equal(t, []string{"Infra", "Media", "Misc"}, names(items(p, transfer.KindSection, transfer.ActionCreate)))
	after := apply(t, newMem(st), p)
	assert.Equal(t, []string{"Home", "Infra", "Media", "Misc"}, sectionNames(after))
	assert.Equal(t, 4, after.Sections[0].SortIndex, "Home kept its sort_index")
	for _, s := range after.Sections[1:] {
		assert.Greater(t, s.SortIndex, 4)
	}
}

// §4.1 "Service name → name": trimmed; an empty name skips the entry.
func TestMappingRow_ServiceName(t *testing.T) {
	p := importPlan(t, freshState(), svc("- G:\n    - \"  Sonarr  \":\n        href: https://sonarr.home.arpa\n    - \"  \":\n        href: https://blank.home.arpa\n"), nil)
	assert.Equal(t, transfer.ActionCreate, item(t, p, transfer.KindApp, "Sonarr").Action)
	assert.Equal(t, transfer.ReasonNoName, item(t, p, transfer.KindApp, "").Reason)
}

// §4.1 "— → slug": derived from the name; a collision gets -2, -3.
func TestMappingRow_Slug(t *testing.T) {
	assert.Equal(t, "home-assistant", transfer.Slugify("Home Assistant"))
	assert.Equal(t, "pi-hole-v6", transfer.Slugify("  Pi-hole (v6)!  "))
	assert.Equal(t, "", transfer.Slugify("★"))

	st := freshState()
	st.Apps = []storage.TransferApp{{ID: "app-x", Slug: "plex", Name: "Plex", URL: "https://old-plex.example.com", ClickAction: "new_tab"}}
	p := importPlan(t, st, svc("- A:\n    - Plex:\n        href: https://plex1.example.com\n- B:\n    - Plex:\n        href: https://plex2.example.com\n    - ★:\n        href: https://star.example.com\n"), nil)
	after := apply(t, newMem(st), p)
	assert.NotNil(t, appBySlug(after, "plex-2"))
	assert.NotNil(t, appBySlug(after, "plex-3"))
	assert.NotNil(t, appBySlug(after, "app"), "a name with no a-z0-9 gets a fallback slug")
	assert.Equal(t, "https://old-plex.example.com", appBySlug(after, "plex").URL, "the existing app is untouched")
}

// §4.1 "href → url": required, http or https.
func TestMappingRow_Href(t *testing.T) {
	p := importPlan(t, freshState(), svc("- G:\n    - Ok:\n        href: HTTPS://ok.example.com\n    - None:\n        description: x\n    - Ssh:\n        href: ssh://box.home.arpa\n    - Bare:\n        href: 192.0.2.1\n    - Js:\n        href: javascript:alert(1)\n"), nil)
	assert.Equal(t, transfer.ActionCreate, item(t, p, transfer.KindApp, "Ok").Action)
	assert.Equal(t, transfer.ReasonNoLink, item(t, p, transfer.KindApp, "None").Reason)
	for _, n := range []string{"Ssh", "Bare", "Js"} {
		assert.Equal(t, transfer.ReasonBadScheme, item(t, p, transfer.KindApp, n).Reason, n)
	}
	assert.True(t, transfer.IsHTTPURL("http://192.0.2.1"))
	assert.False(t, transfer.IsHTTPURL("http://"))
	assert.False(t, transfer.IsHTTPURL("ftp://x.example.com"))
}

// §4.1 "href with {{HOMEPAGE_VAR_…}}, {{HOMEPAGE_FILE_…}} or ${…}": skipped.
func TestMappingRow_HrefVariable(t *testing.T) {
	p := importPlan(t, freshState(), svc("- G:\n    - V:\n        href: \"{{HOMEPAGE_VAR_URL}}\"\n    - F:\n        href: https://{{HOMEPAGE_FILE_HOST}}/\n    - D:\n        href: https://${HOST}/\n"), nil)
	for _, n := range []string{"V", "F", "D"} {
		assert.Equal(t, transfer.ReasonHrefVariable, item(t, p, transfer.KindApp, n).Reason, n)
	}
}

// §4.1 "description → description": as is; {{…}} placeholders stay as text,
// with a warning.
func TestMappingRow_Description(t *testing.T) {
	st := freshState()
	p := importPlan(t, st, svc("- G:\n    - A:\n        href: https://a.example.com\n        description: DNS for {{HOMEPAGE_VAR_LAN}}\n    - B:\n        href: https://b.example.com\n        description: Plain\n"), nil)
	assert.Contains(t, item(t, p, transfer.KindApp, "A").Warnings, transfer.WarnDescriptionVariable)
	assert.Empty(t, item(t, p, transfer.KindApp, "B").Warnings)
	after := apply(t, newMem(st), p)
	assert.Equal(t, "DNS for {{HOMEPAGE_VAR_LAN}}", appBySlug(after, "a").Description)
}

// §4.1 icon rows, one case each (OQ-8 a).
func TestMappingRow_Icon(t *testing.T) {
	cases := []struct{ row, in, url, warn string }{
		{"http(s) URL", "https://example.com/proxmox.png", "https://example.com/proxmox.png", ""},
		{"local path", "/icons/x.png", "", transfer.WarnIconLocal},
		{"relative local path", "icons/x.png", "", transfer.WarnIconLocal},
		{"Dashboard Icons, no extension", "sonarr", "https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons/png/sonarr.png", ""},
		{"Dashboard Icons .png", "sonarr.png", "https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons/png/sonarr.png", ""},
		{"Dashboard Icons .svg", "pi-hole.svg", "https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons/svg/pi-hole.svg", ""},
		{"Dashboard Icons .webp", "home-assistant.webp", "https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons/webp/home-assistant.webp", ""},
		{"selfh.st, no extension", "sh-sonarr", "https://cdn.jsdelivr.net/gh/selfhst/icons@main/png/sonarr.png", ""},
		{"selfh.st .svg", "sh-jellyfin.svg", "https://cdn.jsdelivr.net/gh/selfhst/icons@main/svg/jellyfin.svg", ""},
		{"mdi", "mdi-plex", "", transfer.WarnIconGlyph},
		{"mdi with color", "mdi-plex-#E5A00D", "", transfer.WarnIconGlyph},
		{"si", "si-github", "", transfer.WarnIconGlyph},
		{"other scheme", "data:image/png;base64,AAAA", "", transfer.WarnIconUnsupported},
		{"empty", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.row, func(t *testing.T) {
			got := transfer.MapIcon(c.in)
			assert.Equal(t, c.url, got.URL)
			assert.Equal(t, c.warn, got.Warning)
		})
	}
	p := importPlan(t, freshState(), svc("- G:\n    - Plex:\n        icon: mdi-plex\n        href: https://plex.home.arpa\n    - Sonarr:\n        icon: sonarr.png\n        href: https://sonarr.home.arpa\n"), nil)
	plex := item(t, p, transfer.KindApp, "Plex")
	assert.Equal(t, []string{transfer.WarnIconGlyph}, plex.Warnings)
	assert.Equal(t, "mdi-plex", plex.Detail)
	assert.Equal(t, "", plex.Icon)
	assert.Equal(t, "https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons/png/sonarr.png", item(t, p, transfer.KindApp, "Sonarr").Icon)
}

// §4.1 "target → clickAction": _blank → new_tab, _self/_top → same_tab; with
// no target, settings.yaml target, then new_tab.
func TestMappingRow_Target(t *testing.T) {
	assert.Equal(t, "new_tab", transfer.ClickActionFor("_blank", ""))
	assert.Equal(t, "same_tab", transfer.ClickActionFor("_self", ""))
	assert.Equal(t, "same_tab", transfer.ClickActionFor("_top", ""))
	assert.Equal(t, "same_tab", transfer.ClickActionFor("", "_self"))
	assert.Equal(t, "new_tab", transfer.ClickActionFor("", ""))
	assert.Equal(t, "new_tab", transfer.ClickActionFor("_parent", ""))

	st := freshState()
	in := transfer.HomepageInput{
		Services: "- G:\n    - A:\n        href: https://a.example.com\n    - B:\n        href: https://b.example.com\n        target: _blank\n",
		Settings: "target: _self\n",
	}
	after := apply(t, newMem(st), importPlan(t, st, in, nil))
	assert.Equal(t, "same_tab", appBySlug(after, "a").ClickAction, "settings.yaml target")
	assert.Equal(t, "new_tab", appBySlug(after, "b").ClickAction, "the entry's own target wins")
}

// §4.1 "weight → order within the section" (AC-029), checked in the
// importing admin's own order.
func TestMappingRow_Weight(t *testing.T) {
	st := freshState()
	in := svc("- G:\n    - Late:\n        href: https://late.example.com\n        weight: 900\n    - Early:\n        href: https://early.example.com\n        weight: 10\n    - Middle:\n        href: https://middle.example.com\n")
	after := apply(t, newMem(st), importPlan(t, st, in, nil))
	var got []string
	for _, id := range personByEmail(after, "admin@example.com").Order {
		for _, a := range after.Apps {
			if a.ID == id {
				got = append(got, a.Name)
			}
		}
	}
	assert.Equal(t, []string{"Early", "Middle", "Late"}, got)
}

// §4.1 "siteMonitor, ping → gatus_key and the Gatus snippet" (OQ-9 b).
func TestMappingRow_Monitors(t *testing.T) {
	assert.Equal(t, "media_jellyfin", transfer.GatusKey("Media", "Jellyfin"))
	assert.Equal(t, "home-lab_pi-hole-(v6)", transfer.GatusKey(" Home Lab ", "Pi-hole (v6)"))
	assert.Equal(t, "a---b_x-y-z-w-v-u-t", transfer.GatusKey("A / B", "x_y.z,w#v+u&t"))

	st := freshState()
	in := svc("- Media:\n    - Jellyfin:\n        href: https://jellyfin.home.arpa\n        siteMonitor: https://jellyfin.home.arpa\n    - Sonarr:\n        href: https://sonarr.home.arpa\n        siteMonitor: https://sonarr.home.arpa/\n- Infra:\n    - Router:\n        href: http://192.0.2.1\n        ping: 192.0.2.1\n")
	p := importPlan(t, st, in, map[string]bool{"media_jellyfin": true})
	after := apply(t, newMem(st), p)
	assert.Equal(t, "media_jellyfin", appBySlug(after, "jellyfin").GatusKey, "Gatus knows the key")
	assert.Equal(t, "", appBySlug(after, "sonarr").GatusKey, "Gatus doesn't know it yet: Not monitored")
	assert.Equal(t, "", appBySlug(after, "router").GatusKey)
	assert.Contains(t, p.GatusSnippet, "name: Sonarr")
	assert.Contains(t, p.GatusSnippet, "url: https://sonarr.home.arpa/")
	assert.Contains(t, p.GatusSnippet, "url: icmp://192.0.2.1")
	assert.NotContains(t, p.GatusSnippet, "Jellyfin", "a matched app needs no snippet")
}

// §4.1 "widget, widgets → skipped: Widgets don't carry over".
func TestMappingRow_Widget(t *testing.T) {
	p := importPlan(t, freshState(), svc("- G:\n    - A:\n        href: https://a.example.com\n        widget: { type: sonarr, key: k-1 }\n    - B:\n        href: https://b.example.com\n        widgets:\n          - type: x\n            key: k-2\n"), nil)
	assert.Equal(t, []string{transfer.WarnWidgetDropped}, item(t, p, transfer.KindApp, "A").Warnings)
	assert.Equal(t, []string{transfer.WarnWidgetDropped}, item(t, p, transfer.KindApp, "B").Warnings)
	assert.Contains(t, p.Ignored.Dropped, "widgets")
}

// §4.1 "server, container, showStats → skipped (Docker stats)".
func TestMappingRow_Docker(t *testing.T) {
	p := importPlan(t, freshState(), svc("- G:\n    - A:\n        href: https://a.example.com\n        server: local\n        container: a\n        showStats: true\n"), nil)
	assert.Equal(t, transfer.ActionCreate, item(t, p, transfer.KindApp, "A").Action)
	assert.Equal(t, []string{"docker"}, p.Ignored.Dropped)
}

// §4.1 "app, namespace, podSelector → skipped (Kubernetes)".
func TestMappingRow_Kubernetes(t *testing.T) {
	p := importPlan(t, freshState(), svc("- G:\n    - A:\n        href: https://a.example.com\n        app: a\n        namespace: default\n        podSelector: app=a\n"), nil)
	assert.Equal(t, []string{"kubernetes"}, p.Ignored.Dropped)
}

// §4.1 "statusStyle, id, any other key → ignored, and counted".
func TestMappingRow_OtherKeys(t *testing.T) {
	p := importPlan(t, freshState(), svc("- G:\n    - A:\n        href: https://a.example.com\n        statusStyle: dot\n        id: a1\n        fancy: true\n"), nil)
	assert.Equal(t, 3, p.Ignored.Fields)
	assert.Empty(t, p.Ignored.Dropped)
}

// §4.2 "Bookmark group → Section": same rules; a bookmark group with the same
// name as a service group shares that section.
func TestMappingRow_BookmarkGroup(t *testing.T) {
	in := transfer.HomepageInput{
		Services:  "- Media:\n    - Jellyfin:\n        href: https://jellyfin.home.arpa\n",
		Bookmarks: "- Media:\n    - Reddit:\n        - href: https://www.reddit.com/r/selfhosted/\n- Developer:\n    - Github:\n        - href: https://github.com/\n",
	}
	p := importPlan(t, freshState(), in, nil)
	assert.Equal(t, []string{"Media", "Developer"}, names(items(p, transfer.KindSection, transfer.ActionCreate)))
	assert.Equal(t, strp("Media"), item(t, p, transfer.KindApp, "Reddit").Section)
}

// §4.2 "Bookmark name → name", "href → url" (same rules), "description",
// "icon" (same rules) and "abbr → ignored".
func TestMappingRow_BookmarkFields(t *testing.T) {
	st := freshState()
	in := transfer.HomepageInput{
		Services:  "- G: []\n",
		Bookmarks: "- Links:\n    - Github:\n        - abbr: GH\n          href: https://github.com/\n    - Docs:\n        - icon: si-readthedocs\n          href: https://docs.example.com\n          description: Team docs\n    - Ftp:\n        - href: ftp://files.home.arpa\n",
	}
	p := importPlan(t, st, in, nil)
	assert.Equal(t, transfer.ReasonBadScheme, item(t, p, transfer.KindApp, "Ftp").Reason)
	assert.Equal(t, []string{transfer.WarnIconGlyph}, item(t, p, transfer.KindApp, "Docs").Warnings)
	assert.Equal(t, 0, p.Ignored.Fields, "abbr isn't counted")
	after := apply(t, newMem(st), p)
	gh := appBySlug(after, "github")
	require.NotNil(t, gh)
	assert.Equal(t, "Github", gh.Name)
	assert.Equal(t, "https://github.com/", gh.URL)
	assert.Equal(t, "", gh.Description, "Homepage shows the host; Homepad leaves it empty")
	assert.Equal(t, "Team docs", appBySlug(after, "docs").Description)
}

// §4.2 settings.yaml — only layout and target are read; the rest is listed
// once, by name.
func TestMappingRow_Settings(t *testing.T) {
	p := importPlan(t, freshState(), fixtureInput(t), nil)
	assert.Equal(t, []string{"title", "theme", "color", "background", "providers", "quicklaunch"}, p.Ignored.Settings)
}

// §2.4 — links are compared trimmed, without a trailing "/", with the scheme
// and host lowercased (and nothing else).
func TestNormalizeLink(t *testing.T) {
	assert.Equal(t, "https://sonarr.home.arpa", transfer.NormalizeLink("  HTTPS://Sonarr.Home.Arpa/  "))
	assert.Equal(t, "http://a.example.com/Path", transfer.NormalizeLink("http://A.example.com/Path/"))
	assert.NotEqual(t, transfer.NormalizeLink("http://a.example.com/x"), transfer.NormalizeLink("http://a.example.com/X"))
}
