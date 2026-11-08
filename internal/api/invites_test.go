package api_test

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/api"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// SPEC-household-basics scope 2 — adding an adult through a one-time invite
// link (AC-008…AC-018). The admin routes are admin-only; lookup and accept
// are public and rate-limited.

type inviteCreated struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	Token     string    `json:"token"`
}

type inviteListItem struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	CreatedBy struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"createdBy"`
}

// roomyLimit keeps the rate limit out of the way of tests that make many
// public calls on purpose.
var roomyLimit = api.RateLimit{PerMinute: 6000, Burst: 1000}

func inviteEnv(t *testing.T) *testsupport.Env {
	t.Helper()
	return testsupport.NewEnvWith(t, testsupport.Options{InviteLimit: roomyLimit})
}

func createInvite(t *testing.T, env *testsupport.Env, session string, body any) (int, inviteCreated, []byte) {
	t.Helper()
	r := doJSON(t, http.MethodPost, env.URL+"/api/admin/invites", session, body)
	b := readAll(t, r)
	var v inviteCreated
	if r.StatusCode == http.StatusCreated {
		require.NoError(t, json.Unmarshal(b, &v), string(b))
	}
	return r.StatusCode, v, b
}

func mustInvite(t *testing.T, env *testsupport.Env, name, role string) inviteCreated {
	t.Helper()
	code, v, b := createInvite(t, env, "admin-session", map[string]any{"name": name, "role": role})
	require.Equal(t, http.StatusCreated, code, string(b))
	return v
}

func listInvites(t *testing.T, env *testsupport.Env, session string) (int, []inviteListItem, []byte) {
	t.Helper()
	r := doJSON(t, http.MethodGet, env.URL+"/api/admin/invites", session, nil)
	b := readAll(t, r)
	var out []inviteListItem
	if r.StatusCode == http.StatusOK {
		require.NoError(t, json.Unmarshal(b, &out), "the invite list is a JSON array: %s", b)
	}
	return r.StatusCode, out, b
}

func lookupInvite(t *testing.T, env *testsupport.Env, token string) (int, []byte) {
	t.Helper()
	r := postJSON(t, env.URL+"/api/invites/lookup", map[string]any{"token": token})
	return r.StatusCode, readAll(t, r)
}

func acceptInvite(t *testing.T, env *testsupport.Env, body map[string]any) (*http.Response, []byte) {
	t.Helper()
	r := postJSON(t, env.URL+"/api/invites/accept", body)
	return r, readAll(t, r)
}

func acceptBody(token, email string) map[string]any {
	return map[string]any{"token": token, "email": email, "password": "a long enough password"}
}

func revokeInvite(t *testing.T, env *testsupport.Env, session, id string) int {
	t.Helper()
	r := doJSON(t, http.MethodDelete, env.URL+"/api/admin/invites/"+id, session, nil)
	readAll(t, r)
	return r.StatusCode
}

// expireInvite moves an invite's expiry into the past, as if 8 days passed.
func expireInvite(t *testing.T, env *testsupport.Env, id string) {
	t.Helper()
	_, err := env.Store.Pool().Exec(t.Context(),
		`UPDATE invites SET created_at = now() - interval '8 days', expires_at = now() - interval '1 day' WHERE id = $1`, id)
	require.NoError(t, err)
}

func randomToken(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(b)
}

func countUsers(t *testing.T, env *testsupport.Env) int {
	t.Helper()
	var n int
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM users`).Scan(&n))
	return n
}

// AC-008 — name 1–40 characters with no control characters, role admin or
// user. The response carries the token once.
func TestInvites_CreateValidatesNameAndRole(t *testing.T) {
	env := inviteEnv(t)

	for _, role := range []string{"user", "admin"} {
		code, v, b := createInvite(t, env, "admin-session", map[string]any{"name": "  Alex  ", "role": role})
		require.Equal(t, http.StatusCreated, code, string(b))
		assert.NotEmpty(t, v.ID)
		assert.Equal(t, "Alex", v.Name, "the name is trimmed")
		assert.Equal(t, role, v.Role)
		assert.NotEmpty(t, v.Token, "the token is returned once, on create")
		assert.False(t, v.CreatedAt.IsZero())
		assert.False(t, v.ExpiresAt.IsZero())
	}

	for name, body := range map[string]any{
		"empty name":         map[string]any{"name": "", "role": "user"},
		"blank name":         map[string]any{"name": "   ", "role": "user"},
		"41 characters":      map[string]any{"name": strings.Repeat("a", 41), "role": "user"},
		"control character":  map[string]any{"name": "Al\u0007ex", "role": "user"},
		"missing name":       map[string]any{"role": "user"},
		"kid role":           map[string]any{"name": "Sam", "role": "kid"},
		"unknown role":       map[string]any{"name": "Sam", "role": "superadmin"},
		"missing role":       map[string]any{"name": "Sam"},
		"unknown field":      map[string]any{"name": "Sam", "role": "user", "email": "sam@example.com"},
		"not a JSON object":  []string{"Sam"},
		"role is not string": map[string]any{"name": "Sam", "role": 1},
	} {
		code, _, b := createInvite(t, env, "admin-session", body)
		assert.Equal(t, http.StatusBadRequest, code, "%s: %s", name, b)
	}
	// 40 characters, counted as characters rather than bytes, is fine.
	code, _, b := createInvite(t, env, "admin-session", map[string]any{"name": strings.Repeat("é", 40), "role": "user"})
	assert.Equal(t, http.StatusCreated, code, string(b))
}

// AC-009 — the token is 32 CSPRNG bytes, base64url. Only its SHA-256 is
// stored, and no later response carries it or its hash.
func TestInvites_TokenIs32BytesAndOnlyHashStored(t *testing.T) {
	env := inviteEnv(t)
	a := mustInvite(t, env, "Alex", "user")
	b := mustInvite(t, env, "Blair", "user")
	assert.NotEqual(t, a.Token, b.Token)

	raw, err := base64.RawURLEncoding.DecodeString(a.Token)
	require.NoError(t, err, "the token is unpadded base64url")
	assert.Len(t, raw, 32)

	var stored []byte
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT token_hash FROM invites WHERE id = $1`, a.ID).Scan(&stored))
	sum := sha256.Sum256([]byte(a.Token))
	assert.Equal(t, sum[:], stored, "token_hash is sha256(token)")

	var row string
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT row_to_json(i)::text FROM invites i WHERE id = $1`, a.ID).Scan(&row))
	assert.NotContains(t, row, a.Token, "no column holds the raw token")

	_, _, listBody := listInvites(t, env, "admin-session")
	assertNoTokenMaterial(t, listBody, a.Token)
	code, lookupBody := lookupInvite(t, env, a.Token)
	require.Equal(t, http.StatusOK, code)
	assertNoTokenMaterial(t, lookupBody, a.Token)
	_, acceptRespBody := acceptInvite(t, env, acceptBody(a.Token, "alex@example.com"))
	assertNoTokenMaterial(t, acceptRespBody, a.Token)
}

// AC-010 — an invite lasts 7 days from creation.
func TestInvites_ExpireAfterSevenDays(t *testing.T) {
	env := inviteEnv(t)
	inv := mustInvite(t, env, "Alex", "user")
	assert.InDelta(t, (7 * 24 * time.Hour).Seconds(), inv.ExpiresAt.Sub(inv.CreatedAt).Seconds(), 2,
		"expiresAt is createdAt + 7 days")

	expireInvite(t, env, inv.ID)
	code, _ := lookupInvite(t, env, inv.Token)
	assert.Equal(t, http.StatusNotFound, code, "an expired invite is dead at lookup")
	r, b := acceptInvite(t, env, acceptBody(inv.Token, "alex@example.com"))
	assert.Equal(t, http.StatusNotFound, r.StatusCode, string(b))
	_, list, _ := listInvites(t, env, "admin-session")
	assert.Empty(t, list, "an expired invite is not pending")
	assert.Equal(t, http.StatusNotFound, revokeInvite(t, env, "admin-session", inv.ID))
}

// AC-010 — single use: the second accept is 404 and creates nothing.
func TestInvites_SingleUse(t *testing.T) {
	env := inviteEnv(t)
	inv := mustInvite(t, env, "Alex", "user")
	before := countUsers(t, env)

	r, b := acceptInvite(t, env, acceptBody(inv.Token, "alex@example.com"))
	require.Equal(t, http.StatusCreated, r.StatusCode, string(b))
	r2, b2 := acceptInvite(t, env, acceptBody(inv.Token, "alex2@example.com"))
	assert.Equal(t, http.StatusNotFound, r2.StatusCode, string(b2))
	assert.Equal(t, before+1, countUsers(t, env), "only the first accept creates an account")

	var usedBy *string
	var usedAt *time.Time
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT used_by::text, used_at FROM invites WHERE id = $1`, inv.ID).Scan(&usedBy, &usedAt))
	require.NotNil(t, usedBy, "used_by records who joined")
	assert.NotNil(t, usedAt)
	var newID string
	require.NoError(t, json.Unmarshal(mustField(t, b, "id"), &newID))
	assert.Equal(t, newID, *usedBy)
}

func mustField(t *testing.T, body []byte, key string) json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &m), string(body))
	v, ok := m[key]
	require.True(t, ok, "%s missing from %s", key, body)
	return v
}

// AC-010 — the use is marked in the same transaction that creates the
// account, so racing accepts of one token create exactly one account.
func TestInvites_ConcurrentAcceptCreatesOne(t *testing.T) {
	env := inviteEnv(t)
	inv := mustInvite(t, env, "Alex", "user")
	before := countUsers(t, env)

	const n = 6
	codes := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			body, _ := json.Marshal(acceptBody(inv.Token, fmt.Sprintf("racer%d@example.com", i)))
			resp, err := http.Post(env.URL+"/api/invites/accept", "application/json", strings.NewReader(string(body)))
			if err != nil {
				return
			}
			resp.Body.Close()
			codes[i] = resp.StatusCode
		}(i)
	}
	close(start)
	wg.Wait()

	created, notFound := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusNotFound:
			notFound++
		}
	}
	assert.Equal(t, 1, created, "exactly one accept wins (codes %v)", codes)
	assert.Equal(t, n-1, notFound, "the rest see a dead link")
	assert.Equal(t, before+1, countUsers(t, env))
}

// AC-011 — the list shows pending invites only, newest first, with who made
// each one. Revoke removes one; a revoked token is dead.
func TestInvites_ListPendingOnly(t *testing.T) {
	env := inviteEnv(t)
	used := mustInvite(t, env, "Used", "user")
	revoked := mustInvite(t, env, "Revoked", "user")
	expired := mustInvite(t, env, "Expired", "admin")
	older := mustInvite(t, env, "Older", "user")
	newer := mustInvite(t, env, "Newer", "admin")

	r, b := acceptInvite(t, env, acceptBody(used.Token, "used@example.com"))
	require.Equal(t, http.StatusCreated, r.StatusCode, string(b))
	require.Equal(t, http.StatusNoContent, revokeInvite(t, env, "admin-session", revoked.ID))
	expireInvite(t, env, expired.ID)

	code, list, body := listInvites(t, env, "admin-session")
	require.Equal(t, http.StatusOK, code, string(body))
	require.Len(t, list, 2, "only the two pending invites: %s", body)
	assert.Equal(t, newer.ID, list[0].ID, "newest first")
	assert.Equal(t, older.ID, list[1].ID)
	assert.Equal(t, "Newer", list[0].Name)
	assert.Equal(t, "admin", list[0].Role)
	assert.Equal(t, env.AdminID, list[0].CreatedBy.ID)
	assert.Equal(t, "admin@homepad.test", list[0].CreatedBy.Name, "an admin with no display name reads as their email")
	assert.WithinDuration(t, newer.ExpiresAt, list[0].ExpiresAt, time.Second)
	assert.NotContains(t, string(body), `"token"`, "the list never carries a token field")
}

// AC-011 — revoke: 204 once, then 404; unknown and malformed ids are 404.
func TestInvites_Revoke(t *testing.T) {
	env := inviteEnv(t)
	inv := mustInvite(t, env, "Alex", "user")
	assert.Equal(t, http.StatusNoContent, revokeInvite(t, env, "admin-session", inv.ID))
	assert.Equal(t, http.StatusNotFound, revokeInvite(t, env, "admin-session", inv.ID), "already revoked")
	assert.Equal(t, http.StatusNotFound, revokeInvite(t, env, "admin-session", "00000000-0000-0000-0000-000000000000"))
	assert.Equal(t, http.StatusNotFound, revokeInvite(t, env, "admin-session", "not-a-uuid"))

	code, _ := lookupInvite(t, env, inv.Token)
	assert.Equal(t, http.StatusNotFound, code)
	r, b := acceptInvite(t, env, acceptBody(inv.Token, "alex@example.com"))
	assert.Equal(t, http.StatusNotFound, r.StatusCode, string(b))

	used := mustInvite(t, env, "Used", "user")
	r, b = acceptInvite(t, env, acceptBody(used.Token, "used@example.com"))
	require.Equal(t, http.StatusCreated, r.StatusCode, string(b))
	assert.Equal(t, http.StatusNotFound, revokeInvite(t, env, "admin-session", used.ID), "a used invite can't be revoked")
}

// AC-012 — lookup returns the invite for a live token. Every dead token gets
// the same 404 status and body, at lookup and at accept.
func TestInvites_LookupLiveAndEveryDeadTokenIdentical404(t *testing.T) {
	env := inviteEnv(t)
	live := mustInvite(t, env, "Alex", "admin")

	code, body := lookupInvite(t, env, live.Token)
	require.Equal(t, http.StatusOK, code, string(body))
	var got struct {
		Name        string    `json:"name"`
		Role        string    `json:"role"`
		InviterName string    `json:"inviterName"`
		ExpiresAt   time.Time `json:"expiresAt"`
	}
	require.NoError(t, json.Unmarshal(body, &got))
	assert.Equal(t, "Alex", got.Name)
	assert.Equal(t, "admin", got.Role)
	assert.Equal(t, "admin@homepad.test", got.InviterName)
	assert.WithinDuration(t, live.ExpiresAt, got.ExpiresAt, time.Second)
	var keys map[string]any
	require.NoError(t, json.Unmarshal(body, &keys))
	assert.ElementsMatch(t, []string{"name", "role", "inviterName", "expiresAt"}, keysOfAny(keys))

	used := mustInvite(t, env, "Used", "user")
	r, b := acceptInvite(t, env, acceptBody(used.Token, "used@example.com"))
	require.Equal(t, http.StatusCreated, r.StatusCode, string(b))
	revoked := mustInvite(t, env, "Revoked", "user")
	require.Equal(t, http.StatusNoContent, revokeInvite(t, env, "admin-session", revoked.ID))
	expired := mustInvite(t, env, "Expired", "user")
	expireInvite(t, env, expired.ID)

	dead := map[string]string{
		"unknown":      randomToken(t),
		"malformed":    "not a token at all",
		"empty":        "",
		"short":        "abc",
		"bad encoding": strings.Repeat("!", 43),
		"used":         used.Token,
		"revoked":      revoked.Token,
		"expired":      expired.Token,
	}
	var wantLookup, wantAccept string
	for name, tok := range dead {
		code, body := lookupInvite(t, env, tok)
		assert.Equal(t, http.StatusNotFound, code, "lookup %s", name)
		if wantLookup == "" {
			wantLookup = string(body)
		}
		assert.Equal(t, wantLookup, string(body), "lookup %s: the same body for every dead token", name)

		r, b := acceptInvite(t, env, acceptBody(tok, "dead-"+strings.ReplaceAll(name, " ", "-")+"@example.com"))
		assert.Equal(t, http.StatusNotFound, r.StatusCode, "accept %s: %s", name, b)
		if wantAccept == "" {
			wantAccept = string(b)
		}
		assert.Equal(t, wantAccept, string(b), "accept %s: the same body for every dead token", name)
	}
	assert.Equal(t, wantLookup, wantAccept, "lookup and accept share the one dead-link answer")
}

// AC-013 — accept creates an adult with the invite's role and signs them in
// with an adult session: 201 and the /api/me body.
func TestInvites_AcceptCreatesAdultAndSignsIn(t *testing.T) {
	env := inviteEnv(t)
	for _, tc := range []struct {
		role, name, override, wantName string
	}{
		{"admin", "Jordan", "", "Jordan"},
		{"user", "Alex", "  Alexandra ", "Alexandra"},
	} {
		t.Run(tc.role, func(t *testing.T) {
			inv := mustInvite(t, env, tc.name, tc.role)
			email := strings.ToLower(tc.name) + "@example.com"
			body := acceptBody(inv.Token, email)
			if tc.override != "" {
				body["name"] = tc.override
			}
			r, b := acceptInvite(t, env, body)
			require.Equal(t, http.StatusCreated, r.StatusCode, string(b))

			var me struct {
				ID          string `json:"id"`
				Email       string `json:"email"`
				Role        string `json:"role"`
				Name        string `json:"name"`
				DisplayName string `json:"displayName"`
				ThemePref   string `json:"themePref"`
			}
			require.NoError(t, json.Unmarshal(b, &me))
			assert.Equal(t, tc.role, me.Role, "the role comes from the invite")
			assert.Equal(t, email, me.Email)
			assert.Equal(t, tc.wantName, me.Name)
			assert.Equal(t, tc.wantName, me.DisplayName)
			assert.NotEmpty(t, me.ThemePref, "the full /api/me body")

			var cookie *http.Cookie
			for _, c := range r.Cookies() {
				if c.Name == "homepad_session" {
					cookie = c
				}
			}
			require.NotNil(t, cookie, "accept signs the new adult in")
			assert.Equal(t, int((30 * 24 * time.Hour).Seconds()), cookie.MaxAge, "an adult (30-day) session")
			assert.True(t, cookie.HttpOnly)

			gotEmail, gotRole := fetchMe(t, env.Server, cookie)
			assert.Equal(t, email, gotEmail)
			assert.Equal(t, tc.role, gotRole)

			login := postJSON(t, env.URL+"/api/login", map[string]string{"email": email, "password": "a long enough password"})
			readAll(t, login)
			assert.Equal(t, http.StatusOK, login.StatusCode, "the chosen password signs in")
		})
	}
}

// AC-013 — unknown fields, including role, are a 400, and leave the invite
// usable.
func TestInvites_AcceptRefusesUnknownFields(t *testing.T) {
	env := inviteEnv(t)
	inv := mustInvite(t, env, "Alex", "user")
	for name, extra := range map[string]any{"role": "admin", "isAdmin": true} {
		body := acceptBody(inv.Token, "alex@example.com")
		body[name] = extra
		r, b := acceptInvite(t, env, body)
		assert.Equal(t, http.StatusBadRequest, r.StatusCode, "%s: %s", name, b)
	}
	r, b := acceptInvite(t, env, acceptBody(inv.Token, "alex@example.com"))
	require.Equal(t, http.StatusCreated, r.StatusCode, string(b))
	assert.Contains(t, string(b), `"role":"user"`, "the invite's role, not the requested one")
}

// AC-014 — a failure on the invitee's input rolls back and leaves the invite
// unused and valid.
func TestInvites_FailedAcceptLeavesInviteUsable(t *testing.T) {
	env := inviteEnv(t)
	inv := mustInvite(t, env, "Alex", "user")
	before := countUsers(t, env)

	for name, tc := range map[string]struct {
		body map[string]any
		want int
	}{
		"invalid email":  {map[string]any{"token": inv.Token, "email": "not-an-email", "password": "a long enough password"}, http.StatusBadRequest},
		"missing email":  {map[string]any{"token": inv.Token, "password": "a long enough password"}, http.StatusBadRequest},
		"short password": {map[string]any{"token": inv.Token, "email": "alex@example.com", "password": "short"}, http.StatusBadRequest},
		"bad name":       {map[string]any{"token": inv.Token, "email": "alex@example.com", "password": "a long enough password", "name": strings.Repeat("x", 41)}, http.StatusBadRequest},
		"email taken":    {acceptBody(inv.Token, "alice@example.com"), http.StatusConflict},
	} {
		r, b := acceptInvite(t, env, tc.body)
		assert.Equal(t, tc.want, r.StatusCode, "%s: %s", name, b)
		code, _ := lookupInvite(t, env, inv.Token)
		assert.Equal(t, http.StatusOK, code, "%s: the invite is still live", name)
	}
	assert.Equal(t, before, countUsers(t, env), "no failed accept created an account")

	r, b := acceptInvite(t, env, acceptBody(inv.Token, "alex@example.com"))
	assert.Equal(t, http.StatusCreated, r.StatusCode, string(b))
}

// AC-015 — invites work in both registration modes.
func TestInvites_WorkInBothModes(t *testing.T) {
	for _, mode := range []api.RegistrationMode{api.RegistrationClosed, api.RegistrationOpen} {
		t.Run(fmt.Sprintf("mode %d", mode), func(t *testing.T) {
			env := testsupport.NewEnvWith(t, testsupport.Options{Registration: modePtr(mode), InviteLimit: roomyLimit})
			inv := mustInvite(t, env, "Alex", "user")
			r, b := acceptInvite(t, env, acceptBody(inv.Token, "alex@example.com"))
			assert.Equal(t, http.StatusCreated, r.StatusCode, string(b))
		})
	}
}

// AC-016 — create, list and revoke are admin-only: no session 401, member
// 403, kid 403.
func TestInvites_AdminOnly(t *testing.T) {
	env := inviteEnv(t)
	inv := mustInvite(t, env, "Alex", "user")
	_, kidToken := env.SeedKid(t, "Sam")

	type call struct{ method, path string }
	for _, c := range []call{
		{http.MethodPost, "/api/admin/invites"},
		{http.MethodGet, "/api/admin/invites"},
		{http.MethodDelete, "/api/admin/invites/" + inv.ID},
	} {
		body := map[string]any{"name": "Mallory", "role": "admin"}
		anon := doNoCookie(t, c.method, env.URL+c.path, body)
		readAll(t, anon)
		assert.Equal(t, http.StatusUnauthorized, anon.StatusCode, "%s %s without a session", c.method, c.path)
		for who, tok := range map[string]string{"member": "non-admin-session", "kid": kidToken} {
			r := doJSON(t, c.method, env.URL+c.path, tok, body)
			readAll(t, r)
			assert.Equal(t, http.StatusForbidden, r.StatusCode, "%s %s as a %s", c.method, c.path, who)
		}
	}
	_, list, _ := listInvites(t, env, "admin-session")
	require.Len(t, list, 1, "nothing was created or revoked by the refused calls")
	assert.Equal(t, inv.ID, list[0].ID)
}

// AC-017 — lookup and accept share one bucket: 30 a minute, burst 10. Over
// it: 429 with Retry-After.
func TestInvites_PublicRoutesRateLimited(t *testing.T) {
	env := testsupport.NewEnv(t) // the default limit
	junk := randomToken(t)
	for i := 0; i < 6; i++ {
		code, _ := lookupInvite(t, env, junk)
		require.Equal(t, http.StatusNotFound, code, "request %d is inside the burst", i+1)
	}
	for i := 0; i < 4; i++ {
		r, b := acceptInvite(t, env, acceptBody(junk, "x@example.com"))
		require.Equal(t, http.StatusNotFound, r.StatusCode, "accept %d shares the burst: %s", i+1, b)
	}
	// The burst of 10 is spent across both routes.
	r, _ := acceptInvite(t, env, acceptBody(junk, "x@example.com"))
	require.Equal(t, http.StatusTooManyRequests, r.StatusCode)
	retry, err := strconv.Atoi(r.Header.Get("Retry-After"))
	require.NoError(t, err, "Retry-After is whole seconds")
	assert.GreaterOrEqual(t, retry, 1)
	assert.LessOrEqual(t, retry, 2, "one token refills every 2 seconds at 30 a minute")
	code, _ := lookupInvite(t, env, junk)
	assert.Equal(t, http.StatusTooManyRequests, code, "lookup is limited by the same bucket")

	// Other routes are not limited.
	login := postJSON(t, env.URL+"/api/login", map[string]string{"email": "alice@example.com", "password": "correct horse battery staple"})
	readAll(t, login)
	assert.Equal(t, http.StatusOK, login.StatusCode)
}

// AC-018 — create, revoke and accept each write one log line, and no log line
// carries a token or its hash.
func TestInvites_LogLinesNeverCarryTokenOrHash(t *testing.T) {
	logs := captureLog(t)
	env := inviteEnv(t)
	kept := mustInvite(t, env, "Alex", "user")
	dropped := mustInvite(t, env, "Blair", "admin")
	require.Equal(t, http.StatusNoContent, revokeInvite(t, env, "admin-session", dropped.ID))
	r, b := acceptInvite(t, env, acceptBody(kept.Token, "alex@example.com"))
	require.Equal(t, http.StatusCreated, r.StatusCode, string(b))
	var newID string
	require.NoError(t, json.Unmarshal(mustField(t, b, "id"), &newID))

	out := logs.String()
	assert.Equal(t, 1, strings.Count(out, fmt.Sprintf("invite: admin %s created invite %s role=user expires=", env.AdminID, kept.ID)), out)
	assert.Equal(t, 1, strings.Count(out, fmt.Sprintf("invite: admin %s created invite %s role=admin expires=", env.AdminID, dropped.ID)), out)
	assert.Equal(t, 1, strings.Count(out, fmt.Sprintf("invite: admin %s revoked invite %s", env.AdminID, dropped.ID)), out)
	assert.Equal(t, 1, strings.Count(out, fmt.Sprintf("invite: invite %s accepted, created user %s role=user", kept.ID, newID)), out)

	for _, tok := range []string{kept.Token, dropped.Token} {
		sum := sha256.Sum256([]byte(tok))
		for _, form := range []string{
			tok,
			hex.EncodeToString(sum[:]),
			base64.StdEncoding.EncodeToString(sum[:]),
			base64.RawURLEncoding.EncodeToString(sum[:]),
		} {
			assert.NotContains(t, out, form, "no log line carries a token or its hash")
		}
	}
	assert.NotContains(t, out, "a long enough password")
}
