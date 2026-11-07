package transfer_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/transfer"
)

// SPEC-backup-import §4 — parsing Homepage's files (AC-019, AC-020, AC-022,
// AC-026, AC-028), against fixtures written from Homepage's docs.

func fixtureInput(t *testing.T) transfer.HomepageInput {
	t.Helper()
	return transfer.HomepageInput{
		Services:  string(readFixture(t, "homepage/services.yaml")),
		Bookmarks: string(readFixture(t, "homepage/bookmarks.yaml")),
		Settings:  string(readFixture(t, "homepage/settings.yaml")),
	}
}

func parse(t *testing.T, in transfer.HomepageInput) *transfer.Homepage {
	t.Helper()
	hp, err := transfer.ParseHomepage(in)
	require.Nil(t, err, "%v", err)
	require.NotNil(t, hp)
	return hp
}

func entry(t *testing.T, hp *transfer.Homepage, name string) transfer.HomepageEntry {
	t.Helper()
	for _, e := range hp.Entries {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("no entry %q", name)
	return transfer.HomepageEntry{}
}

// AC-019 — all three files parse: groups, nested groups, bookmarks, layout,
// target and the ignored settings.
func TestHomepage_ParsesTheThreeFiles(t *testing.T) {
	hp := parse(t, fixtureInput(t))
	var secs []string
	for _, s := range hp.Sections {
		secs = append(secs, s.Name)
	}
	assert.Equal(t, []string{"Media", "Network", "Network / DNS", "Monitoring", "Developer"}, secs)
	assert.Equal(t, "Network", hp.Sections[2].Root)
	assert.Equal(t, []string{"Network", "Media", "Developer"}, hp.Layout)
	assert.Equal(t, "_blank", hp.Target)
	assert.Equal(t, []string{"title", "theme", "color", "background", "providers", "quicklaunch"}, hp.IgnoredSettings)

	var order []string
	for _, e := range hp.Entries {
		order = append(order, e.Section+"/"+e.Name)
	}
	assert.Equal(t, []string{
		"Media/Sonarr", "Media/Jellyfin", "Media/Plex", "Media/Reddit", // weight 50 first; bookmarks after services
		"Network/Router", "Network/Pi-hole",
		"Network / DNS/AdGuard",
		"Monitoring/Grafana", "Monitoring/Uptime Kuma", "Monitoring/Broken",
		"Developer/Github", "Developer/Docs",
	}, order)

	j := entry(t, hp, "Jellyfin")
	assert.Equal(t, "https://jellyfin.home.arpa", j.Href)
	assert.Equal(t, "Movies and TV", j.Description)
	assert.Equal(t, "jellyfin.png", j.Icon)
	assert.Equal(t, "https://jellyfin.home.arpa", j.SiteMonitor)
	assert.True(t, j.Widget)
	s := entry(t, hp, "Sonarr")
	assert.True(t, s.Docker, "server/container/showStats")
	a := entry(t, hp, "AdGuard")
	assert.True(t, a.Kubernetes, "app/namespace/podSelector")
	assert.Equal(t, "192.0.2.1", entry(t, hp, "Router").Ping)
	assert.Equal(t, 1, entry(t, hp, "Router").IgnoredKeys, "statusStyle")
	assert.Equal(t, "_self", entry(t, hp, "Plex").Target)
	assert.True(t, entry(t, hp, "Broken").NoFields)
	gh := entry(t, hp, "Github")
	assert.True(t, gh.Bookmark)
	assert.Equal(t, "https://github.com/", gh.Href)
	assert.Equal(t, 0, gh.IgnoredKeys, "abbr is ignored silently: the initials badge does its job")
	assert.Equal(t, "Team docs", entry(t, hp, "Docs").Description)
}

// AC-022 / §9 — widget blocks and providers never reach the parsed result.
func TestHomepage_DropsWidgetAndProviderValuesWhileParsing(t *testing.T) {
	hp := parse(t, fixtureInput(t))
	b, err := json.Marshal(hp)
	require.NoError(t, err)
	for _, secret := range []string{"fixture-widget-key-7c1d", "fixture-provider-key-55ab", "openweathermap", "enableBlocks"} {
		assert.NotContains(t, string(b), secret)
	}
}

// Only services.yaml is required (OQ-5 c).
func TestHomepage_ServicesAlone(t *testing.T) {
	hp := parse(t, transfer.HomepageInput{Services: string(readFixture(t, "homepage/tc008-services.yaml"))})
	assert.Len(t, hp.Entries, 4)
	assert.Empty(t, hp.Layout)
	assert.Equal(t, "", hp.Target)
}

func parseErr(t *testing.T, in transfer.HomepageInput) *transfer.Error {
	t.Helper()
	hp, err := transfer.ParseHomepage(in)
	assert.Nil(t, hp)
	require.NotNil(t, err)
	return err
}

// AC-028 / TC-010 — the wrong shape is a plain message with a line number.
func TestHomepage_WrongShapeNamesTheLine(t *testing.T) {
	err := parseErr(t, transfer.HomepageInput{Services: string(readFixture(t, "homepage/docker-compose.yaml"))})
	assert.Equal(t, http.StatusUnprocessableEntity, err.Status)
	assert.Equal(t, transfer.CodeNotHomepageServices, err.Code)
	assert.Equal(t, "services", err.File)
	assert.Equal(t, 1, err.Line)
	assert.Equal(t, "This doesn't look like a Homepage services.yaml. It should be a list of groups.", err.Message)

	err = parseErr(t, transfer.HomepageInput{Services: "- Media:\n    - Sonarr:\n        href: http://a.home.arpa\n- just a string\n"})
	assert.Equal(t, transfer.CodeNotHomepageServices, err.Code)
	assert.Equal(t, 4, err.Line)

	err = parseErr(t, transfer.HomepageInput{Services: "- Media:\n    href: http://a.home.arpa\n"})
	assert.Equal(t, transfer.CodeNotHomepageServices, err.Code)
	assert.Equal(t, 2, err.Line)

	err = parseErr(t, transfer.HomepageInput{Services: "   \n"})
	assert.Equal(t, transfer.CodeNotHomepageServices, err.Code)
}

func TestHomepage_BookmarksAndSettingsShapes(t *testing.T) {
	svc := string(readFixture(t, "homepage/tc008-services.yaml"))
	err := parseErr(t, transfer.HomepageInput{Services: svc, Bookmarks: "Developer:\n  - Github: x\n"})
	assert.Equal(t, transfer.CodeNotHomepageBookmark, err.Code)
	assert.Equal(t, "bookmarks", err.File)
	assert.Equal(t, 1, err.Line)

	err = parseErr(t, transfer.HomepageInput{Services: svc, Settings: "- title: x\n"})
	assert.Equal(t, transfer.CodeNotHomepageSettings, err.Code)
	assert.Equal(t, "settings", err.File)

	// A bookmark written as a plain map (no list) is accepted too.
	hp := parse(t, transfer.HomepageInput{Services: svc, Bookmarks: "- Links:\n    - Wiki:\n        href: https://wiki.example.com\n"})
	assert.Equal(t, "https://wiki.example.com", entry(t, hp, "Wiki").Href)
}

// AC-028 — a YAML syntax error is a 400 naming the file and line.
func TestHomepage_SyntaxErrorIs400WithLine(t *testing.T) {
	err := parseErr(t, transfer.HomepageInput{Services: "- Media:\n    - Sonarr:\n        href: [unclosed\n"})
	assert.Equal(t, http.StatusBadRequest, err.Status)
	assert.Equal(t, transfer.CodeYAMLSyntax, err.Code)
	assert.Equal(t, "services", err.File)
	assert.Greater(t, err.Line, 0)

	err = parseErr(t, transfer.HomepageInput{Services: "- Media: [ok]\n", Settings: "title: a\n  bad: indent\n"})
	assert.Equal(t, transfer.CodeYAMLSyntax, err.Code)
	assert.Equal(t, "settings", err.File)
	assert.Equal(t, 2, err.Line)
}

// §4.5 — custom tags are refused.
func TestHomepage_RefusesCustomTags(t *testing.T) {
	for _, tag := range []string{"!env", "!!python/object", "!secret"} {
		err := parseErr(t, transfer.HomepageInput{Services: "- Media:\n    - Sonarr:\n        href: " + tag + " SONARR_URL\n"})
		assert.Equal(t, transfer.CodeYAMLSyntax, err.Code, tag)
		assert.Equal(t, 3, err.Line, tag)
	}
}

// §4.5 — aliases work, and an alias bomb is refused instead of expanded.
func TestHomepage_AliasesAreCapped(t *testing.T) {
	ok := "- Media:\n    - Sonarr: &arr\n        href: http://sonarr.home.arpa\n        description: Arr\n    - Radarr:\n        <<: *arr\n        href: http://radarr.home.arpa\n"
	hp := parse(t, transfer.HomepageInput{Services: ok})
	assert.Equal(t, "http://radarr.home.arpa", entry(t, hp, "Radarr").Href, "an explicit key beats the merged one")
	assert.Equal(t, "Arr", entry(t, hp, "Radarr").Description)

	var b strings.Builder
	b.WriteString("a: &a [x, x, x, x, x, x, x, x, x, x]\n")
	for i := 1; i <= 9; i++ {
		fmt.Fprintf(&b, "%c: &%c [*%c, *%c, *%c, *%c, *%c, *%c, *%c, *%c, *%c, *%c]\n", 'a'+i, 'a'+i, 'a'+i-1, 'a'+i-1, 'a'+i-1, 'a'+i-1, 'a'+i-1, 'a'+i-1, 'a'+i-1, 'a'+i-1, 'a'+i-1, 'a'+i-1)
	}
	err := parseErr(t, transfer.HomepageInput{Services: "- Media:\n    - Sonarr:\n        href: http://sonarr.home.arpa\n", Settings: b.String() + "layout: *j\n"})
	assert.Equal(t, transfer.CodeYAMLSyntax, err.Code)
}

// §4.5 — nesting deeper than 8 groups is refused (8 is fine).
func TestHomepage_DepthLimit(t *testing.T) {
	nest := func(depth int) string {
		var b strings.Builder
		for i := 0; i < depth; i++ {
			pad := strings.Repeat("    ", i)
			fmt.Fprintf(&b, "%s- G%d:\n", pad, i+1)
		}
		pad := strings.Repeat("    ", depth)
		fmt.Fprintf(&b, "%s- App:\n%s    href: https://app.example.com\n", pad, pad)
		return b.String()
	}
	hp := parse(t, transfer.HomepageInput{Services: nest(8)})
	assert.Equal(t, "G1 / G2 / G3 / G4 / G5 / G6 / G7 / G8", hp.Entries[0].Section)
	err := parseErr(t, transfer.HomepageInput{Services: nest(9)})
	assert.Equal(t, http.StatusUnprocessableEntity, err.Status)
	assert.Equal(t, transfer.CodeTooDeep, err.Code)
	assert.Greater(t, err.Line, 0)
}

// AC-026 — each file ≤1 MB (413), ≤1,000 apps and ≤200 sections (422).
func TestHomepage_Limits(t *testing.T) {
	big := "- Media:\n    - A:\n        href: https://a.example.com\n        description: " + strings.Repeat("x", transfer.MaxHomepageFileBytes) + "\n"
	err := parseErr(t, transfer.HomepageInput{Services: big})
	assert.Equal(t, http.StatusRequestEntityTooLarge, err.Status)
	assert.Equal(t, transfer.CodeTooLarge, err.Code)
	assert.Equal(t, "services", err.File)
	err = parseErr(t, transfer.HomepageInput{Services: "- A: []\n", Bookmarks: big})
	assert.Equal(t, "bookmarks", err.File)

	apps := func(n int) string {
		var b strings.Builder
		b.WriteString("- Media:\n")
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "    - App %d:\n        href: https://a%d.example.com\n", i, i)
		}
		return b.String()
	}
	parse(t, transfer.HomepageInput{Services: apps(transfer.MaxApps)})
	err = parseErr(t, transfer.HomepageInput{Services: apps(transfer.MaxApps + 1)})
	assert.Equal(t, transfer.CodeTooMany, err.Code)
	assert.Equal(t, transfer.MaxApps+1, err.Count)
	assert.Equal(t, "apps", err.Things)

	var b strings.Builder
	for i := 0; i <= transfer.MaxSections; i++ {
		fmt.Fprintf(&b, "- S%d: []\n", i)
	}
	err = parseErr(t, transfer.HomepageInput{Services: b.String()})
	assert.Equal(t, transfer.CodeTooMany, err.Code)
	assert.Equal(t, "sections", err.Things)
	assert.Equal(t, transfer.MaxSections, err.Limit)
}

// settings.yaml `layout` in its list form, as some configs write it.
func TestHomepage_LayoutListForm(t *testing.T) {
	hp := parse(t, transfer.HomepageInput{Services: "- A: []\n", Settings: "layout:\n  - Infra:\n      style: row\n  - Media:\n"})
	assert.Equal(t, []string{"Infra", "Media"}, hp.Layout)
}

// Homepage sorts a group by weight; equal or missing weights keep file order.
func TestHomepage_WeightOrder(t *testing.T) {
	in := "- G:\n    - C:\n        href: https://c.example.com\n        weight: 300\n    - A:\n        href: https://a.example.com\n    - B:\n        href: https://b.example.com\n        weight: \"150\"\n    - D:\n        href: https://d.example.com\n        weight: 300\n"
	hp := parse(t, transfer.HomepageInput{Services: in})
	var got []string
	for _, e := range hp.Entries {
		got = append(got, e.Name)
	}
	// A's default weight is 200 (its position, 2, × 100).
	assert.Equal(t, []string{"B", "A", "C", "D"}, got)
}
