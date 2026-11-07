package transfer_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/transfer"
)

// memWriter applies a plan to an in-memory TransferState, with the same
// cascade rules as the schema (deleting an app drops it from every person's
// favorites, order and allow-list). It lets the pure tests check the full
// preview → apply → preview loop without a database.
type memWriter struct {
	st *storage.TransferState
	n  int
	// calls records every write, in order, for tests that check what an
	// apply did and didn't touch.
	calls []string
}

func newMem(st *storage.TransferState) *memWriter { return &memWriter{st: st} }

func (m *memWriter) id(prefix string) string {
	m.n++
	return fmt.Sprintf("%s-%04d", prefix, m.n)
}

func (m *memWriter) log(format string, args ...any) {
	m.calls = append(m.calls, fmt.Sprintf(format, args...))
}

func dropIDs(ids []string, gone map[string]bool) []string {
	out := []string{}
	for _, id := range ids {
		if !gone[id] {
			out = append(out, id)
		}
	}
	return out
}

func (m *memWriter) DeleteCatalog(_ context.Context, owner string) error {
	m.log("DeleteCatalog %s", owner)
	gone := map[string]bool{}
	for _, a := range m.st.Apps {
		gone[a.ID] = true
	}
	for _, s := range m.st.Sections {
		gone[s.ID] = true
	}
	m.st.Apps, m.st.Sections, m.st.Offers = nil, nil, nil
	for i := range m.st.People {
		p := &m.st.People[i]
		p.Favorites, p.Order = dropIDs(p.Favorites, gone), dropIDs(p.Order, gone)
		p.Collapsed, p.Allowed = dropIDs(p.Collapsed, gone), dropIDs(p.Allowed, gone)
	}
	return nil
}

func (m *memWriter) CreateSection(_ context.Context, owner, name string, sortIndex, width int) (string, error) {
	for _, s := range m.st.Sections {
		if s.Name == name {
			return "", fmt.Errorf("section %q exists", name)
		}
	}
	id := m.id("sec")
	m.log("CreateSection %s", name)
	m.st.Sections = append(m.st.Sections, storage.TransferSection{ID: id, Name: name, SortIndex: sortIndex, GridWidth: width})
	return id, nil
}

func (m *memWriter) UpdateSection(_ context.Context, owner, id string, sortIndex, width int) error {
	m.log("UpdateSection %s", id)
	for i := range m.st.Sections {
		if m.st.Sections[i].ID == id {
			m.st.Sections[i].SortIndex, m.st.Sections[i].GridWidth = sortIndex, width
			return nil
		}
	}
	return fmt.Errorf("no section %s", id)
}

func (m *memWriter) CreateOffer(_ context.Context, o storage.TransferOfferRow) (string, error) {
	id := m.id("off")
	m.log("CreateOffer %s", o.Name)
	m.st.Offers = append(m.st.Offers, storage.TransferOffer{ID: id, Name: o.Name, URL: o.URL, Icon: o.Icon,
		Description: o.Description, SuggestedCategory: o.SuggestedCategory, SortIndex: o.SortIndex})
	return id, nil
}

func (m *memWriter) UpdateOffer(_ context.Context, id string, o storage.TransferOfferRow) error {
	m.log("UpdateOffer %s", id)
	for i := range m.st.Offers {
		if m.st.Offers[i].ID == id {
			m.st.Offers[i] = storage.TransferOffer{ID: id, Name: o.Name, URL: o.URL, Icon: o.Icon,
				Description: o.Description, SuggestedCategory: o.SuggestedCategory, SortIndex: o.SortIndex}
			return nil
		}
	}
	return fmt.Errorf("no offer %s", id)
}

func (m *memWriter) CreateApp(_ context.Context, owner string, a storage.TransferAppRow) (string, error) {
	for _, x := range m.st.Apps {
		if x.Slug == a.Slug {
			return "", fmt.Errorf("slug %q taken", a.Slug)
		}
	}
	id := m.id("app")
	m.log("CreateApp %s", a.Slug)
	m.st.Apps = append(m.st.Apps, storage.TransferApp{ID: id, Slug: a.Slug, Name: a.Name, Description: a.Description,
		URL: a.URL, Icon: a.Icon, GatusKey: a.GatusKey, ClickAction: a.ClickAction, SectionID: a.SectionID})
	return id, nil
}

func (m *memWriter) UpdateApp(_ context.Context, owner, id string, a storage.TransferAppRow) error {
	m.log("UpdateApp %s", a.Slug)
	for i := range m.st.Apps {
		x := &m.st.Apps[i]
		if x.ID == id {
			x.Slug, x.Name, x.Description, x.URL, x.Icon = a.Slug, a.Name, a.Description, a.URL, a.Icon
			x.GatusKey, x.ClickAction, x.SectionID = a.GatusKey, a.ClickAction, a.SectionID
			return nil
		}
	}
	return fmt.Errorf("no app %s", id)
}

func (m *memWriter) SetGatusKey(_ context.Context, owner, id, key string) error {
	m.log("SetGatusKey %s %s", id, key)
	for i := range m.st.Apps {
		if m.st.Apps[i].ID == id {
			m.st.Apps[i].GatusKey = key
			return nil
		}
	}
	return fmt.Errorf("no app %s", id)
}

func (m *memWriter) LinkLibrary(_ context.Context, owner string, appIDs []string) error {
	want := map[string]bool{}
	for _, id := range appIDs {
		want[id] = true
	}
	for i := range m.st.Apps {
		a := &m.st.Apps[i]
		if !want[a.ID] || a.SourceLibraryID != nil {
			continue
		}
		for _, o := range m.st.Offers {
			if o.Name == a.Name && o.URL == a.URL {
				id := o.ID
				a.SourceLibraryID = &id
				break
			}
		}
	}
	return nil
}

func (m *memWriter) PutIcon(_ context.Context, appID, variant string, png []byte, w, h int, etag string) error {
	m.log("PutIcon %s %s", appID, variant)
	for i := range m.st.Apps {
		a := &m.st.Apps[i]
		if a.ID != appID {
			continue
		}
		for j := range a.Icons {
			if a.Icons[j].Variant == variant {
				a.Icons[j] = storage.TransferIcon{Variant: variant, ETag: etag, Bytes: png}
				return nil
			}
		}
		a.Icons = append(a.Icons, storage.TransferIcon{Variant: variant, ETag: etag, Bytes: png})
		sort.Slice(a.Icons, func(x, y int) bool { return a.Icons[x].Variant > a.Icons[y].Variant }) // light, dark
		return nil
	}
	return fmt.Errorf("no app %s", appID)
}

func (m *memWriter) SetSettings(_ context.Context, s storage.SystemSettings) error {
	m.log("SetSettings")
	m.st.Settings = s
	return nil
}

func (m *memWriter) CreateKid(_ context.Context, name, color string) (string, error) {
	id := m.id("kid")
	m.log("CreateKid %s", name)
	m.st.People = append(m.st.People, storage.TransferPerson{ID: id, Role: "kid", DisplayName: name, AvatarColor: color,
		ThemePref: "system", DensityPref: "large", ShowHealthBar: false, ShowUptimeDisplay: true,
		Favorites: []string{}, Order: []string{}, Collapsed: []string{}, Allowed: []string{}})
	return id, nil
}

func (m *memWriter) person(id string) *storage.TransferPerson {
	for i := range m.st.People {
		if m.st.People[i].ID == id {
			return &m.st.People[i]
		}
	}
	return nil
}

func (m *memWriter) SetPerson(_ context.Context, id string, p storage.TransferPersonRow) error {
	m.log("SetPerson %s", id)
	x := m.person(id)
	if x == nil {
		return fmt.Errorf("no person %s", id)
	}
	if x.Role != "kid" {
		x.DisplayName = ""
		if p.DisplayName != nil {
			x.DisplayName = *p.DisplayName
		}
	}
	x.AvatarColor = ""
	if p.AvatarColor != nil {
		x.AvatarColor = *p.AvatarColor
	}
	x.ThemePref, x.DensityPref, x.ShowHealthBar, x.ShowUptimeDisplay = p.ThemePref, p.DensityPref, p.ShowHealthBar, p.ShowUptimeDisplay
	return nil
}

func (m *memWriter) AddFavorites(_ context.Context, userID string, ids []string) error {
	m.log("AddFavorites %s %v", userID, ids)
	x := m.person(userID)
	have := map[string]bool{}
	for _, id := range x.Favorites {
		have[id] = true
	}
	for _, id := range ids {
		if !have[id] {
			x.Favorites = append(x.Favorites, id)
			have[id] = true
		}
	}
	return nil
}

func (m *memWriter) SetLayout(_ context.Context, userID string, ids []string) error {
	m.log("SetLayout %s", userID)
	m.person(userID).Order = append([]string{}, ids...)
	return nil
}

func (m *memWriter) SetCollapsed(_ context.Context, userID string, ids []string) error {
	m.log("SetCollapsed %s", userID)
	m.person(userID).Collapsed = append([]string{}, ids...)
	return nil
}

func (m *memWriter) SetAllowed(_ context.Context, kidID string, ids []string) error {
	m.log("SetAllowed %s", kidID)
	m.person(kidID).Allowed = append([]string{}, ids...)
	return nil
}

// state returns the state as LoadTransferState would: sections and offers by
// sort_index, apps by slug, and the caller's view order recomputed.
func (m *memWriter) state() *storage.TransferState {
	st := m.st
	sort.SliceStable(st.Sections, func(i, j int) bool { return st.Sections[i].SortIndex < st.Sections[j].SortIndex })
	sort.SliceStable(st.Offers, func(i, j int) bool { return st.Offers[i].SortIndex < st.Offers[j].SortIndex })
	sort.SliceStable(st.Apps, func(i, j int) bool { return st.Apps[i].Slug < st.Apps[j].Slug })
	st.CallerOrder = viewOrder(st, st.CallerID)
	return st
}

// viewOrder is ListServices' order for a viewer: saved layout first, then by
// name.
func viewOrder(st *storage.TransferState, viewer string) []string {
	var placed []string
	seen := map[string]bool{}
	exists := map[string]bool{}
	for _, a := range st.Apps {
		exists[a.ID] = true
	}
	for _, p := range st.People {
		if p.ID == viewer {
			for _, id := range p.Order {
				if exists[id] && !seen[id] {
					placed = append(placed, id)
					seen[id] = true
				}
			}
		}
	}
	rest := []storage.TransferApp{}
	for _, a := range st.Apps {
		if !seen[a.ID] {
			rest = append(rest, a)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].Name < rest[j].Name })
	for _, a := range rest {
		placed = append(placed, a.ID)
	}
	return placed
}

// apply runs a plan through the in-memory writer and returns the new state.
func apply(t *testing.T, m *memWriter, p *transfer.Plan) *storage.TransferState {
	t.Helper()
	require.NoError(t, transfer.Apply(context.Background(), m, p))
	return m.state()
}

// freshState is a new install: one admin (the owner and the caller), plus an
// adult who signed up with the fixture's email, and an empty catalog.
func freshState() *storage.TransferState {
	return &storage.TransferState{
		OwnerID:  "u-admin",
		CallerID: "u-admin",
		Settings: storage.SystemSettings{ShowUptimeDisplay: true},
		People: []storage.TransferPerson{
			adult("u-admin", "admin@example.com"),
			adult("u-alex", "Alex@Example.com"),
		},
		OtherAccounts: 1,
	}
}

func adult(id, email string) storage.TransferPerson {
	return storage.TransferPerson{ID: id, Role: "admin", Email: email, ThemePref: "system", DensityPref: "compact",
		ShowHealthBar: true, ShowUptimeDisplay: true,
		Favorites: []string{}, Order: []string{}, Collapsed: []string{}, Allowed: []string{}}
}

func strp(s string) *string { return &s }

func readFixture(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", rel))
	require.NoError(t, err)
	return b
}

func decodeFixture(t *testing.T) *transfer.Decoded {
	t.Helper()
	d, perr := transfer.DecodeBackup(readFixture(t, "backups/v1.json"))
	require.Nil(t, perr)
	return d
}

// items filters a plan's items by kind and action ("" matches any).
func items(p *transfer.Plan, kind, action string) []transfer.Item {
	var out []transfer.Item
	for _, it := range p.Items {
		if (kind == "" || it.Kind == kind) && (action == "" || it.Action == action) {
			out = append(out, it)
		}
	}
	return out
}

func item(t *testing.T, p *transfer.Plan, kind, name string) transfer.Item {
	t.Helper()
	for _, it := range p.Items {
		if it.Kind == kind && it.Name == name {
			return it
		}
	}
	t.Fatalf("no %s item named %q in plan: %+v", kind, name, p.Items)
	return transfer.Item{}
}

func names(its []transfer.Item) []string {
	out := []string{}
	for _, it := range its {
		out = append(out, it.Name)
	}
	return out
}

// assertEverySkipHasReason is AC-007's rule, checked on every plan a test makes.
func assertEverySkipHasReason(t *testing.T, p *transfer.Plan) {
	t.Helper()
	for _, it := range p.Items {
		if it.Action == transfer.ActionSkip {
			require.NotEmpty(t, it.Reason, "skip without a reason: %+v", it)
		}
	}
}

func zeroChanges(s transfer.Summary) bool {
	return s.Create == (transfer.CreateCounts{}) && s.Update == (transfer.UpdateCounts{}) && s.Delete == (transfer.DeleteCounts{})
}

func appBySlug(st *storage.TransferState, slug string) *storage.TransferApp {
	for i := range st.Apps {
		if st.Apps[i].Slug == slug {
			return &st.Apps[i]
		}
	}
	return nil
}

func sectionNames(st *storage.TransferState) []string {
	out := []string{}
	for _, s := range st.Sections {
		out = append(out, s.Name)
	}
	return out
}

func personByEmail(st *storage.TransferState, email string) *storage.TransferPerson {
	for i := range st.People {
		if strings.EqualFold(st.People[i].Email, email) {
			return &st.People[i]
		}
	}
	return nil
}
