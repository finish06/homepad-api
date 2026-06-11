package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// v4 app-categories — API integration (A1–A8). Drives the /api/categories CRUD
// + reorder surface and the extended PATCH /api/services category assignment
// against the real test Postgres via testsupport.NewServer.

type categoryItem struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	SortIndex int    `json:"sortIndex"`
}

type serviceItem struct {
	ID           string  `json:"id"`
	CategoryID   *string `json:"categoryId"`
	CategoryName *string `json:"categoryName"`
}

func doJSON(t *testing.T, method, url, token string, body any) *http.Response {
	t.Helper()
	var r *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	} else {
		r = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, url, r)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "homepad_session", Value: token})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func getCategories(t *testing.T, baseURL, token string) []categoryItem {
	t.Helper()
	resp := doJSON(t, http.MethodGet, baseURL+"/api/categories", token, nil)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "GET /api/categories must return 200")
	var payload struct {
		Categories []categoryItem `json:"categories"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	return payload.Categories
}

func getServicesFull(t *testing.T, baseURL, token string) []serviceItem {
	t.Helper()
	resp := doJSON(t, http.MethodGet, baseURL+"/api/services", token, nil)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var payload struct {
		Services []serviceItem `json:"services"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	return payload.Services
}

func createCategory(t *testing.T, baseURL, token, name string) categoryItem {
	t.Helper()
	resp := doJSON(t, http.MethodPost, baseURL+"/api/categories", token, map[string]any{"name": name})
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode, "POST /api/categories must return 201")
	var c categoryItem
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&c))
	return c
}

// A1 — admin creates a category; it appears in GET; duplicate name → 409.
func TestAdminCanCreateCategory_AndDuplicate409(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	c := createCategory(t, s.URL, "admin-session", "Media")
	assert.Equal(t, "Media", c.Name)
	assert.Equal(t, 0, c.SortIndex, "first category appends at sortIndex 0")

	cats := getCategories(t, s.URL, "admin-session")
	require.Len(t, cats, 1)
	assert.Equal(t, "Media", cats[0].Name)

	dup := doJSON(t, http.MethodPost, s.URL+"/api/categories", "admin-session", map[string]any{"name": "Media"})
	defer dup.Body.Close()
	assert.Equal(t, http.StatusConflict, dup.StatusCode, "duplicate category name must be 409")
}

// A2 — a non-admin gets 403 on every mutating verb (create/rename/reorder/delete)
// and on assigning a service's category.
func TestNonAdmin_403_OnEveryCategoryMutation(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	// Seed a real category + service as admin for the rename/reorder/delete/assign attempts.
	cat := createCategory(t, s.URL, "admin-session", "Media")
	svc := getServicesFull(t, s.URL, "admin-session")[0]

	cases := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"create", http.MethodPost, "/api/categories", map[string]any{"name": "Infra"}},
		{"rename", http.MethodPatch, "/api/categories/" + cat.ID, map[string]any{"name": "Infra"}},
		{"reorder", http.MethodPut, "/api/categories/order", map[string]any{"order": []string{cat.ID}}},
		{"delete", http.MethodDelete, "/api/categories/" + cat.ID, nil},
		{"assign", http.MethodPatch, "/api/services/" + svc.ID, map[string]any{"categoryId": cat.ID}},
	}
	for _, tc := range cases {
		resp := doJSON(t, tc.method, s.URL+tc.path, "non-admin-session", tc.body)
		resp.Body.Close()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode,
			"non-admin %s must return 403", tc.name)
	}
}

// A3 — admin renames a category (200); rename to an existing name → 409; unknown id → 404.
func TestAdminCanRenameCategory_409_404(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	media := createCategory(t, s.URL, "admin-session", "Media")
	createCategory(t, s.URL, "admin-session", "Infra")

	ok := doJSON(t, http.MethodPatch, s.URL+"/api/categories/"+media.ID, "admin-session", map[string]any{"name": "Movies"})
	defer ok.Body.Close()
	require.Equal(t, http.StatusOK, ok.StatusCode)
	var got categoryItem
	require.NoError(t, json.NewDecoder(ok.Body).Decode(&got))
	assert.Equal(t, "Movies", got.Name)

	collide := doJSON(t, http.MethodPatch, s.URL+"/api/categories/"+media.ID, "admin-session", map[string]any{"name": "Infra"})
	collide.Body.Close()
	assert.Equal(t, http.StatusConflict, collide.StatusCode, "rename onto an existing name must be 409")

	bogus := doJSON(t, http.MethodPatch, s.URL+"/api/categories/11111111-1111-1111-1111-111111111111", "admin-session", map[string]any{"name": "X"})
	bogus.Body.Close()
	assert.Equal(t, http.StatusNotFound, bogus.StatusCode, "renaming an unknown id must be 404")
}

// A4 — admin reorders via PUT /api/categories/order; GET reflects the new order.
func TestAdminCanReorderCategories(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	a := createCategory(t, s.URL, "admin-session", "A")
	b := createCategory(t, s.URL, "admin-session", "B")
	c := createCategory(t, s.URL, "admin-session", "C")

	resp := doJSON(t, http.MethodPut, s.URL+"/api/categories/order", "admin-session",
		map[string]any{"order": []string{c.ID, a.ID, b.ID}})
	resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode, "PUT /api/categories/order must return 204")

	cats := getCategories(t, s.URL, "admin-session")
	require.Len(t, cats, 3)
	assert.Equal(t, "C", cats[0].Name)
	assert.Equal(t, "A", cats[1].Name)
	assert.Equal(t, "B", cats[2].Name)
}

// A5 — admin assigns a service to a category and clears it back to Uncategorized
// via PATCH /api/services/{id} (categoryId: id / null).
func TestAdminCanAssignAndClearServiceCategory(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	cat := createCategory(t, s.URL, "admin-session", "Media")
	svc := getServicesFull(t, s.URL, "admin-session")[0]
	require.Nil(t, svc.CategoryID, "seeded service starts Uncategorized")

	assign := doJSON(t, http.MethodPatch, s.URL+"/api/services/"+svc.ID, "admin-session", map[string]any{"categoryId": cat.ID})
	assign.Body.Close()
	require.Equal(t, http.StatusOK, assign.StatusCode)

	after := findService(t, getServicesFull(t, s.URL, "admin-session"), svc.ID)
	require.NotNil(t, after.CategoryID)
	assert.Equal(t, cat.ID, *after.CategoryID)
	require.NotNil(t, after.CategoryName)
	assert.Equal(t, "Media", *after.CategoryName, "categoryName is denormalized onto the tile")

	// Clear back to Uncategorized via explicit null.
	clear := doJSON(t, http.MethodPatch, s.URL+"/api/services/"+svc.ID, "admin-session", map[string]any{"categoryId": nil})
	clear.Body.Close()
	require.Equal(t, http.StatusOK, clear.StatusCode)

	cleared := findService(t, getServicesFull(t, s.URL, "admin-session"), svc.ID)
	assert.Nil(t, cleared.CategoryID, "categoryId null after clear")
	assert.Nil(t, cleared.CategoryName, "categoryName null after clear")
}

// A6 — assigning a categoryId that names no category → 400; the service is unchanged.
func TestAssignUnknownCategory_400_ServiceUnchanged(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	svc := getServicesFull(t, s.URL, "admin-session")[0]

	resp := doJSON(t, http.MethodPatch, s.URL+"/api/services/"+svc.ID, "admin-session",
		map[string]any{"categoryId": "11111111-1111-1111-1111-111111111111"})
	resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "unknown categoryId must be 400")

	after := findService(t, getServicesFull(t, s.URL, "admin-session"), svc.ID)
	assert.Nil(t, after.CategoryID, "a rejected assignment must leave the service Uncategorized")
}

// A7 — deleting a category sets its apps Uncategorized (FK SET NULL) — no service
// deleted; deleting again is idempotent (204).
func TestDeleteCategory_UnassignsServices_Idempotent(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	cat := createCategory(t, s.URL, "admin-session", "Media")
	svcs := getServicesFull(t, s.URL, "admin-session")
	require.GreaterOrEqual(t, len(svcs), 2, "fixture seeds at least two services")

	for _, sv := range svcs {
		resp := doJSON(t, http.MethodPatch, s.URL+"/api/services/"+sv.ID, "admin-session", map[string]any{"categoryId": cat.ID})
		resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}

	del := doJSON(t, http.MethodDelete, s.URL+"/api/categories/"+cat.ID, "admin-session", nil)
	del.Body.Close()
	require.Equal(t, http.StatusNoContent, del.StatusCode, "DELETE /api/categories/{id} must return 204")

	after := getServicesFull(t, s.URL, "admin-session")
	assert.Len(t, after, len(svcs), "no service may be deleted when its category is")
	for _, sv := range after {
		assert.Nil(t, sv.CategoryID, "every app falls back to Uncategorized after its category is deleted")
	}

	// Idempotent: deleting the now-gone category again is still 204.
	again := doJSON(t, http.MethodDelete, s.URL+"/api/categories/"+cat.ID, "admin-session", nil)
	again.Body.Close()
	assert.Equal(t, http.StatusNoContent, again.StatusCode, "deleting an absent category is idempotent 204")
}

// A8 — GET /api/services carries categoryId/categoryName per tile (null when
// Uncategorized) and is otherwise unchanged.
func TestServicesList_CarriesCategoryFields(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	svcs := getServicesFull(t, s.URL, "admin-session")
	require.NotEmpty(t, svcs)
	for _, sv := range svcs {
		assert.Nil(t, sv.CategoryID, "uncategorized tile reports null categoryId")
		assert.Nil(t, sv.CategoryName, "uncategorized tile reports null categoryName")
	}

	// GET /api/categories is session-gated (any logged-in user), empty by default.
	require.Empty(t, getCategories(t, s.URL, "non-admin-session"),
		"GET /api/categories is readable by any session and starts empty")
}

func findService(t *testing.T, svcs []serviceItem, id string) serviceItem {
	t.Helper()
	for _, sv := range svcs {
		if sv.ID == id {
			return sv
		}
	}
	t.Fatalf("service %s not found in catalog", id)
	return serviceItem{}
}
