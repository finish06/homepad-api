package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/session"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// SPEC-kid-accounts §7 — the admin Accounts API (plan TASK-301…304; AC-001,
// AC-002, AC-009, AC-012, AC-014). Every route is admin-only; the 403s for a
// kid and a plain user on each are proven route by route in routes_test.go.

type accountView struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Role        string     `json:"role"`
	LastSeenAt  *time.Time `json:"lastSeenAt"`
	Email       string     `json:"email"`
	AvatarColor string     `json:"avatarColor"`
	AppCount    *int       `json:"appCount"`
	DeviceCount *int       `json:"deviceCount"`
}

type deviceView struct {
	ID         string    `json:"id"`
	UserAgent  string    `json:"userAgent"`
	CreatedAt  time.Time `json:"createdAt"`
	LastSeenAt time.Time `json:"lastSeenAt"`
}

func readAll(t *testing.T, r *http.Response) []byte {
	t.Helper()
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	return b
}

func createKid(t *testing.T, env *testsupport.Env, body map[string]any) (int, accountView) {
	t.Helper()
	r := doJSON(t, http.MethodPost, env.URL+"/api/admin/kids", "admin-session", body)
	b := readAll(t, r)
	var v accountView
	if r.StatusCode == http.StatusCreated {
		require.NoError(t, json.Unmarshal(b, &v), string(b))
	}
	return r.StatusCode, v
}

func listAccounts(t *testing.T, env *testsupport.Env) ([]accountView, []byte) {
	t.Helper()
	r := doJSON(t, http.MethodGet, env.URL+"/api/admin/accounts", "admin-session", nil)
	b := readAll(t, r)
	require.Equal(t, http.StatusOK, r.StatusCode, string(b))
	var out []accountView
	require.NoError(t, json.Unmarshal(b, &out), "the accounts list is a JSON array (§7): %s", b)
	return out, b
}

// AC-001 — an admin creates a kid with only a name. No email, no password.
func TestAdminKids_CreateWithOnlyAName(t *testing.T) {
	env := testsupport.NewEnv(t)

	code, kid := createKid(t, env, map[string]any{"name": "  Sam  "})
	require.Equal(t, http.StatusCreated, code)
	assert.NotEmpty(t, kid.ID)
	assert.Equal(t, "Sam", kid.Name, "the name is trimmed")
	assert.Equal(t, "kid", kid.Role)
	assert.Equal(t, "blue", kid.AvatarColor, "a default color is chosen when none is sent")
	assert.Nil(t, kid.LastSeenAt, "a new kid has never been seen")
	require.NotNil(t, kid.AppCount)
	assert.Zero(t, *kid.AppCount, "OQ-1: a new kid sees nothing until apps are ticked")

	u, err := env.Store.KidByID(context.Background(), kid.ID)
	require.NoError(t, err)
	assert.Equal(t, "", u.Email)
	assert.Equal(t, "", u.PasswordHash)
	assert.False(t, u.ShowHealthBar, "AC-019")
	assert.Equal(t, "large", u.DensityPref, "UX K-3")

	code, kid2 := createKid(t, env, map[string]any{"name": "Leo", "avatarColor": "purple"})
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, "purple", kid2.AvatarColor)
}

// UX §3 — name 1–40 characters, unique among kids; color from the accent set;
// nothing but name and avatarColor accepted.
func TestAdminKids_CreateValidation(t *testing.T) {
	env := testsupport.NewEnv(t)
	code, _ := createKid(t, env, map[string]any{"name": "Maya"})
	require.Equal(t, http.StatusCreated, code)

	forty := strings.Repeat("é", 40) // 40 characters, 80 bytes
	code, k := createKid(t, env, map[string]any{"name": forty})
	assert.Equal(t, http.StatusCreated, code, "40 characters is allowed, counted in characters not bytes")
	assert.Equal(t, forty, k.Name)

	for _, tc := range []struct {
		name string
		body map[string]any
		want int
	}{
		{"missing name", map[string]any{}, http.StatusBadRequest},
		{"empty name", map[string]any{"name": ""}, http.StatusBadRequest},
		{"blank name", map[string]any{"name": "   "}, http.StatusBadRequest},
		{"41 characters", map[string]any{"name": strings.Repeat("a", 41)}, http.StatusBadRequest},
		{"control character", map[string]any{"name": "Ma\nya"}, http.StatusBadRequest},
		{"non-string name", map[string]any{"name": 7}, http.StatusBadRequest},
		{"bad color", map[string]any{"name": "Zed", "avatarColor": "#ff0000"}, http.StatusBadRequest},
		{"duplicate", map[string]any{"name": "Maya"}, http.StatusConflict},
		{"duplicate, other case", map[string]any{"name": "mAYA"}, http.StatusConflict},
		{"password field", map[string]any{"name": "Pat", "password": "hunter2hunter2"}, http.StatusBadRequest},
		{"email field", map[string]any{"name": "Pat", "email": "pat@example.com"}, http.StatusBadRequest},
		{"role field", map[string]any{"name": "Pat", "role": "admin"}, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _ := createKid(t, env, tc.body)
			assert.Equal(t, tc.want, code)
		})
	}
	accts, _ := listAccounts(t, env)
	kids := 0
	for _, a := range accts {
		if a.Role == "kid" {
			kids++
		}
		assert.NotEqual(t, "Pat", a.Name, "a rejected create wrote nothing")
	}
	assert.Equal(t, 2, kids)
}

// §7 — GET /api/admin/accounts lists every account: adults (listing only, with
// their email) and kids (with app and device counts). lastSeenAt is the most
// recent live session.
func TestAdminAccounts_ListsEveryAccount(t *testing.T) {
	env := testsupport.NewEnv(t)
	svc := env.SeedService(t, "jellyfin", "Jellyfin", "")
	kidID, _ := env.SeedKid(t, "Maya", svc)
	_, err := env.Store.CreateKid(context.Background(), "Leo", "red")
	require.NoError(t, err)

	accts, body := listAccounts(t, env)
	byName := map[string]accountView{}
	for _, a := range accts {
		byName[a.Name] = a
	}
	require.Len(t, accts, 5, "admin, user, alice, Maya, Leo")
	assert.Equal(t, "admin", accts[0].Role, "the admin is listed first")

	admin := byName["admin@homepad.test"]
	assert.Equal(t, env.AdminID, admin.ID)
	assert.Equal(t, "admin", admin.Role)
	assert.Equal(t, "admin@homepad.test", admin.Email)
	assert.NotNil(t, admin.LastSeenAt, "the fixture admin has a live session")
	assert.Nil(t, admin.AppCount, "app counts are a kid thing")

	maya := byName["Maya"]
	assert.Equal(t, kidID, maya.ID)
	assert.Equal(t, "kid", maya.Role)
	assert.Equal(t, "", maya.Email)
	assert.Equal(t, "blue", maya.AvatarColor)
	require.NotNil(t, maya.AppCount)
	assert.Equal(t, 1, *maya.AppCount)
	require.NotNil(t, maya.DeviceCount)
	assert.Equal(t, 1, *maya.DeviceCount)
	assert.NotNil(t, maya.LastSeenAt)

	leo := byName["Leo"]
	assert.Equal(t, "red", leo.AvatarColor)
	require.NotNil(t, leo.DeviceCount)
	assert.Zero(t, *leo.DeviceCount)
	assert.Nil(t, leo.LastSeenAt, "never signed in")

	// Never a credential.
	for _, tok := range []string{"admin-session", "non-admin-session", "kid-session-" + kidID} {
		assertNoTokenMaterial(t, body, tok)
	}
	assert.NotContains(t, string(body), "$2a$", "no password hash")
}

// assertNoTokenMaterial fails if body carries tok in any form: raw, or its
// sha256 in hex or base64.
func assertNoTokenMaterial(t *testing.T, body []byte, tok string) {
	t.Helper()
	sum := sha256.Sum256([]byte(tok))
	for _, form := range []string{
		tok,
		hex.EncodeToString(sum[:]),
		base64.StdEncoding.EncodeToString(sum[:]),
		base64.RawURLEncoding.EncodeToString(sum[:]),
	} {
		assert.NotContains(t, string(body), form, "the response must never carry a session token or its hash")
	}
}

func patchKid(t *testing.T, env *testsupport.Env, id string, body any) (int, accountView) {
	t.Helper()
	r := doJSON(t, http.MethodPatch, env.URL+"/api/admin/kids/"+id, "admin-session", body)
	b := readAll(t, r)
	var v accountView
	if r.StatusCode == http.StatusOK {
		require.NoError(t, json.Unmarshal(b, &v), string(b))
	}
	return r.StatusCode, v
}

// §7 — PATCH /api/admin/kids/{id}: name and color. It never touches an adult.
func TestAdminKids_Patch(t *testing.T) {
	env := testsupport.NewEnv(t)
	kidID, _ := env.SeedKid(t, "Maya")
	_, err := env.Store.CreateKid(context.Background(), "Leo", "red")
	require.NoError(t, err)

	code, v := patchKid(t, env, kidID, map[string]any{"name": " Maia "})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "Maia", v.Name)
	assert.Equal(t, "blue", v.AvatarColor, "an absent field is unchanged")

	code, v = patchKid(t, env, kidID, map[string]any{"avatarColor": "green"})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "Maia", v.Name)
	assert.Equal(t, "green", v.AvatarColor)

	code, _ = patchKid(t, env, kidID, map[string]any{"name": "maia"})
	assert.Equal(t, http.StatusOK, code, "renaming to a case variant of its own name is fine")

	for _, tc := range []struct {
		name string
		id   string
		body any
		want int
	}{
		{"duplicate name", kidID, map[string]any{"name": "LEO"}, http.StatusConflict},
		{"empty body", kidID, map[string]any{}, http.StatusBadRequest},
		{"blank name", kidID, map[string]any{"name": " "}, http.StatusBadRequest},
		{"long name", kidID, map[string]any{"name": strings.Repeat("x", 41)}, http.StatusBadRequest},
		{"bad color", kidID, map[string]any{"avatarColor": "plaid"}, http.StatusBadRequest},
		{"password (not supported)", kidID, map[string]any{"password": "correct horse battery"}, http.StatusBadRequest},
		{"unknown field", kidID, map[string]any{"role": "admin"}, http.StatusBadRequest},
		{"adult id", env.AdminID, map[string]any{"name": "Pwned"}, http.StatusNotFound},
		{"unknown id", "00000000-0000-0000-0000-000000000000", map[string]any{"name": "X"}, http.StatusNotFound},
		{"malformed id", "nope", map[string]any{"name": "X"}, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _ := patchKid(t, env, tc.id, tc.body)
			assert.Equal(t, tc.want, code)
		})
	}

	kid, err := env.Store.KidByID(context.Background(), kidID)
	require.NoError(t, err)
	assert.Equal(t, "maia", kid.DisplayName)
	assert.Equal(t, "", kid.PasswordHash, "no password was set")
	admin, err := env.Store.UserByID(context.Background(), env.AdminID)
	require.NoError(t, err)
	assert.Equal(t, "admin", admin.Role)
	assert.NotEqual(t, "Pwned", admin.DisplayName, "PATCH on an adult id changed nothing")
}

// AC-014 — DELETE removes the kid and everything it owns; never an adult.
func TestAdminKids_Delete(t *testing.T) {
	env := testsupport.NewEnv(t)
	svc := env.SeedService(t, "jellyfin", "Jellyfin", "")
	kidID, kidTok := env.SeedKid(t, "Maya", svc)
	fav := doJSON(t, http.MethodPost, env.URL+"/api/favorites/"+svc, kidTok, nil)
	fav.Body.Close()
	require.Equal(t, http.StatusNoContent, fav.StatusCode)

	adult := doJSON(t, http.MethodDelete, env.URL+"/api/admin/kids/"+env.UserID, "admin-session", nil)
	adult.Body.Close()
	assert.Equal(t, http.StatusNotFound, adult.StatusCode, "an adult is not a kid")
	code, _ := getMeRaw(t, env.URL, "non-admin-session")
	assert.Equal(t, http.StatusOK, code, "the adult and its session survive")

	del := doJSON(t, http.MethodDelete, env.URL+"/api/admin/kids/"+kidID, "admin-session", nil)
	del.Body.Close()
	assert.Equal(t, http.StatusNoContent, del.StatusCode)

	code, _ = getMeRaw(t, env.URL, kidTok)
	assert.Equal(t, http.StatusUnauthorized, code, "the deleted kid's devices are signed out")
	ctx := context.Background()
	for _, tbl := range []string{"sessions", "favorites", "kid_allowed_services", "users"} {
		col := "user_id"
		if tbl == "users" {
			col = "id"
		}
		var n int
		require.NoError(t, env.Store.Pool().QueryRow(ctx, `SELECT count(*) FROM `+tbl+` WHERE `+col+` = $1`, kidID).Scan(&n))
		assert.Zero(t, n, "%s rows are gone", tbl)
	}

	again := doJSON(t, http.MethodDelete, env.URL+"/api/admin/kids/"+kidID, "admin-session", nil)
	again.Body.Close()
	assert.Equal(t, http.StatusNotFound, again.StatusCode)
}

func getKidServices(t *testing.T, env *testsupport.Env, id string) (int, []string) {
	t.Helper()
	r := doJSON(t, http.MethodGet, env.URL+"/api/admin/kids/"+id+"/services", "admin-session", nil)
	b := readAll(t, r)
	var v struct {
		ServiceIDs []string `json:"serviceIds"`
	}
	if r.StatusCode == http.StatusOK {
		require.NoError(t, json.Unmarshal(b, &v), string(b))
		require.NotNil(t, v.ServiceIDs, "serviceIds is an array, never null: %s", b)
	}
	return r.StatusCode, v.ServiceIDs
}

// AC-002 — PUT/GET the allow-list: whole-set replace; unknown ids → 400.
func TestAdminKids_Services(t *testing.T) {
	env := testsupport.NewEnv(t)
	a := env.SeedService(t, "jellyfin", "Jellyfin", "")
	b := env.SeedService(t, "immich", "Immich", "")
	kidID, kidTok := env.SeedKid(t, "Maya")

	code, ids := getKidServices(t, env, kidID)
	require.Equal(t, http.StatusOK, code)
	assert.Empty(t, ids)

	put := func(body any) int {
		r := doJSON(t, http.MethodPut, env.URL+"/api/admin/kids/"+kidID+"/services", "admin-session", body)
		r.Body.Close()
		return r.StatusCode
	}
	require.Equal(t, http.StatusNoContent, put(map[string]any{"serviceIds": []string{a, b}}))
	_, ids = getKidServices(t, env, kidID)
	assert.ElementsMatch(t, []string{a, b}, ids)
	assert.Len(t, listServicesFull(t, env.URL, kidTok), 2, "the kid sees the change on its next read, no sign-out")

	require.Equal(t, http.StatusNoContent, put(map[string]any{"serviceIds": []string{b}}))
	_, ids = getKidServices(t, env, kidID)
	assert.Equal(t, []string{b}, ids, "whole-set replace")

	// The fixture plain user owns its own (non-shared) gitea row.
	var userSvc string
	require.NoError(t, env.Store.Pool().QueryRow(context.Background(),
		`SELECT id FROM services WHERE user_id = $1 LIMIT 1`, env.UserID).Scan(&userSvc))

	for _, body := range []any{
		map[string]any{"serviceIds": []string{a, "00000000-0000-0000-0000-000000000000"}},
		map[string]any{"serviceIds": []string{"nope"}},
		map[string]any{"serviceIds": []string{userSvc}},
		map[string]any{"serviceIds": nil},
		map[string]any{},
		map[string]any{"serviceIds": "a"},
		map[string]any{"serviceIds": []string{a}, "extra": true},
	} {
		assert.Equal(t, http.StatusBadRequest, put(body), "%v", body)
	}
	_, ids = getKidServices(t, env, kidID)
	assert.Equal(t, []string{b}, ids, "rejected PUTs changed nothing")

	require.Equal(t, http.StatusNoContent, put(map[string]any{"serviceIds": []string{}}))
	_, ids = getKidServices(t, env, kidID)
	assert.Empty(t, ids, "saving zero apps is allowed (UX §3)")

	for _, id := range []string{env.UserID, "00000000-0000-0000-0000-000000000000", "nope"} {
		code, _ := getKidServices(t, env, id)
		assert.Equal(t, http.StatusNotFound, code, "GET services for non-kid %s", id)
		r := doJSON(t, http.MethodPut, env.URL+"/api/admin/kids/"+id+"/services", "admin-session",
			map[string]any{"serviceIds": []string{a}})
		r.Body.Close()
		assert.Equal(t, http.StatusNotFound, r.StatusCode, "PUT services for non-kid %s", id)
	}
}

func kidSessions(t *testing.T, env *testsupport.Env, id string) (int, []deviceView, []byte) {
	t.Helper()
	r := doJSON(t, http.MethodGet, env.URL+"/api/admin/kids/"+id+"/sessions", "admin-session", nil)
	b := readAll(t, r)
	var out []deviceView
	if r.StatusCode == http.StatusOK {
		require.NoError(t, json.Unmarshal(b, &out), "the sessions list is a JSON array (§7): %s", b)
	}
	return r.StatusCode, out, b
}

// AC-012 — list a kid's sessions and revoke any of them; the revoked device
// gets 401 on its next request. The id is an opaque handle, never the token.
func TestAdminKids_SessionsListAndRevoke(t *testing.T) {
	env := testsupport.NewEnv(t)
	kidID, tokA := env.SeedKid(t, "Maya")
	ctx := context.Background()
	tokB := "maya-ipad-two"
	require.NoError(t, env.Sessions.Bind(ctx, tokB, kidID, session.KidLifetime))
	otherKid, otherTok := env.SeedKid(t, "Leo")

	code, devs, body := kidSessions(t, env, kidID)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, devs, 2)
	for _, d := range devs {
		assert.NotEmpty(t, d.ID)
		assert.False(t, d.CreatedAt.IsZero())
		assert.False(t, d.LastSeenAt.IsZero())
	}
	assertNoTokenMaterial(t, body, tokA)
	assertNoTokenMaterial(t, body, tokB)
	var raw []map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	for _, m := range raw {
		assert.ElementsMatch(t, []string{"id", "userAgent", "createdAt", "lastSeenAt"}, keysOfAny(m),
			"exactly the §7 fields, nothing that could authenticate")
	}

	// Which handle is tokB's? Revoke each in turn and see which device drops.
	_, otherDevs, _ := kidSessions(t, env, otherKid)
	require.Len(t, otherDevs, 1)
	cross := doJSON(t, http.MethodDelete, env.URL+"/api/admin/kids/"+kidID+"/sessions/"+otherDevs[0].ID, "admin-session", nil)
	cross.Body.Close()
	assert.Equal(t, http.StatusNotFound, cross.StatusCode, "a handle is scoped to the kid in the path")
	code, _ = getMeRaw(t, env.URL, otherTok)
	assert.Equal(t, http.StatusOK, code, "the other kid's device survives")

	rev := doJSON(t, http.MethodDelete, env.URL+"/api/admin/kids/"+kidID+"/sessions/"+devs[0].ID, "admin-session", nil)
	rev.Body.Close()
	require.Equal(t, http.StatusNoContent, rev.StatusCode)
	codeA, _ := getMeRaw(t, env.URL, tokA)
	codeB, _ := getMeRaw(t, env.URL, tokB)
	assert.ElementsMatch(t, []int{http.StatusOK, http.StatusUnauthorized}, []int{codeA, codeB},
		"AC-012: exactly the revoked device gets 401 on its next request")

	again := doJSON(t, http.MethodDelete, env.URL+"/api/admin/kids/"+kidID+"/sessions/"+devs[0].ID, "admin-session", nil)
	again.Body.Close()
	assert.Equal(t, http.StatusNotFound, again.StatusCode)

	// An adult's sessions are not reachable through the kids routes.
	code, _, _ = kidSessions(t, env, env.UserID)
	assert.Equal(t, http.StatusNotFound, code)
	adultHandles, err := env.Sessions.ListForUser(ctx, env.UserID)
	require.NoError(t, err)
	require.NotEmpty(t, adultHandles)
	r := doJSON(t, http.MethodDelete, env.URL+"/api/admin/kids/"+env.UserID+"/sessions/"+adultHandles[0].Handle, "admin-session", nil)
	r.Body.Close()
	assert.Equal(t, http.StatusNotFound, r.StatusCode)
	after, err := env.Sessions.ListForUser(ctx, env.UserID)
	require.NoError(t, err)
	assert.Len(t, after, len(adultHandles), "no adult session was revoked")
}

func keysOfAny(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func signInHere(t *testing.T, env *testsupport.Env, kidID, token, userAgent string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, env.URL+"/api/admin/kids/"+kidID+"/sign-in-here", nil)
	if token != "" {
		req.AddCookie(&http.Cookie{Name: "homepad_session", Value: token})
	}
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	return resp
}

// AC-009 / §2.2 — "Sign in this device as …": needs an admin session, ends it,
// and sets a 180-day kid session cookie, all in one response.
func TestAdminKids_SignInHere(t *testing.T) {
	env := testsupport.NewEnv(t)
	kidID, _ := env.SeedKid(t, "Maya")
	ctx := context.Background()

	// Needs a session, and an admin one.
	assert.Equal(t, http.StatusUnauthorized, signInHere(t, env, kidID, "", "").StatusCode)
	resp := signInHere(t, env, kidID, "non-admin-session", "")
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Nil(t, sessionCookieFrom(resp))
	code, _ := getMeRaw(t, env.URL, "non-admin-session")
	assert.Equal(t, http.StatusOK, code, "a refused swap leaves the caller signed in")

	// A kid cannot swap into another kid (the swap needs the admin's session).
	_, leoTok := env.SeedKid(t, "Leo")
	resp = signInHere(t, env, kidID, leoTok, "")
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Nil(t, sessionCookieFrom(resp))

	// Not a kid → 404, and the admin is still signed in.
	for _, id := range []string{env.UserID, "00000000-0000-0000-0000-000000000000", "nope"} {
		resp := signInHere(t, env, id, "admin-session", "")
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, id)
		assert.Nil(t, sessionCookieFrom(resp), "no cookie on a failed swap")
	}
	code, _ = getMeRaw(t, env.URL, "admin-session")
	require.Equal(t, http.StatusOK, code)

	before, err := env.Sessions.ListForUser(ctx, kidID)
	require.NoError(t, err)

	resp = signInHere(t, env, kidID, "admin-session", "Mozilla/5.0 (iPad; CPU OS 18_0 like Mac OS X) Safari")
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	ck := sessionCookieFrom(resp)
	require.NotNil(t, ck, "the response sets the kid session cookie")
	assert.NotEqual(t, "admin-session", ck.Value)
	assert.NotEmpty(t, ck.Value)
	assert.Equal(t, int(session.KidLifetime/time.Second), ck.MaxAge, "AC-010: 180-day Max-Age")
	assert.True(t, ck.HttpOnly)
	assert.Equal(t, "/", ck.Path)
	assert.Equal(t, http.SameSiteLaxMode, ck.SameSite)

	code, _ = getMeRaw(t, env.URL, "admin-session")
	assert.Equal(t, http.StatusUnauthorized, code, "the admin's session on this device has ended (server-side)")
	code, me := getMeRaw(t, env.URL, ck.Value)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "kid", me["role"])
	assert.Equal(t, kidID, me["id"])

	after, err := env.Sessions.ListForUser(ctx, kidID)
	require.NoError(t, err)
	require.Len(t, after, len(before)+1)
	assert.Equal(t, "Mozilla/5.0 (iPad; CPU OS 18_0 like Mac OS X) Safari", after[0].UserAgent, "the device is labelled")
	var exp time.Time
	h := sha256.Sum256([]byte(ck.Value))
	require.NoError(t, env.Store.Pool().QueryRow(ctx, `SELECT expires_at FROM sessions WHERE token_hash = $1`, h[:]).Scan(&exp))
	assert.WithinDuration(t, time.Now().Add(session.KidLifetime), exp, time.Minute)
}

// Every admin route answers 401 without a session (the 403s for kids and
// plain users are in routes_test.go).
func TestAdminAccounts_RequireSession(t *testing.T) {
	env := testsupport.NewEnv(t)
	kidID, _ := env.SeedKid(t, "Maya")
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/admin/accounts"},
		{http.MethodPost, "/api/admin/kids"},
		{http.MethodPatch, "/api/admin/kids/" + kidID},
		{http.MethodDelete, "/api/admin/kids/" + kidID},
		{http.MethodGet, "/api/admin/kids/" + kidID + "/services"},
		{http.MethodPut, "/api/admin/kids/" + kidID + "/services"},
		{http.MethodGet, "/api/admin/kids/" + kidID + "/sessions"},
		{http.MethodDelete, "/api/admin/kids/" + kidID + "/sessions/x"},
		{http.MethodPost, "/api/admin/kids/" + kidID + "/sign-in-here"},
	} {
		r := doNoCookie(t, c.method, env.URL+c.path, nil)
		r.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, r.StatusCode, "%s %s", c.method, c.path)
	}
	_, err := env.Store.KidByID(context.Background(), kidID)
	assert.NoError(t, err, "nothing was deleted")
}
