package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// SPEC category-pane-width-layout — API integration for the atomic layout batch.
// PUT /api/categories/layout persists a whole-batch layout assignment (AC10) and
// the change survives a re-read (the server half of AC7). A batch naming a
// category the caller doesn't own rolls the whole thing back — no partial state.

type layoutCat struct {
	ID             string `json:"id"`
	LayoutRow      int    `json:"layoutRow"`
	LayoutColOrder int    `json:"layoutColOrder"`
	LayoutWidthPct int    `json:"layoutWidthPct"`
}

func getLayoutCats(t *testing.T, baseURL, token string) map[string]layoutCat {
	t.Helper()
	resp := doJSON(t, http.MethodGet, baseURL+"/api/categories", token, nil)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var payload struct {
		Categories []layoutCat `json:"categories"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	out := map[string]layoutCat{}
	for _, c := range payload.Categories {
		out[c.ID] = c
	}
	return out
}

// AC7/AC10 — a batch layout save returns 200 and persists across a fresh read.
func TestPutCategoryLayout_PersistsBatch(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	a := createCategory(t, s.URL, "admin-session", "Friends")
	b := createCategory(t, s.URL, "admin-session", "Development")

	resp := doJSON(t, http.MethodPut, s.URL+"/api/categories/layout", "admin-session", map[string]any{
		"layout": []layoutCat{
			{ID: a.ID, LayoutRow: 0, LayoutColOrder: 0, LayoutWidthPct: 50},
			{ID: b.ID, LayoutRow: 0, LayoutColOrder: 1, LayoutWidthPct: 50},
		},
	})
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "PUT /api/categories/layout must return 200")

	got := getLayoutCats(t, s.URL, "admin-session")
	assert.Equal(t, 0, got[a.ID].LayoutRow)
	assert.Equal(t, 0, got[b.ID].LayoutRow)
	assert.Equal(t, 1, got[b.ID].LayoutColOrder)
	assert.Equal(t, 50, got[a.ID].LayoutWidthPct)
	assert.Equal(t, 50, got[b.ID].LayoutWidthPct)
}

// AC10 — a batch with an unknown id is rejected atomically; nothing changes.
func TestPutCategoryLayout_AtomicRollback(t *testing.T) {
	s := testsupport.NewServer(t)
	defer s.Close()

	a := createCategory(t, s.URL, "admin-session", "Media")
	before := getLayoutCats(t, s.URL, "admin-session")[a.ID]

	resp := doJSON(t, http.MethodPut, s.URL+"/api/categories/layout", "admin-session", map[string]any{
		"layout": []layoutCat{
			{ID: a.ID, LayoutRow: 7, LayoutColOrder: 2, LayoutWidthPct: 25},
			{ID: "00000000-0000-0000-0000-000000000000", LayoutRow: 0, LayoutColOrder: 0, LayoutWidthPct: 100},
		},
	})
	resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode, "an unknown id fails the whole batch")

	after := getLayoutCats(t, s.URL, "admin-session")[a.ID]
	assert.Equal(t, before.LayoutRow, after.LayoutRow, "no partial update on rollback")
	assert.Equal(t, before.LayoutWidthPct, after.LayoutWidthPct)
}
