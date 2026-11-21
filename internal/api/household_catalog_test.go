package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// SPEC-household-catalog-owner — the catalog belongs to the household. No
// account owns it, so it outlives every account, including the last admin.

func slugsOf(svcs []fullService) []string {
	out := make([]string, 0, len(svcs))
	for _, sv := range svcs {
		out = append(out, sv.Slug)
	}
	return out
}

// AC-003, AC-014, TC-002 — with every admin row deleted by hand, every catalog
// read still answers the catalog: apps, sections, icons, the Library's added
// hint, folded sections, and a kid's allow-list.
func TestHouseholdCatalog_ReadsWorkWithNoAdmin(t *testing.T) {
	env := testsupport.NewEnv(t)
	ctx := context.Background()
	cat := createCategory(t, env.URL, "admin-session", "Media")
	gitea := firstSlugID(t, env, "gitea")
	r := doJSON(t, http.MethodPatch, env.URL+"/api/services/"+gitea, "admin-session", map[string]any{"categoryId": cat.ID})
	r.Body.Close()
	require.Equal(t, http.StatusOK, r.StatusCode)
	r = putIcon(t, env.URL, "admin-session", gitea, "light", pngBytes(t, 32, 32))
	r.Body.Close()
	require.Equal(t, http.StatusNoContent, r.StatusCode)
	offer := createOffer(t, env.URL, "admin-session", "Jellyfin", "https://jellyfin.example.com")
	r = addFromLibrary(t, env.URL, "admin-session", offer.ID, nil)
	r.Body.Close()
	require.Equal(t, http.StatusCreated, r.StatusCode)
	r = putCollapsed(t, env.URL, "non-admin-session", []string{cat.ID}, true)
	r.Body.Close()
	require.Equal(t, http.StatusNoContent, r.StatusCode)
	_, kidTok := env.SeedKid(t, "Maya", gitea)

	_, err := env.Store.Pool().Exec(ctx, `DELETE FROM users WHERE role = 'admin'`)
	require.NoError(t, err)

	svcs := listServicesFull(t, env.URL, "non-admin-session")
	assert.ElementsMatch(t, []string{"gitea", "grafana", "jellyfin"}, slugsOf(svcs), "a member reads the whole catalog")
	cats := getCategories(t, env.URL, "non-admin-session")
	require.Len(t, cats, 1)
	assert.Equal(t, "Media", cats[0].Name)
	code, body := getBody(t, env.URL+"/api/services/"+gitea+"/icon/light", "non-admin-session")
	assert.Equal(t, http.StatusOK, code)
	assert.NotEmpty(t, body)
	code, folded := getCollapsed(t, env.URL, "non-admin-session")
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, []string{cat.ID}, folded)
	lib := getLibrary(t, env.URL, "non-admin-session")
	require.Len(t, lib, 1)
	assert.True(t, lib[0].Added, "AC-014: an app in the catalog came from this offer")
	code, _ = getBody(t, env.URL+"/api/status", "non-admin-session")
	assert.Equal(t, http.StatusOK, code)

	assert.Equal(t, []string{"gitea"}, slugsOf(listServicesFull(t, env.URL, kidTok)), "the kid's allow-list is untouched")
	code, kidCats := getBody(t, env.URL+"/api/categories", kidTok)
	assert.Equal(t, http.StatusOK, code)
	assert.Contains(t, string(kidCats), "Media")

	// AC-001 — deleting the admins deleted no app, section or icon.
	assert.Equal(t, 3, countRows(t, env, `SELECT count(*) FROM services`))
	assert.Equal(t, 1, countRows(t, env, `SELECT count(*) FROM categories`))
	assert.Equal(t, 1, countRows(t, env, `SELECT count(*) FROM service_icons`))
}

// AC-003 — every catalog write works with no admin present. No route can
// make one then (they are admin-only), so this drives the storage writes the
// routes use, and the backup load the export and restore routes use.
func TestHouseholdCatalog_WritesWorkWithNoAdmin(t *testing.T) {
	env := testsupport.NewEnv(t)
	ctx := context.Background()
	_, err := env.Store.Pool().Exec(ctx, `DELETE FROM users WHERE role = 'admin'`)
	require.NoError(t, err)

	cat, err := env.Store.CreateCategory(ctx, "Tools")
	require.NoError(t, err)
	_, err = env.Store.RenameCategory(ctx, cat.ID, "Tooling")
	require.NoError(t, err)
	_, err = env.Store.SetCategoryWidth(ctx, cat.ID, 6)
	require.NoError(t, err)
	require.NoError(t, env.Store.SetCategoryOrder(ctx, []string{cat.ID}))
	sv, err := env.Store.CreateService(ctx, storage.Service{Slug: "notes", Name: "Notes", URL: "https://notes.example.com"})
	require.NoError(t, err)
	_, err = env.Store.UpdateService(ctx, sv.ID, storage.ServiceUpdate{SetCategory: true, CategoryID: &cat.ID})
	require.NoError(t, err)
	require.NoError(t, env.Store.PutIcon(ctx, sv.ID, "light", pngBytes(t, 32, 32), 32, 32, "etag"))
	require.NoError(t, env.Store.DeleteIcon(ctx, sv.ID, "light"))
	url, err := env.Store.ServiceURL(ctx, sv.ID)
	require.NoError(t, err)
	assert.Equal(t, "https://notes.example.com", url)

	st, err := env.Store.LoadTransferState(ctx, storage.TransferLoad{CallerID: env.UserID, People: true})
	require.NoError(t, err)
	assert.Len(t, st.Apps, 3)
	assert.Len(t, st.Sections, 1)

	assert.ElementsMatch(t, []string{"gitea", "grafana", "notes"}, slugsOf(listServicesFull(t, env.URL, "non-admin-session")))
	require.NoError(t, env.Store.DeleteService(ctx, sv.ID))
	require.NoError(t, env.Store.DeleteCategory(ctx, cat.ID))
	assert.ElementsMatch(t, []string{"gitea", "grafana"}, slugsOf(listServicesFull(t, env.URL, "non-admin-session")))
}

// AC-002 — a slug and a section name are unique across the catalog, whichever
// admin adds them: a second admin's duplicate is a 409, not a hidden copy.
func TestHouseholdCatalog_SlugsAndSectionNamesAreUnique(t *testing.T) {
	env := testsupport.NewEnv(t)
	second := seedSecondAdmin(t, env)
	createCategory(t, env.URL, "admin-session", "Media")

	r := doJSON(t, http.MethodPost, env.URL+"/api/categories", second, map[string]any{"name": "Media"})
	r.Body.Close()
	assert.Equal(t, http.StatusConflict, r.StatusCode)
	r = doJSON(t, http.MethodPost, env.URL+"/api/services", second, map[string]any{"slug": "gitea", "name": "Gitea 2", "url": "https://g.example.com"})
	r.Body.Close()
	assert.Equal(t, http.StatusConflict, r.StatusCode)
}

// AC-015 — the kid allow-list takes any catalog app, whichever admin added it,
// and refuses an id that names no app.
func TestHouseholdCatalog_KidAllowListTakesAnyCatalogApp(t *testing.T) {
	env := testsupport.NewEnv(t)
	second := seedSecondAdmin(t, env)
	byFirst := firstSlugID(t, env, "gitea")
	bySecond := createServiceCA(t, env.URL, second, map[string]any{"slug": "immich", "name": "Immich", "url": "https://immich.example.com"}).ID
	kidID, _ := env.SeedKid(t, "Maya")

	put := func(token string, ids ...string) int {
		r := doJSON(t, http.MethodPut, env.URL+"/api/admin/kids/"+kidID+"/services", token, map[string]any{"serviceIds": ids})
		r.Body.Close()
		return r.StatusCode
	}
	assert.Equal(t, http.StatusNoContent, put("admin-session", byFirst, bySecond))
	assert.Equal(t, http.StatusNoContent, put(second, byFirst, bySecond))
	_, ids := getKidServices(t, env, kidID)
	assert.ElementsMatch(t, []string{byFirst, bySecond}, ids)
	assert.Equal(t, http.StatusBadRequest, put("admin-session", byFirst, "11111111-1111-1111-1111-111111111111"))
}

// AC-012, AC-013, TC-004 — a 17.1.0 (v1) file and the same file at v2 get the
// same preview in both modes; the v1 file applies in both modes; the export
// after it is v2.
func TestHouseholdCatalog_V1BackupRestoresInBothModes(t *testing.T) {
	v1 := fixtureBytes(t, "backups/v1.json")
	v2 := fixtureBytes(t, "backups/v2.json")
	for _, mode := range []string{"merge", "replace"} {
		t.Run(mode, func(t *testing.T) {
			env := testsupport.NewEnv(t)
			p1 := previewRestore(t, env.URL, "admin-session", mode, v1)
			p2 := previewRestore(t, env.URL, "admin-session", mode, v2)
			assert.Equal(t, p2.Summary, p1.Summary)
			assert.Equal(t, p2.Items, p1.Items)
			assert.Equal(t, p2.PlanHash, p1.PlanHash)

			code, b := applyRestore(t, env.URL, "admin-session", mode, p1.PlanHash, v1)
			require.Equal(t, http.StatusOK, code, "%s", b)
			if mode == "merge" {
				again := previewRestore(t, env.URL, "admin-session", mode, v1)
				assert.True(t, again.noChanges(), "%+v", again.Summary)
			} else {
				assert.Equal(t, 5, countRows(t, env, `SELECT count(*) FROM services`), "AC-013: only the file's apps are left")
			}

			resp, out := getExport(t, env.URL, "admin-session", "")
			require.Equal(t, http.StatusOK, resp.StatusCode)
			var head struct {
				SchemaVersion int `json:"schemaVersion"`
			}
			require.NoError(t, json.NewDecoder(bytes.NewReader(out)).Decode(&head))
			assert.Equal(t, 2, head.SchemaVersion)
		})
	}
}
