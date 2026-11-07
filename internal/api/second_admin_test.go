package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// SPEC-household-basics scope 3 (#87) — every admin edits the one shared
// catalog. A second admin (an admin who is not the first, catalog-owning one)
// must get the same status and the same effect as the owner on every catalog
// write, and the change must show for everyone (AC-023).
//
// AC-025: the route list comes from the router. Every route server.go marks
// catalog-write must have a probe in secondAdminProbes, and every probe must
// name such a route. A new catalog route nobody probed fails the build.

// secondAdminWorld is the NewEnv fixture plus a second admin's session.
type secondAdminWorld struct {
	env    *testsupport.Env
	second string // the second admin's session token
}

// catalogViewers are the accounts that must all see a second admin's change:
// the owner, a member, and the second admin themself.
var catalogViewers = []string{"admin-session", "non-admin-session"}

type iconService struct {
	ID              string  `json:"id"`
	Slug            string  `json:"slug"`
	Name            string  `json:"name"`
	CategoryID      *string `json:"categoryId"`
	IconLight       bool    `json:"iconLight"`
	SourceLibraryID *string `json:"sourceLibraryId"`
}

func catalogOf(t *testing.T, w secondAdminWorld, token string) map[string]iconService {
	t.Helper()
	code, body := getBody(t, w.env.URL+"/api/services", token)
	require.Equal(t, http.StatusOK, code)
	var payload struct {
		Services []iconService `json:"services"`
	}
	require.NoError(t, json.Unmarshal(body, &payload))
	out := map[string]iconService{}
	for _, s := range payload.Services {
		out[s.ID] = s
	}
	return out
}

func categoryNames(t *testing.T, w secondAdminWorld, token string) []string {
	t.Helper()
	var names []string
	for _, c := range getCategories(t, w.env.URL, token) {
		names = append(names, c.Name)
	}
	return names
}

// everyoneSees runs check against the catalog as every viewer, the second
// admin included.
func everyoneSees(t *testing.T, w secondAdminWorld, check func(who string, svcs map[string]iconService)) {
	t.Helper()
	for _, who := range append(append([]string{}, catalogViewers...), w.second) {
		check(who, catalogOf(t, w, who))
	}
}

func status(t *testing.T, r *http.Response) int {
	t.Helper()
	_, _ = io.Copy(io.Discard, r.Body)
	r.Body.Close()
	return r.StatusCode
}

// secondAdminProbes is keyed by route pattern. Each probe writes as the
// second admin, asserts the owner's status code for that write, and checks
// the change in GET /api/services or GET /api/categories for every viewer.
var secondAdminProbes = map[string]func(t *testing.T, w secondAdminWorld){
	"POST /api/services": func(t *testing.T, w secondAdminWorld) {
		r := doJSON(t, http.MethodPost, w.env.URL+"/api/services", w.second, map[string]any{
			"slug": "by-second", "name": "By Second", "url": "https://second.example.com",
		})
		var sv iconService
		require.Equal(t, http.StatusCreated, r.StatusCode)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&sv))
		r.Body.Close()
		everyoneSees(t, w, func(who string, svcs map[string]iconService) {
			assert.Equal(t, "By Second", svcs[sv.ID].Name, "%s sees the second admin's new app", who)
		})
	},
	"PATCH /api/services/{id}": func(t *testing.T, w secondAdminWorld) {
		id := firstServiceID(t, w.env.URL, "admin-session")
		cat := createCategory(t, w.env.URL, "admin-session", "Owner Cat")
		r := doJSON(t, http.MethodPatch, w.env.URL+"/api/services/"+id, w.second, map[string]any{
			"name": "Renamed By Second", "categoryId": cat.ID,
		})
		require.Equal(t, http.StatusOK, status(t, r))
		everyoneSees(t, w, func(who string, svcs map[string]iconService) {
			assert.Equal(t, "Renamed By Second", svcs[id].Name, who)
			require.NotNil(t, svcs[id].CategoryID, who)
			assert.Equal(t, cat.ID, *svcs[id].CategoryID, "%s: the owner's category can be assigned", who)
		})
	},
	"DELETE /api/services/{id}": func(t *testing.T, w secondAdminWorld) {
		id := firstServiceID(t, w.env.URL, "admin-session")
		require.Equal(t, http.StatusNoContent, status(t, doJSON(t, http.MethodDelete, w.env.URL+"/api/services/"+id, w.second, nil)))
		everyoneSees(t, w, func(who string, svcs map[string]iconService) {
			assert.NotContains(t, svcs, id, "%s: the deleted app is gone", who)
		})
	},
	"POST /api/categories": func(t *testing.T, w secondAdminWorld) {
		r := doJSON(t, http.MethodPost, w.env.URL+"/api/categories", w.second, map[string]any{"name": "Second's Cat"})
		require.Equal(t, http.StatusCreated, status(t, r))
		for _, who := range append(append([]string{}, catalogViewers...), w.second) {
			assert.Contains(t, categoryNames(t, w, who), "Second's Cat", who)
		}
	},
	"PUT /api/categories/order": func(t *testing.T, w secondAdminWorld) {
		a := createCategory(t, w.env.URL, "admin-session", "Alpha")
		b := createCategory(t, w.env.URL, "admin-session", "Bravo")
		r := doJSON(t, http.MethodPut, w.env.URL+"/api/categories/order", w.second, map[string]any{"order": []string{b.ID, a.ID}})
		require.Equal(t, http.StatusNoContent, status(t, r))
		assert.Equal(t, []string{"Bravo", "Alpha"}, categoryNames(t, w, "admin-session"), "the new order shows for the owner")
		assert.Equal(t, []string{"Bravo", "Alpha"}, categoryNames(t, w, w.second))
	},
	"PATCH /api/categories/{id}": func(t *testing.T, w secondAdminWorld) {
		c := createCategory(t, w.env.URL, "admin-session", "Before")
		r := doJSON(t, http.MethodPatch, w.env.URL+"/api/categories/"+c.ID, w.second, map[string]any{"name": "After"})
		require.Equal(t, http.StatusOK, status(t, r))
		assert.Equal(t, []string{"After"}, categoryNames(t, w, "admin-session"))
		assert.Equal(t, []string{"After"}, categoryNames(t, w, w.second))
	},
	"DELETE /api/categories/{id}": func(t *testing.T, w secondAdminWorld) {
		c := createCategory(t, w.env.URL, "admin-session", "Doomed")
		r := doJSON(t, http.MethodDelete, w.env.URL+"/api/categories/"+c.ID, w.second, nil)
		require.Equal(t, http.StatusNoContent, status(t, r))
		assert.NotContains(t, categoryNames(t, w, "admin-session"), "Doomed", "the delete really happened")
	},
	"PUT /api/services/{id}/icon/{variant}": func(t *testing.T, w secondAdminWorld) {
		id := firstServiceID(t, w.env.URL, "admin-session")
		require.Equal(t, http.StatusNoContent, status(t, putIcon(t, w.env.URL, w.second, id, "light", pngBytes(t, 32, 32))))
		everyoneSees(t, w, func(who string, svcs map[string]iconService) {
			assert.True(t, svcs[id].IconLight, "%s sees the second admin's icon", who)
		})
	},
	"DELETE /api/services/{id}/icon/{variant}": func(t *testing.T, w secondAdminWorld) {
		id := firstServiceID(t, w.env.URL, "admin-session")
		require.Equal(t, http.StatusNoContent, status(t, putIcon(t, w.env.URL, "admin-session", id, "light", pngBytes(t, 32, 32))))
		r := doJSON(t, http.MethodDelete, w.env.URL+"/api/services/"+id+"/icon/light", w.second, nil)
		require.Equal(t, http.StatusNoContent, status(t, r))
		everyoneSees(t, w, func(who string, svcs map[string]iconService) {
			assert.False(t, svcs[id].IconLight, "%s: the icon is gone", who)
		})
	},
	"POST /api/services/{id}/fetch-icon": func(t *testing.T, w secondAdminWorld) {
		png := pngBytes(t, 32, 32)
		target := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/":
				rw.Header().Set("Content-Type", "text/html")
				_, _ = io.WriteString(rw, `<html><head><link rel="icon" href="/brand.png"></head></html>`)
			case "/brand.png":
				rw.Header().Set("Content-Type", "image/png")
				_, _ = rw.Write(png)
			default:
				http.NotFound(rw, r)
			}
		}))
		t.Cleanup(target.Close)
		id := createServiceForFetch(t, w.env.URL, "admin-session", target.URL)
		r := doJSON(t, http.MethodPost, w.env.URL+"/api/services/"+id+"/fetch-icon", w.second, nil)
		require.Equal(t, http.StatusOK, status(t, r))
		everyoneSees(t, w, func(who string, svcs map[string]iconService) {
			assert.True(t, svcs[id].IconLight, "%s sees the fetched icon", who)
		})
	},
	"POST /api/library/{id}/add": func(t *testing.T, w secondAdminWorld) {
		offer := createOffer(t, w.env.URL, "admin-session", "Jellyfin", "https://jellyfin.example.com")
		cat := createCategory(t, w.env.URL, "admin-session", "Media")
		r := addFromLibrary(t, w.env.URL, w.second, offer.ID, map[string]any{"categoryId": cat.ID})
		require.Equal(t, http.StatusCreated, r.StatusCode)
		sv := decodeService(t, r)
		everyoneSees(t, w, func(who string, svcs map[string]iconService) {
			got, ok := svcs[sv.ID]
			require.True(t, ok, "%s sees the app the second admin added", who)
			require.NotNil(t, got.SourceLibraryID)
			assert.Equal(t, offer.ID, *got.SourceLibraryID)
			require.NotNil(t, got.CategoryID)
			assert.Equal(t, cat.ID, *got.CategoryID, "%s: placed in the owner's category", who)
		})
	},
}

func newSecondAdminWorld(t *testing.T) secondAdminWorld {
	t.Helper()
	env := testsupport.NewEnv(t)
	return secondAdminWorld{env: env, second: seedSecondAdmin(t, env)}
}

// AC-025 — every catalog-write route in the router has a second-admin probe,
// and every probe names a catalog-write route.
func TestSecondAdmin_EveryCatalogWriteRouteIsProbed(t *testing.T) {
	var catalogWrites []string
	for _, r := range allRoutes(t) {
		if r.Class != "catalog-write" {
			continue
		}
		catalogWrites = append(catalogWrites, r.Pattern)
		_, ok := secondAdminProbes[r.Pattern]
		assert.True(t, ok, "catalog-write route %q has no second-admin probe in secondAdminProbes. "+
			"Add one that writes as a second admin and checks the change in GET /api/services or /api/categories.", r.Pattern)
	}
	sort.Strings(catalogWrites)
	for p := range secondAdminProbes {
		assert.Contains(t, catalogWrites, p, "secondAdminProbes lists %q, which server.go does not register as catalog-write", p)
	}
	require.Len(t, catalogWrites, 11, "the AC-023 list: services ×3, categories ×4, icons ×3, library add")
}

// AC-023 — a second admin gets the owner's status and effect on every
// catalog write.
func TestSecondAdmin_CatalogWrites(t *testing.T) {
	patterns := make([]string, 0, len(secondAdminProbes))
	for p := range secondAdminProbes {
		patterns = append(patterns, p)
	}
	sort.Strings(patterns)
	for _, p := range patterns {
		probe := secondAdminProbes[p]
		t.Run(p, func(t *testing.T) {
			probe(t, newSecondAdminWorld(t))
		})
	}
}

// AC-024 — GET /api/library's `added` hint reads the shared catalog for every
// caller: the owner, a second admin and a member.
func TestSecondAdmin_LibraryAddedHintForEveryCaller(t *testing.T) {
	w := newSecondAdminWorld(t)
	added := createOffer(t, w.env.URL, "admin-session", "Plex", "https://plex.example.com")
	notAdded := createOffer(t, w.env.URL, "admin-session", "Kavita", "https://kavita.example.com")
	r := addFromLibrary(t, w.env.URL, "admin-session", added.ID, nil)
	require.Equal(t, http.StatusCreated, status(t, r))

	for _, who := range []string{"admin-session", w.second, "non-admin-session"} {
		hints := map[string]bool{}
		for _, o := range getLibrary(t, w.env.URL, who) {
			hints[o.ID] = o.Added
		}
		assert.True(t, hints[added.ID], "%s: the shared catalog has Plex, so it reads added", who)
		assert.False(t, hints[notAdded.ID], "%s: Kavita is not in the catalog", who)
	}

	// And an add by the second admin reads added for the owner.
	r = addFromLibrary(t, w.env.URL, w.second, notAdded.ID, nil)
	require.Equal(t, http.StatusCreated, status(t, r))
	for _, o := range getLibrary(t, w.env.URL, "admin-session") {
		if o.ID == notAdded.ID {
			assert.True(t, o.Added, "the owner sees the second admin's add as added")
		}
	}
}
