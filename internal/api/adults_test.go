package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/session"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// SPEC-household-manage-adults — an admin removes an adult or changes an
// adult's role: PATCH and DELETE /api/admin/accounts/{id}. The 401/403 of
// every route class is also probed in routes_test.go; these tests prove each
// AC through the real routes.

const userPassword = "stitch-user-pw" // the fixture's user@homepad.test

// addAdmin creates a second admin with a fixed-token session.
func addAdmin(t *testing.T, env *testsupport.Env, email string) (id, token string) {
	t.Helper()
	u, err := env.Store.CreateUser(context.Background(), email, "x", "admin")
	require.NoError(t, err)
	token = "admin-session-" + u.ID
	require.NoError(t, env.Sessions.Bind(context.Background(), token, u.ID, session.AdultLifetime))
	return u.ID, token
}

func accountPath(env *testsupport.Env, id string) string {
	return env.URL + "/api/admin/accounts/" + id
}

func patchRole(t *testing.T, env *testsupport.Env, token, id string, body any) (int, []byte) {
	t.Helper()
	r := doJSON(t, http.MethodPatch, accountPath(env, id), token, body)
	return r.StatusCode, readAll(t, r)
}

func removeAdult(t *testing.T, env *testsupport.Env, token, id string) (int, []byte) {
	t.Helper()
	r := doJSON(t, http.MethodDelete, accountPath(env, id), token, nil)
	return r.StatusCode, readAll(t, r)
}

// meStatus is GET /api/me with token: 200 while the session lives, 401 after.
func meStatus(t *testing.T, env *testsupport.Env, token string) int {
	t.Helper()
	r := doJSON(t, http.MethodGet, env.URL+"/api/me", token, nil)
	r.Body.Close()
	return r.StatusCode
}

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &e), "a 409 body is JSON: %s", body)
	return e.Error
}

func dbCount(t *testing.T, env *testsupport.Env, sql string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, env.Store.Pool().QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

func roleIn(t *testing.T, env *testsupport.Env, id string) string {
	t.Helper()
	u, err := env.Store.UserByID(context.Background(), id)
	require.NoError(t, err)
	return u.Role
}

// catalogSnapshot is every app and section as the household sees them.
func catalogSnapshot(t *testing.T, env *testsupport.Env) string {
	t.Helper()
	rows, err := env.Store.Pool().Query(context.Background(),
		`SELECT 's:' || id::text || ':' || slug || ':' || name || ':' || COALESCE(category_id::text, '') FROM services
		 UNION ALL SELECT 'c:' || id::text || ':' || name || ':' || sort_index FROM categories
		 UNION ALL SELECT 'k:' || user_id::text || ':' || service_id::text FROM kid_allowed_services
		 ORDER BY 1`)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	require.NoError(t, rows.Err())
	return strings.Join(out, "\n")
}

func favoritesOf(t *testing.T, env *testsupport.Env, userID string) []string {
	t.Helper()
	rows, err := env.Store.Pool().Query(context.Background(),
		`SELECT service_id::text FROM favorites WHERE user_id = $1 ORDER BY 1`, userID)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	return out
}

func mustOK(t *testing.T, r *http.Response, want int) {
	t.Helper()
	b := readAll(t, r)
	require.Equal(t, want, r.StatusCode, string(b))
}

// AC-001, AC-007 — removing an adult answers 204. Their favorites, order,
// folded sections, preferences and sessions go; the catalog stays. Their old
// cookie gets 401.
func TestAdults_RemoveAnAdult(t *testing.T) {
	env := testsupport.NewEnv(t)
	svc := firstServiceID(t, env.URL, "admin-session")
	cat := createCategory(t, env.URL, "admin-session", "Media")

	// The member's own state, through the routes they use.
	mustOK(t, doJSON(t, http.MethodPost, env.URL+"/api/favorites/"+svc, "non-admin-session", nil), http.StatusNoContent)
	mustOK(t, doJSON(t, http.MethodPut, env.URL+"/api/layout", "non-admin-session", map[string]any{"order": []string{svc}}), http.StatusNoContent)
	mustOK(t, doJSON(t, http.MethodPut, env.URL+"/api/me/collapsed-categories", "non-admin-session",
		map[string]any{"collapsed": []string{cat.ID}}), http.StatusNoContent)
	require.Equal(t, 1, dbCount(t, env, `SELECT count(*) FROM favorites WHERE user_id = $1`, env.UserID))
	before := catalogSnapshot(t, env)

	code, body := removeAdult(t, env, "admin-session", env.UserID)
	require.Equal(t, http.StatusNoContent, code, string(body))
	assert.Empty(t, body)

	_, err := env.Store.UserByID(context.Background(), env.UserID)
	assert.Error(t, err, "the row is gone")
	for _, table := range []string{"favorites", "user_layout", "user_collapsed_categories", "sessions"} {
		assert.Zero(t, dbCount(t, env, `SELECT count(*) FROM `+table+` WHERE user_id = $1`, env.UserID), "%s rows go with the account", table)
	}
	// AC-007 — every one of the fixture's sessions for that member is dead.
	for _, tok := range []string{"non-admin-session", "any-user", "session-one", "session-two"} {
		assert.Equal(t, http.StatusUnauthorized, meStatus(t, env, tok), "old cookie %s", tok)
	}
	assert.Equal(t, before, catalogSnapshot(t, env), "nothing in the shared app list changes")
	assert.Equal(t, http.StatusOK, meStatus(t, env, "admin-session"), "the acting admin stays signed in")

	// An admin can be removed too (Q5), by another admin.
	alexID, alexTok := addAdmin(t, env, "alex@homepad.test")
	code, body = removeAdult(t, env, "admin-session", alexID)
	require.Equal(t, http.StatusNoContent, code, string(body))
	assert.Equal(t, http.StatusUnauthorized, meStatus(t, env, alexTok))

	rows, _ := listAccounts(t, env)
	for _, a := range rows {
		assert.NotEqual(t, env.UserID, a.ID)
		assert.NotEqual(t, alexID, a.ID)
	}
}

// AC-002 — both routes: no session 401, member 403, kid 403. Nothing changes.
func TestAdults_AdminOnly(t *testing.T) {
	env := testsupport.NewEnv(t)
	_, kidTok := env.SeedKid(t, "Sam")
	alexID, _ := addAdmin(t, env, "alex@homepad.test")

	for _, target := range []string{alexID, env.AdminID} {
		anon := doNoCookie(t, http.MethodPatch, accountPath(env, target), map[string]any{"role": "user"})
		anon.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, anon.StatusCode, "PATCH with no session")
		anon = doNoCookie(t, http.MethodDelete, accountPath(env, target), nil)
		anon.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, anon.StatusCode, "DELETE with no session")

		for who, tok := range map[string]string{"member": "non-admin-session", "kid": kidTok} {
			code, _ := patchRole(t, env, tok, target, map[string]any{"role": "user"})
			assert.Equal(t, http.StatusForbidden, code, "PATCH as %s", who)
			code, _ = removeAdult(t, env, tok, target)
			assert.Equal(t, http.StatusForbidden, code, "DELETE as %s", who)
		}
	}
	// A member can't promote themselves either.
	code, _ := patchRole(t, env, "non-admin-session", env.UserID, map[string]any{"role": "admin"})
	assert.Equal(t, http.StatusForbidden, code)

	assert.Equal(t, "admin", roleIn(t, env, alexID))
	assert.Equal(t, "admin", roleIn(t, env, env.AdminID))
	assert.Equal(t, "user", roleIn(t, env, env.UserID))
}

// AC-003 / TC-005 — a kid's id, an unknown id and a malformed id are 404 on
// both routes. Kids keep DELETE /api/admin/kids/{id}.
func TestAdults_NotAnAdultIs404(t *testing.T) {
	env := testsupport.NewEnv(t)
	kidID, kidTok := env.SeedKid(t, "Sam")

	for _, id := range []string{kidID, "00000000-0000-0000-0000-0000000000ff", "not-a-uuid", "%27%3B--"} {
		code, body := patchRole(t, env, "admin-session", id, map[string]any{"role": "admin"})
		assert.Equal(t, http.StatusNotFound, code, "PATCH %s: %s", id, body)
		assert.NotEqual(t, muxNotFound, string(body), "PATCH %s: the handler answers, not the mux", id)
		code, body = removeAdult(t, env, "admin-session", id)
		assert.Equal(t, http.StatusNotFound, code, "DELETE %s: %s", id, body)
		assert.NotEqual(t, muxNotFound, string(body), "DELETE %s: the handler answers, not the mux", id)
	}
	_, err := env.Store.KidByID(context.Background(), kidID)
	require.NoError(t, err, "the kid is still there")
	assert.Equal(t, "kid", roleIn(t, env, kidID), "a kid can never be promoted through the adult route")
	assert.Equal(t, http.StatusOK, meStatus(t, env, kidTok))
}

// AC-004 / TC-007 — removing an adult revokes the invites they made; the link
// stops working. Other admins' invites stay.
func TestAdults_RemovalRevokesTheirInvites(t *testing.T) {
	env := testsupport.NewEnv(t)
	alexID, alexTok := addAdmin(t, env, "alex@homepad.test")

	code, pat, b := createInvite(t, env, alexTok, map[string]any{"name": "Pat", "role": "user"})
	require.Equal(t, http.StatusCreated, code, string(b))
	mine := mustInvite(t, env, "Lee", "user")

	code, body := removeAdult(t, env, "admin-session", alexID)
	require.Equal(t, http.StatusNoContent, code, string(body))

	code, list, b := listInvites(t, env, "admin-session")
	require.Equal(t, http.StatusOK, code, string(b))
	require.Len(t, list, 1, "Alex's invite is gone from Pending invites: %s", b)
	assert.Equal(t, mine.ID, list[0].ID)

	code, _ = lookupInvite(t, env, pat.Token)
	assert.Equal(t, http.StatusNotFound, code, "the removed adult's link doesn't work anymore")
	code, _ = lookupInvite(t, env, mine.Token)
	assert.Equal(t, http.StatusOK, code, "another admin's link still works")
}

// AC-005 — an admin acting on their own id gets 409 {"error":"self"} on both
// routes, and nothing changes.
func TestAdults_SelfIsRefused(t *testing.T) {
	env := testsupport.NewEnv(t)
	addAdmin(t, env, "alex@homepad.test") // so "last admin" is not the reason

	for _, role := range []string{"user", "admin"} {
		code, body := patchRole(t, env, "admin-session", env.AdminID, map[string]any{"role": role})
		require.Equal(t, http.StatusConflict, code, string(body))
		assert.Equal(t, "self", errorCode(t, body))
	}
	code, body := removeAdult(t, env, "admin-session", env.AdminID)
	require.Equal(t, http.StatusConflict, code, string(body))
	assert.Equal(t, "self", errorCode(t, body))

	assert.Equal(t, "admin", roleIn(t, env, env.AdminID))
	assert.Equal(t, http.StatusOK, meStatus(t, env, "admin-session"), "still signed in")
}

// TC-003 — with only one admin, that admin's own row is refused as self; a
// second admin can demote the first, who then can't reach the route at all.
func TestAdults_TheOnlyAdmin(t *testing.T) {
	env := testsupport.NewEnv(t)
	code, body := patchRole(t, env, "admin-session", env.AdminID, map[string]any{"role": "user"})
	require.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "self", errorCode(t, body))

	alexID, alexTok := addAdmin(t, env, "alex@homepad.test")
	code, body = patchRole(t, env, alexTok, env.AdminID, map[string]any{"role": "user"})
	require.Equal(t, http.StatusOK, code, string(body))

	// The first admin's sessions ended (Q2). Signed in again, a member: 403.
	require.NoError(t, env.Sessions.Bind(context.Background(), "caleb-again", env.AdminID, session.AdultLifetime))
	code, _ = patchRole(t, env, "caleb-again", alexID, map[string]any{"role": "user"})
	assert.Equal(t, http.StatusForbidden, code)
	assert.Equal(t, "admin", roleIn(t, env, alexID))
}

// AC-006 / TC-005 — {"role":"admin"} or {"role":"user"} changes the role and
// answers 200 with the account row. Anything else is 400. The same role is
// 200 and ends no sessions.
func TestAdults_PatchRole(t *testing.T) {
	env := testsupport.NewEnv(t)

	code, body := patchRole(t, env, "admin-session", env.UserID, map[string]any{"role": "admin"})
	require.Equal(t, http.StatusOK, code, string(body))
	var row accountView
	require.NoError(t, json.Unmarshal(body, &row), string(body))
	assert.Equal(t, env.UserID, row.ID)
	assert.Equal(t, "admin", row.Role)
	assert.Equal(t, "user@homepad.test", row.Email)
	assert.Equal(t, "user@homepad.test", row.Name, "an adult with no display name shows the email, as in the list")
	assert.Nil(t, row.AppCount, "adult rows carry no kid fields")
	assert.Nil(t, row.LastSeenAt, "every session ended, so nothing is live")
	assert.Equal(t, "admin", roleIn(t, env, env.UserID))

	// The same shape as GET /api/admin/accounts.
	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	list, _ := listAccounts(t, env)
	_, listBody := listAccounts(t, env)
	var rawList []map[string]any
	require.NoError(t, json.Unmarshal(listBody, &rawList))
	for _, r := range rawList {
		if r["id"] == env.UserID {
			assert.Equal(t, keysOfAny(r), keysOfAny(raw), "PATCH answers with the list's row shape")
		}
	}
	require.NotEmpty(t, list)

	code, body = patchRole(t, env, "admin-session", env.UserID, map[string]any{"role": "user"})
	require.Equal(t, http.StatusOK, code, string(body))
	assert.Equal(t, "user", roleIn(t, env, env.UserID))

	// Same role: 200, nothing changes, no sessions end.
	require.NoError(t, env.Sessions.Bind(context.Background(), "jo-live", env.UserID, session.AdultLifetime))
	code, body = patchRole(t, env, "admin-session", env.UserID, map[string]any{"role": "user"})
	require.Equal(t, http.StatusOK, code, string(body))
	require.NoError(t, json.Unmarshal(body, &row))
	assert.Equal(t, "user", row.Role)
	assert.Equal(t, http.StatusOK, meStatus(t, env, "jo-live"), "a no-op ends no sessions")

	for name, b := range map[string]any{
		"kid role":           map[string]any{"role": "kid"},
		"unknown role":       map[string]any{"role": "owner"},
		"empty role":         map[string]any{"role": ""},
		"null role":          map[string]any{"role": nil},
		"number role":        map[string]any{"role": 1},
		"no role":            map[string]any{},
		"another field":      map[string]any{"role": "admin", "email": "x"},
		"only another field": map[string]any{"name": "Jo"},
		"not an object":      []string{"admin"},
	} {
		code, body := patchRole(t, env, "admin-session", env.UserID, b)
		assert.Equal(t, http.StatusBadRequest, code, "%s: %s", name, body)
	}
	// No body at all.
	req, _ := http.NewRequest(http.MethodPatch, accountPath(env, env.UserID), nil)
	req.AddCookie(&http.Cookie{Name: "homepad_session", Value: "admin-session"})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "no body")

	assert.Equal(t, "user", roleIn(t, env, env.UserID), "no refused body changed anything")
	assert.Equal(t, http.StatusOK, meStatus(t, env, "jo-live"), "no refused body ended a session")
}

// AC-008 / TC-002 — a role change ends every session of that account. After
// signing in again they get the screens of the new role.
func TestAdults_RoleChangeEndsEverySession(t *testing.T) {
	env := testsupport.NewEnv(t)
	jo := []string{"non-admin-session", "any-user", "session-one", "session-two", "real-once-impl-exists"}
	for _, tok := range jo {
		require.Equal(t, http.StatusOK, meStatus(t, env, tok))
	}

	code, body := patchRole(t, env, "admin-session", env.UserID, map[string]any{"role": "admin"})
	require.Equal(t, http.StatusOK, code, string(body))
	for _, tok := range jo {
		assert.Equal(t, http.StatusUnauthorized, meStatus(t, env, tok), "session %s ended", tok)
	}
	assert.Zero(t, dbCount(t, env, `SELECT count(*) FROM sessions WHERE user_id = $1`, env.UserID))
	assert.Equal(t, http.StatusOK, meStatus(t, env, "admin-session"), "the acting admin's session is not touched")

	// Sign in again: now an admin.
	b, _ := json.Marshal(map[string]string{"email": "user@homepad.test", "password": userPassword})
	resp := doNoCookie(t, http.MethodPost, env.URL+"/api/login", json.RawMessage(b))
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var me struct {
		Role string `json:"role"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&me))
	assert.Equal(t, "admin", me.Role)
	ck := sessionCookieFrom(resp)
	require.NotNil(t, ck)
	r := doJSON(t, http.MethodGet, env.URL+"/api/admin/accounts", ck.Value, nil)
	r.Body.Close()
	assert.Equal(t, http.StatusOK, r.StatusCode, "the new admin reaches Administration")

	// And back: the demoted admin's sessions end too.
	code, _ = patchRole(t, env, "admin-session", env.UserID, map[string]any{"role": "user"})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, http.StatusUnauthorized, meStatus(t, env, ck.Value))
}

// AC-009 / TC-004 — two admins demote each other at the same moment over
// HTTP: exactly one succeeds and one admin remains. The loser is refused as
// last_admin when both reach the account lock. If the winner finished first,
// the loser is already a member (403) or already signed out (401).
func TestAdults_OppositeDemotionsOverHTTP(t *testing.T) {
	env := testsupport.NewEnv(t)
	logs := captureLog(t)
	a, aTok := env.AdminID, "admin-session"

	conflicts := 0
	for round := 0; round < 8; round++ {
		b, bTok := addAdmin(t, env, fmt.Sprintf("b%d@homepad.test", round))
		var wg sync.WaitGroup
		start := make(chan struct{})
		codes := make([]int, 2)
		bodies := make([][]byte, 2)
		for i, p := range [][2]string{{aTok, b}, {bTok, a}} {
			wg.Add(1)
			go func(i int, tok, target string) {
				defer wg.Done()
				<-start
				codes[i], bodies[i] = patchRole(t, env, tok, target, map[string]any{"role": "user"})
			}(i, p[0], p[1])
		}
		close(start)
		wg.Wait()

		sort.Ints(codes)
		require.Equal(t, http.StatusOK, codes[0], "round %d: exactly one succeeds (%v)", round, codes)
		require.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusConflict}, codes[1], "round %d: %v", round, codes)
		for _, body := range bodies {
			if strings.HasPrefix(string(body), "{") && strings.Contains(string(body), "error") {
				assert.Equal(t, "last_admin", errorCode(t, body))
				conflicts++
			}
		}
		require.Equal(t, 1, dbCount(t, env, `SELECT count(*) FROM users WHERE role = 'admin'`), "round %d: one admin remains", round)

		// The survivor carries on as "a" with a live session.
		if roleIn(t, env, a) != "admin" {
			a = b
		}
		aTok = fmt.Sprintf("survivor-%d", round)
		require.NoError(t, env.Sessions.Bind(context.Background(), aTok, a, session.AdultLifetime))
	}
	t.Logf("%d of 8 rounds reached the lock on both sides (409 last_admin)", conflicts)
	if conflicts > 0 {
		assert.Contains(t, logs.String(), "refused: would leave no admin (PATCH /api/admin/accounts/{id})")
	}
}

// AC-011 — removing or demoting the first admin leaves the catalog, every
// kid's allow-list and every other account's favorites as they were
// (SPEC-household-catalog-owner removed the catalog owner).
func TestAdults_FirstAdminLeavesTheCatalogAlone(t *testing.T) {
	env := testsupport.NewEnv(t)
	cat := createCategory(t, env.URL, "admin-session", "Media")
	svc := firstServiceID(t, env.URL, "admin-session")
	mustOK(t, doJSON(t, http.MethodPatch, env.URL+"/api/services/"+svc, "admin-session",
		map[string]any{"categoryId": cat.ID}), http.StatusOK)
	env.SeedKid(t, "Sam", svc)
	mustOK(t, doJSON(t, http.MethodPost, env.URL+"/api/favorites/"+svc, "non-admin-session", nil), http.StatusNoContent)
	mustOK(t, doJSON(t, http.MethodPost, env.URL+"/api/favorites/"+svc, "admin-session", nil), http.StatusNoContent)
	before := catalogSnapshot(t, env)
	favs := favoritesOf(t, env, env.UserID)
	require.Len(t, favs, 1)
	require.Contains(t, before, "k:", "the kid's allow-list is part of the snapshot")

	_, alexTok := addAdmin(t, env, "alex@homepad.test")
	code, body := patchRole(t, env, alexTok, env.AdminID, map[string]any{"role": "user"})
	require.Equal(t, http.StatusOK, code, string(body))
	assert.Equal(t, before, catalogSnapshot(t, env), "demoting the first admin leaves the catalog")
	assert.Equal(t, favs, favoritesOf(t, env, env.UserID))

	code, body = removeAdult(t, env, alexTok, env.AdminID)
	require.Equal(t, http.StatusNoContent, code, string(body))
	assert.Equal(t, before, catalogSnapshot(t, env), "removing the first admin leaves the catalog")
	assert.Equal(t, favs, favoritesOf(t, env, env.UserID))

	// The catalog still reads and writes for the admin who is left.
	r := doJSON(t, http.MethodGet, env.URL+"/api/services", alexTok, nil)
	b := readAll(t, r)
	require.Equal(t, http.StatusOK, r.StatusCode)
	assert.Contains(t, string(b), svc)
	created := doJSON(t, http.MethodPost, env.URL+"/api/categories", alexTok, map[string]any{"name": "Network"})
	created.Body.Close()
	assert.Equal(t, http.StatusCreated, created.StatusCode)
}

// AC-013 — every action writes one §3 log line, with ids only: no email and
// no token.
func TestAdults_AuditLines(t *testing.T) {
	env := testsupport.NewEnv(t)
	alexID, alexTok := addAdmin(t, env, "alex@homepad.test")
	code, inv, b := createInvite(t, env, alexTok, map[string]any{"name": "Pat", "role": "user"})
	require.Equal(t, http.StatusCreated, code, string(b))
	logs := captureLog(t)

	code, _ = patchRole(t, env, "admin-session", env.UserID, map[string]any{"role": "admin"})
	require.Equal(t, http.StatusOK, code)
	code, _ = removeAdult(t, env, "admin-session", alexID)
	require.Equal(t, http.StatusNoContent, code)

	out := logs.String()
	assert.Contains(t, out, fmt.Sprintf("accounts: admin %s changed role of %s from user to admin", env.AdminID, env.UserID))
	assert.Contains(t, out, fmt.Sprintf("accounts: admin %s removed adult %s role=admin", env.AdminID, alexID))
	assert.Contains(t, out, fmt.Sprintf("invite: revoked 1 pending invites made by removed adult %s", alexID))
	assert.Equal(t, 1, strings.Count(out, "changed role of"), "one line per role change")
	assert.Equal(t, 1, strings.Count(out, "removed adult"), "one line per removal")

	// A same-role PATCH changes nothing and logs no change.
	code, _ = patchRole(t, env, "admin-session", env.UserID, map[string]any{"role": "admin"})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, 1, strings.Count(logs.String(), "changed role of"))

	out = logs.String()
	for _, secret := range []string{"alex@homepad.test", "user@homepad.test", "admin@homepad.test",
		"admin-session", alexTok, "non-admin-session", inv.Token} {
		assert.NotContains(t, out, secret, "log lines hold ids only")
	}
}

// AC-014 — a removal frees a place under HOMEPAD_MAX_ACCOUNTS.
func TestAdults_RemovalFreesAPlace(t *testing.T) {
	env := testsupport.NewEnvWith(t, testsupport.Options{MaxAccounts: 3}) // admin, user, alice
	code, _ := createKid(t, env, map[string]any{"name": "Sam"})
	require.Equal(t, http.StatusConflict, code, "at the limit")

	alice, err := env.Store.UserByEmail(context.Background(), "alice@example.com")
	require.NoError(t, err)
	code2, body := removeAdult(t, env, "admin-session", alice.ID)
	require.Equal(t, http.StatusNoContent, code2, string(body))

	code, _ = createKid(t, env, map[string]any{"name": "Sam"})
	assert.Equal(t, http.StatusCreated, code, "the removed adult's place is free")
}

// AC-015 — kids, their allow-lists and their sessions are unchanged by any
// removal or role change.
func TestAdults_KidsAreUntouched(t *testing.T) {
	env := testsupport.NewEnv(t)
	svc := firstServiceID(t, env.URL, "admin-session")
	kidID, kidTok := env.SeedKid(t, "Sam", svc)
	_, kidTok2 := env.SeedKid(t, "Leo")
	before := catalogSnapshot(t, env)

	code, _ := patchRole(t, env, "admin-session", env.UserID, map[string]any{"role": "admin"})
	require.Equal(t, http.StatusOK, code)
	code, _ = patchRole(t, env, "admin-session", env.UserID, map[string]any{"role": "user"})
	require.Equal(t, http.StatusOK, code)
	code, _ = removeAdult(t, env, "admin-session", env.UserID)
	require.Equal(t, http.StatusNoContent, code)

	assert.Equal(t, before, catalogSnapshot(t, env), "allow-lists unchanged")
	assert.Equal(t, http.StatusOK, meStatus(t, env, kidTok), "the kid's device stays signed in")
	assert.Equal(t, http.StatusOK, meStatus(t, env, kidTok2))
	assert.Equal(t, "kid", roleIn(t, env, kidID))
	ids, err := env.Store.KidServiceIDs(context.Background(), kidID)
	require.NoError(t, err)
	assert.Equal(t, []string{svc}, ids)
}
