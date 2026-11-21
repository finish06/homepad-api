package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/api"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/session"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// SPEC-password-reset-link — an admin-made, one-time reset link
// (AC-001…AC-012), changing your own password (AC-013, AC-014), the audit
// lines (AC-015) and hasPassword on /api/me and the Accounts list (AC-020,
// AC-021). The link copies the invite rules: 32 CSPRNG bytes, only the
// SHA-256 stored, one 404 for every dead link, the token only in POST bodies.

const (
	// userPassword (adults_test.go) is the fixture password of user@homepad.test.
	adminPassword = "stitch-admin-pw" // and this is admin@homepad.test's
	newPassword   = "a brand new password"
	oidcOnlyHash  = "oidc:no-local-password"
)

type resetCreated struct {
	ID        string    `json:"id"`
	ExpiresAt time.Time `json:"expiresAt"`
	Token     string    `json:"token"`
}

func resetEnv(t *testing.T) *testsupport.Env {
	t.Helper()
	return testsupport.NewEnvWith(t, testsupport.Options{InviteLimit: roomyLimit})
}

func createResetLink(t *testing.T, env *testsupport.Env, session, id string) (int, resetCreated, []byte) {
	t.Helper()
	r := doJSON(t, http.MethodPost, env.URL+"/api/admin/accounts/"+id+"/reset-link", session, nil)
	b := readAll(t, r)
	var v resetCreated
	if r.StatusCode == http.StatusCreated {
		require.NoError(t, json.Unmarshal(b, &v), string(b))
	}
	return r.StatusCode, v, b
}

func mustResetLink(t *testing.T, env *testsupport.Env, id string) resetCreated {
	t.Helper()
	code, v, b := createResetLink(t, env, "admin-session", id)
	require.Equal(t, http.StatusCreated, code, string(b))
	return v
}

func lookupReset(t *testing.T, env *testsupport.Env, token string) (int, []byte) {
	t.Helper()
	r := postJSON(t, env.URL+"/api/password-resets/lookup", map[string]any{"token": token})
	return r.StatusCode, readAll(t, r)
}

func acceptReset(t *testing.T, env *testsupport.Env, body map[string]any, cookies ...*http.Cookie) (*http.Response, []byte) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, env.URL+"/api/password-resets/accept", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	r, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return r, readAll(t, r)
}

func changePassword(t *testing.T, env *testsupport.Env, session string, body any) (int, http.Header, []byte) {
	t.Helper()
	r := doJSON(t, http.MethodPost, env.URL+"/api/me/password", session, body)
	b := readAll(t, r)
	return r.StatusCode, r.Header, b
}

func loginStatus(t *testing.T, env *testsupport.Env, email, password string) int {
	t.Helper()
	r := postJSON(t, env.URL+"/api/login", map[string]string{"email": email, "password": password})
	readAll(t, r)
	return r.StatusCode
}

// seedSSOUser makes an account that only signs in with single sign-on (the
// OIDC placeholder hash) and gives it a session.
func seedSSOUser(t *testing.T, env *testsupport.Env, email string) (id, token string) {
	t.Helper()
	ctx := context.Background()
	u, err := env.Store.CreateUser(ctx, email, oidcOnlyHash, "user")
	require.NoError(t, err)
	token = "sso-session-" + u.ID
	require.NoError(t, env.Sessions.Bind(ctx, token, u.ID, session.AdultLifetime))
	return u.ID, token
}

// seedOtherAdmin adds a second admin with a password and returns its id.
func seedOtherAdmin(t *testing.T, env *testsupport.Env) string {
	t.Helper()
	u, err := env.Store.CreateUser(context.Background(), "other-admin@homepad.test", "$2a$04$not-a-real-hash-but-looks-like-one", "admin")
	require.NoError(t, err)
	return u.ID
}

// AC-001, AC-011 — the token is 32 CSPRNG bytes as unpadded base64url,
// returned once; only its SHA-256 is stored.
func TestResetLink_CreateReturnsTheTokenOnce(t *testing.T) {
	env := resetEnv(t)
	code, link, b := createResetLink(t, env, "admin-session", env.UserID)
	require.Equal(t, http.StatusCreated, code, string(b))

	var keys map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(b, &keys))
	assert.ElementsMatch(t, []string{"id", "expiresAt", "token"}, mapKeys(keys), "the create body is {id, expiresAt, token}")
	assert.NotEmpty(t, link.ID)
	raw, err := base64.RawURLEncoding.DecodeString(link.Token)
	require.NoError(t, err, "the token is unpadded base64url")
	assert.Len(t, raw, 32, "32 random bytes")

	var stored []byte
	var n int
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT token_hash, (SELECT count(*) FROM password_resets) FROM password_resets WHERE id = $1`, link.ID).Scan(&stored, &n))
	sum := sha256.Sum256([]byte(link.Token))
	assert.Equal(t, sum[:], stored, "only sha256(token) is stored")
	assert.Equal(t, 1, n)

	var row string
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT row_to_json(p)::text FROM password_resets p WHERE id = $1`, link.ID).Scan(&row))
	assert.NotContains(t, row, link.Token, "the token is in no column")

	// Never listed anywhere an admin reads.
	for _, path := range []string{"/api/admin/accounts", "/api/admin/invites"} {
		r := doJSON(t, http.MethodGet, env.URL+path, "admin-session", nil)
		body := readAll(t, r)
		require.Equal(t, http.StatusOK, r.StatusCode)
		assert.NotContains(t, string(body), link.Token, "%s carries no reset token", path)
	}

	// Making a link changes nothing for the account until it's used.
	assert.Equal(t, http.StatusOK, loginStatus(t, env, "user@homepad.test", userPassword))
	assert.Equal(t, http.StatusOK, meStatus(t, env, "non-admin-session"))
}

func mapKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// AC-002 — create is admin-only.
func TestResetLink_CreateIsAdminOnly(t *testing.T) {
	env := resetEnv(t)
	_, kidToken := env.SeedKid(t, "Maya")

	r := doNoCookie(t, http.MethodPost, env.URL+"/api/admin/accounts/"+env.UserID+"/reset-link", nil)
	readAll(t, r)
	assert.Equal(t, http.StatusUnauthorized, r.StatusCode, "no session")

	code, _, _ := createResetLink(t, env, "non-admin-session", env.AdminID)
	assert.Equal(t, http.StatusForbidden, code, "a member")
	code, _, _ = createResetLink(t, env, kidToken, env.UserID)
	assert.Equal(t, http.StatusForbidden, code, "a kid")

	var n int
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM password_resets`).Scan(&n))
	assert.Zero(t, n, "no refused call made a link")
}

// AC-003 — only another adult: a kid, an unknown id and a malformed id are
// 404; your own id is 409 self. Any admin may make one for another admin.
func TestResetLink_CreateTargets(t *testing.T) {
	env := resetEnv(t)
	kidID, _ := env.SeedKid(t, "Maya")

	for name, id := range map[string]string{
		"kid":       kidID,
		"unknown":   "00000000-0000-0000-0000-000000000000",
		"malformed": "not-a-uuid",
	} {
		code, _, b := createResetLink(t, env, "admin-session", id)
		assert.Equal(t, http.StatusNotFound, code, "%s: %s", name, b)
	}

	code, _, b := createResetLink(t, env, "admin-session", env.AdminID)
	require.Equal(t, http.StatusConflict, code, string(b))
	assert.Equal(t, "self", errorCode(t, b))

	second := seedOtherAdmin(t, env)
	mustResetLink(t, env, second)
}

// AC-004 — a link works for 24 hours.
func TestResetLink_ExpiresIn24Hours(t *testing.T) {
	env := resetEnv(t)
	before := time.Now()
	link := mustResetLink(t, env, env.UserID)
	assert.WithinDuration(t, before.Add(24*time.Hour), link.ExpiresAt, time.Minute)

	var lifetime time.Duration
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT EXTRACT(EPOCH FROM expires_at - created_at)::bigint * 1000000000 FROM password_resets WHERE id = $1`, link.ID).Scan(&lifetime))
	assert.Equal(t, 24*time.Hour, lifetime)
}

// AC-005 — a new link revokes the account's earlier live link.
func TestResetLink_NewLinkRevokesTheOldOne(t *testing.T) {
	env := resetEnv(t)
	first := mustResetLink(t, env, env.UserID)
	other := mustResetLink(t, env, seedOtherAdmin(t, env))
	second := mustResetLink(t, env, env.UserID)

	code, _ := lookupReset(t, env, first.Token)
	assert.Equal(t, http.StatusNotFound, code, "the earlier link is dead")
	code, _ = lookupReset(t, env, second.Token)
	assert.Equal(t, http.StatusOK, code, "the new link works")
	code, _ = lookupReset(t, env, other.Token)
	assert.Equal(t, http.StatusOK, code, "another account's link is untouched")

	var revoked bool
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT revoked_at IS NOT NULL FROM password_resets WHERE id = $1`, first.ID).Scan(&revoked))
	assert.True(t, revoked)
}

// AC-006 — lookup answers {email, name, expiresAt} for a live link and
// doesn't use it.
func TestResetLink_LookupALiveLink(t *testing.T) {
	env := resetEnv(t)
	link := mustResetLink(t, env, env.UserID)

	for i := 0; i < 2; i++ {
		code, b := lookupReset(t, env, link.Token)
		require.Equal(t, http.StatusOK, code, string(b))
		var keys map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(b, &keys))
		assert.ElementsMatch(t, []string{"email", "name", "expiresAt"}, mapKeys(keys))
		var v struct {
			Email     string    `json:"email"`
			Name      string    `json:"name"`
			ExpiresAt time.Time `json:"expiresAt"`
		}
		require.NoError(t, json.Unmarshal(b, &v))
		assert.Equal(t, "user@homepad.test", v.Email)
		assert.Equal(t, "user@homepad.test", v.Name, "an account with no display name is named by its email")
		assert.True(t, v.ExpiresAt.Equal(link.ExpiresAt))
	}
}

// AC-006, AC-009, TC-002 — unknown, malformed, expired, used, revoked and
// "account removed" all get the same 404, from lookup and from accept.
func TestResetLink_EveryDeadLinkLooksTheSame(t *testing.T) {
	env := resetEnv(t)
	ctx := t.Context()

	used := mustResetLink(t, env, env.UserID)
	r, b := acceptReset(t, env, map[string]any{"token": used.Token, "password": newPassword})
	require.Equal(t, http.StatusOK, r.StatusCode, string(b))

	revoked := mustResetLink(t, env, env.UserID)
	mustResetLink(t, env, env.UserID) // revokes it

	second := seedOtherAdmin(t, env)
	expired := mustResetLink(t, env, second)
	_, err := env.Store.Pool().Exec(ctx,
		`UPDATE password_resets SET created_at = now() - interval '25 hours', expires_at = now() - interval '1 hour' WHERE id = $1`, expired.ID)
	require.NoError(t, err)

	goneID := seedAdult(t, env, "gone@example.com")
	gone := mustResetLink(t, env, goneID)
	_, err = env.Store.Pool().Exec(ctx, `DELETE FROM users WHERE id = $1`, goneID)
	require.NoError(t, err)

	ssoLaterID := seedAdult(t, env, "later-sso@example.com")
	ssoLater := mustResetLink(t, env, ssoLaterID)
	_, err = env.Store.Pool().Exec(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1`, ssoLaterID, oidcOnlyHash)
	require.NoError(t, err)

	dead := map[string]string{
		"unknown":          randomToken(t),
		"malformed short":  "abc",
		"malformed chars":  strings.Repeat("!", 43),
		"empty":            "",
		"used":             used.Token,
		"revoked":          revoked.Token,
		"expired":          expired.Token,
		"account removed":  gone.Token,
		"now SSO-only":     ssoLater.Token,
		"padded base64url": base64.URLEncoding.EncodeToString(make([]byte, 32)),
	}
	var want []byte
	for name, tok := range dead {
		code, body := lookupReset(t, env, tok)
		assert.Equal(t, http.StatusNotFound, code, "lookup %s", name)
		if want == nil {
			want = body
		}
		assert.Equal(t, string(want), string(body), "lookup %s: one body for every dead link", name)

		r, body := acceptReset(t, env, map[string]any{"token": tok, "password": "another new password"})
		assert.Equal(t, http.StatusNotFound, r.StatusCode, "accept %s", name)
		assert.Equal(t, string(want), string(body), "accept %s: the same body", name)
		assert.Nil(t, sessionCookieFrom(r), "accept %s signs nobody in", name)
	}
	assert.Equal(t, http.StatusOK, loginStatus(t, env, "user@homepad.test", newPassword), "dead links changed nothing")
}

// AC-007 — accept sets the password, answers the /api/me body and signs in.
func TestResetLink_AcceptSetsThePasswordAndSignsIn(t *testing.T) {
	env := resetEnv(t)
	link := mustResetLink(t, env, env.UserID)

	r, b := acceptReset(t, env, map[string]any{"token": link.Token, "password": newPassword})
	require.Equal(t, http.StatusOK, r.StatusCode, string(b))
	ck := sessionCookieFrom(r)
	require.NotNil(t, ck, "accept sets a session cookie")
	assert.True(t, ck.HttpOnly)

	me := doJSON(t, http.MethodGet, env.URL+"/api/me", ck.Value, nil)
	meBody := readAll(t, me)
	require.Equal(t, http.StatusOK, me.StatusCode)
	var got, want map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	require.NoError(t, json.Unmarshal(meBody, &want))
	assert.Equal(t, want, got, "accept answers the /api/me body")
	assert.Equal(t, env.UserID, got["id"])
	assert.Equal(t, true, got["hasPassword"])

	assert.Equal(t, http.StatusOK, loginStatus(t, env, "user@homepad.test", newPassword))
	assert.Equal(t, http.StatusUnauthorized, loginStatus(t, env, "user@homepad.test", userPassword), "the old password is gone")
}

// AC-007, TC-004 — accept takes {token, password} and nothing else.
func TestResetLink_AcceptRefusesUnknownFields(t *testing.T) {
	env := resetEnv(t)
	link := mustResetLink(t, env, env.UserID)
	for _, body := range []map[string]any{
		{"token": link.Token, "password": newPassword, "email": "x"},
		{"token": link.Token, "password": newPassword, "userId": env.AdminID},
	} {
		r, b := acceptReset(t, env, body)
		assert.Equal(t, http.StatusBadRequest, r.StatusCode, string(b))
	}
	r, b := acceptReset(t, env, map[string]any{"token": link.Token})
	assert.Equal(t, http.StatusBadRequest, r.StatusCode, "a missing password: %s", b)

	code, _ := lookupReset(t, env, link.Token)
	assert.Equal(t, http.StatusOK, code, "a refused accept leaves the link usable")
}

// AC-008 — every session of the account ends; the device that used the link
// is signed in, even when it carried one of the old sessions.
func TestResetLink_AcceptEndsEverySession(t *testing.T) {
	env := resetEnv(t)
	link := mustResetLink(t, env, env.UserID)

	old := &http.Cookie{Name: "homepad_session", Value: "session-one"}
	r, b := acceptReset(t, env, map[string]any{"token": link.Token, "password": newPassword}, old)
	require.Equal(t, http.StatusOK, r.StatusCode, string(b))
	ck := sessionCookieFrom(r)
	require.NotNil(t, ck)
	assert.NotEqual(t, "session-one", ck.Value)

	for _, tok := range []string{"session-one", "session-two", "non-admin-session", "any-user", "real-once-impl-exists"} {
		assert.Equal(t, http.StatusUnauthorized, meStatus(t, env, tok), "%s ended", tok)
	}
	assert.Equal(t, http.StatusOK, meStatus(t, env, ck.Value), "the new session works")
	assert.Equal(t, http.StatusOK, meStatus(t, env, "admin-session"), "another account is untouched")

	list, err := env.Sessions.ListForUser(t.Context(), env.UserID)
	require.NoError(t, err)
	assert.Len(t, list, 1, "only the new session is left")
}

// AC-009 — the link works once; a refused accept on the user's input leaves
// it usable.
func TestResetLink_WorksOnce(t *testing.T) {
	env := resetEnv(t)
	link := mustResetLink(t, env, env.UserID)

	for name, pw := range map[string]string{
		"under 8 characters": "short",
		"over 72 bytes":      strings.Repeat("é", 37),
	} {
		r, b := acceptReset(t, env, map[string]any{"token": link.Token, "password": pw})
		assert.Equal(t, http.StatusBadRequest, r.StatusCode, "%s: %s", name, b)
		if name == "over 72 bytes" {
			assert.Contains(t, string(b), "password must be at most 72 bytes")
		}
	}
	code, _ := lookupReset(t, env, link.Token)
	require.Equal(t, http.StatusOK, code, "still usable after the refused tries")

	r, b := acceptReset(t, env, map[string]any{"token": link.Token, "password": newPassword})
	require.Equal(t, http.StatusOK, r.StatusCode, string(b))
	r, b = acceptReset(t, env, map[string]any{"token": link.Token, "password": "a second new password"})
	assert.Equal(t, http.StatusNotFound, r.StatusCode, string(b))
	assert.Equal(t, http.StatusOK, loginStatus(t, env, "user@homepad.test", newPassword), "the second accept changed nothing")
	assert.Equal(t, http.StatusUnauthorized, loginStatus(t, env, "user@homepad.test", "a second new password"))
}

// AC-009 — two accepts racing for one link: exactly one wins.
func TestResetLink_ConcurrentAcceptsOneWins(t *testing.T) {
	env := resetEnv(t)
	link := mustResetLink(t, env, env.UserID)
	codes := make(chan int, 4)
	for i := 0; i < 4; i++ {
		go func(i int) {
			body, _ := json.Marshal(map[string]any{"token": link.Token, "password": fmt.Sprintf("racing password %d", i)})
			r, err := http.Post(env.URL+"/api/password-resets/accept", "application/json", strings.NewReader(string(body)))
			if err != nil {
				codes <- 0
				return
			}
			r.Body.Close()
			codes <- r.StatusCode
		}(i)
	}
	won := 0
	for i := 0; i < 4; i++ {
		switch c := <-codes; c {
		case http.StatusOK:
			won++
		case http.StatusNotFound:
		default:
			t.Errorf("unexpected status %d", c)
		}
	}
	assert.Equal(t, 1, won, "exactly one accept uses the link")
}

// AC-010, TC-005 — no reset link for an account that only uses single
// sign-on.
func TestResetLink_SSOAccountIsRefused(t *testing.T) {
	env := resetEnv(t)
	ssoID, _ := seedSSOUser(t, env, "pat@example.com")
	code, _, b := createResetLink(t, env, "admin-session", ssoID)
	require.Equal(t, http.StatusConflict, code, string(b))
	assert.Equal(t, "sso_account", errorCode(t, b))
}

// AC-012 — lookup and accept are rate-limited: 429 with Retry-After.
func TestResetLink_PublicRoutesAreRateLimited(t *testing.T) {
	env := testsupport.NewEnvWith(t, testsupport.Options{InviteLimit: api.RateLimit{PerMinute: 30, Burst: 3}})
	junk := randomToken(t)
	for i := 0; i < 3; i++ {
		code, _ := lookupReset(t, env, junk)
		require.Equal(t, http.StatusNotFound, code)
	}
	r := postJSON(t, env.URL+"/api/password-resets/lookup", map[string]any{"token": junk})
	readAll(t, r)
	require.Equal(t, http.StatusTooManyRequests, r.StatusCode)
	retry, err := strconv.Atoi(r.Header.Get("Retry-After"))
	require.NoError(t, err, "Retry-After is whole seconds")
	assert.GreaterOrEqual(t, retry, 1)

	r2, _ := acceptReset(t, env, map[string]any{"token": junk, "password": newPassword})
	assert.Equal(t, http.StatusTooManyRequests, r2.StatusCode, "accept is limited too")
	assert.NotEmpty(t, r2.Header.Get("Retry-After"))
}

// AC-013 — the right current password sets the new one; every other session
// of the account ends and this one stays.
func TestChangePassword_SetsItAndEndsOtherSessions(t *testing.T) {
	env := resetEnv(t)
	code, _, b := changePassword(t, env, "session-one", map[string]any{"currentPassword": userPassword, "newPassword": newPassword})
	require.Equal(t, http.StatusNoContent, code, string(b))
	assert.Empty(t, b)

	assert.Equal(t, http.StatusOK, meStatus(t, env, "session-one"), "this device stays signed in")
	for _, tok := range []string{"session-two", "non-admin-session", "any-user", "real-once-impl-exists"} {
		assert.Equal(t, http.StatusUnauthorized, meStatus(t, env, tok), "%s ended", tok)
	}
	assert.Equal(t, http.StatusOK, meStatus(t, env, "admin-session"), "another account is untouched")
	assert.Equal(t, http.StatusOK, loginStatus(t, env, "user@homepad.test", newPassword))
	assert.Equal(t, http.StatusUnauthorized, loginStatus(t, env, "user@homepad.test", userPassword))

	// An admin changes theirs the same way.
	code, _, b = changePassword(t, env, "admin-session", map[string]any{"currentPassword": adminPassword, "newPassword": newPassword})
	assert.Equal(t, http.StatusNoContent, code, string(b))
}

// AC-014 — a wrong current password is 403 wrong_password, changes nothing
// and keeps the session.
func TestChangePassword_WrongCurrentPassword(t *testing.T) {
	env := resetEnv(t)
	code, _, b := changePassword(t, env, "session-one", map[string]any{"currentPassword": "not my password", "newPassword": newPassword})
	require.Equal(t, http.StatusForbidden, code, string(b))
	assert.Equal(t, "wrong_password", errorCode(t, b))
	assert.Equal(t, http.StatusOK, meStatus(t, env, "session-one"), "never signs them out")
	assert.Equal(t, http.StatusOK, meStatus(t, env, "session-two"), "nor anywhere else")
	assert.Equal(t, http.StatusOK, loginStatus(t, env, "user@homepad.test", userPassword), "the password is unchanged")

	// A current password over 72 bytes whose first 72 are right is still wrong.
	code, _, _ = changePassword(t, env, "session-one", map[string]any{"currentPassword": userPassword + strings.Repeat("x", 72), "newPassword": newPassword})
	assert.Equal(t, http.StatusForbidden, code)
}

// AC-014 — kids are refused by class; SSO-only accounts get 409; the new
// password follows the rules; the body is strict.
func TestChangePassword_Refusals(t *testing.T) {
	env := resetEnv(t)
	_, kidToken := env.SeedKid(t, "Maya")
	_, ssoToken := seedSSOUser(t, env, "pat@example.com")
	good := map[string]any{"currentPassword": userPassword, "newPassword": newPassword}

	r := doNoCookie(t, http.MethodPost, env.URL+"/api/me/password", good)
	readAll(t, r)
	assert.Equal(t, http.StatusUnauthorized, r.StatusCode, "no session")

	code, _, _ := changePassword(t, env, kidToken, good)
	assert.Equal(t, http.StatusForbidden, code, "a kid")

	code, _, b := changePassword(t, env, ssoToken, map[string]any{"currentPassword": "anything at all", "newPassword": newPassword})
	require.Equal(t, http.StatusConflict, code, string(b))
	assert.Equal(t, "sso_account", errorCode(t, b))

	for name, body := range map[string]any{
		"new under 8 characters": map[string]any{"currentPassword": userPassword, "newPassword": "short"},
		"new over 72 bytes":      map[string]any{"currentPassword": userPassword, "newPassword": strings.Repeat("é", 37)},
		"unknown field":          map[string]any{"currentPassword": userPassword, "newPassword": newPassword, "userId": env.AdminID},
		"missing current":        map[string]any{"newPassword": newPassword},
		"missing new":            map[string]any{"currentPassword": userPassword},
		"not JSON":               "nope",
	} {
		code, _, b := changePassword(t, env, "session-one", body)
		assert.Equal(t, http.StatusBadRequest, code, "%s: %s", name, b)
	}
	code, _, b = changePassword(t, env, "session-one", map[string]any{"currentPassword": userPassword, "newPassword": strings.Repeat("é", 37)})
	require.Equal(t, http.StatusBadRequest, code)
	assert.Contains(t, string(b), "password must be at most 72 bytes")

	assert.Equal(t, http.StatusOK, loginStatus(t, env, "user@homepad.test", userPassword), "no refusal changed the password")
	assert.Equal(t, http.StatusOK, meStatus(t, env, "session-two"), "no refusal ended a session")
}

// §3.3 — 5 wrong current passwords in 15 minutes lock the route for that
// account: 429 with Retry-After, even with the right password. Other
// accounts are unaffected.
func TestChangePassword_ThrottlesWrongGuesses(t *testing.T) {
	env := resetEnv(t)
	for i := 0; i < 5; i++ {
		code, _, b := changePassword(t, env, "session-one", map[string]any{"currentPassword": fmt.Sprintf("guess number %d", i), "newPassword": newPassword})
		require.Equal(t, http.StatusForbidden, code, "guess %d: %s", i, b)
	}
	code, h, b := changePassword(t, env, "session-one", map[string]any{"currentPassword": userPassword, "newPassword": newPassword})
	require.Equal(t, http.StatusTooManyRequests, code, string(b))
	retry, err := strconv.Atoi(h.Get("Retry-After"))
	require.NoError(t, err)
	assert.Greater(t, retry, 14*60, "about 15 minutes")
	assert.LessOrEqual(t, retry, 15*60)
	assert.Equal(t, http.StatusOK, meStatus(t, env, "session-one"), "the lock signs nobody out")
	assert.Equal(t, http.StatusOK, loginStatus(t, env, "user@homepad.test", userPassword), "the password is unchanged")

	code, _, b = changePassword(t, env, "admin-session", map[string]any{"currentPassword": adminPassword, "newPassword": newPassword})
	assert.Equal(t, http.StatusNoContent, code, "another account is not locked: %s", b)
}

// AC-015 — one §3.4 line for each create, revoke, use and change; none holds
// a token, a hash, an email or a password.
func TestPasswordAuditLines(t *testing.T) {
	logs := captureLog(t)
	env := resetEnv(t)

	first := mustResetLink(t, env, env.UserID)
	second := mustResetLink(t, env, env.UserID)
	r, b := acceptReset(t, env, map[string]any{"token": second.Token, "password": newPassword})
	require.Equal(t, http.StatusOK, r.StatusCode, string(b))

	code, _, b := changePassword(t, env, "admin-session", map[string]any{"currentPassword": adminPassword, "newPassword": "another new password"})
	require.Equal(t, http.StatusNoContent, code, string(b))
	for i := 0; i < 3; i++ {
		code, _, _ := changePassword(t, env, "admin-session", map[string]any{"currentPassword": "a wrong guess", "newPassword": newPassword})
		require.Equal(t, http.StatusForbidden, code)
	}

	out := logs.String()
	count := func(s string) int { return strings.Count(out, s) }
	assert.Equal(t, 1, count(fmt.Sprintf("reset: admin %s created reset link %s for user %s expires=", env.AdminID, first.ID, env.UserID)), out)
	assert.Equal(t, 1, count(fmt.Sprintf("reset: admin %s created reset link %s for user %s expires=", env.AdminID, second.ID, env.UserID)), out)
	assert.Equal(t, 1, count(fmt.Sprintf("reset: link %s revoked by a newer link", first.ID)), out)
	// The fixture user has five sessions, all ended by the use.
	assert.Equal(t, 1, count(fmt.Sprintf("reset: link %s used, user %s password set, 5 sessions ended", second.ID, env.UserID)), out)
	assert.Equal(t, 1, count(fmt.Sprintf("password: user %s changed their password, 0 other sessions ended", env.AdminID)), out)
	assert.Equal(t, 1, count(fmt.Sprintf("password: user %s wrong current password", env.AdminID)), "throttled to one line a minute: %s", out)

	for _, tok := range []string{first.Token, second.Token} {
		sum := sha256.Sum256([]byte(tok))
		for _, form := range []string{tok, hex.EncodeToString(sum[:]), base64.StdEncoding.EncodeToString(sum[:]), base64.RawURLEncoding.EncodeToString(sum[:])} {
			assert.NotContains(t, out, form, "no line carries a token or its hash")
		}
	}
	for _, secret := range []string{"user@homepad.test", "admin@homepad.test", newPassword, "another new password", adminPassword, "a wrong guess"} {
		assert.NotContains(t, out, secret)
	}
}

// AC-020 — GET /api/me says whether the account has a local password.
func TestMe_HasPassword(t *testing.T) {
	env := resetEnv(t)
	_, kidToken := env.SeedKid(t, "Maya")
	_, ssoToken := seedSSOUser(t, env, "pat@example.com")
	for tok, want := range map[string]bool{"admin-session": true, "non-admin-session": true, ssoToken: false, kidToken: false} {
		r := doJSON(t, http.MethodGet, env.URL+"/api/me", tok, nil)
		b := readAll(t, r)
		require.Equal(t, http.StatusOK, r.StatusCode)
		var v map[string]any
		require.NoError(t, json.Unmarshal(b, &v))
		assert.Equal(t, want, v["hasPassword"], "%s: %s", tok, b)
	}
}

// AC-021 — every Accounts row carries hasPassword: false for kids and for
// SSO-only accounts.
func TestAccounts_HasPassword(t *testing.T) {
	env := resetEnv(t)
	kidID, _ := env.SeedKid(t, "Maya")
	ssoID, _ := seedSSOUser(t, env, "pat@example.com")

	r := doJSON(t, http.MethodGet, env.URL+"/api/admin/accounts", "admin-session", nil)
	b := readAll(t, r)
	require.Equal(t, http.StatusOK, r.StatusCode)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(b, &rows))
	want := map[string]bool{env.AdminID: true, env.UserID: true, ssoID: false, kidID: false}
	seen := 0
	for _, row := range rows {
		v, ok := row["hasPassword"]
		require.True(t, ok, "every row has hasPassword: %v", row)
		if w, ok := want[row["id"].(string)]; ok {
			assert.Equal(t, w, v, "%v", row)
			seen++
		}
	}
	assert.Equal(t, len(want), seen)
}

// AC-005 — racing creates for one account leave exactly one live link.
func TestResetLink_ConcurrentCreatesLeaveOneLiveLink(t *testing.T) {
	env := resetEnv(t)
	done := make(chan int, 5)
	for i := 0; i < 5; i++ {
		go func() {
			req, _ := http.NewRequest(http.MethodPost, env.URL+"/api/admin/accounts/"+env.UserID+"/reset-link", nil)
			req.AddCookie(&http.Cookie{Name: "homepad_session", Value: "admin-session"})
			r, err := http.DefaultClient.Do(req)
			if err != nil {
				done <- 0
				return
			}
			r.Body.Close()
			done <- r.StatusCode
		}()
	}
	for i := 0; i < 5; i++ {
		assert.Equal(t, http.StatusCreated, <-done)
	}
	var live int
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM password_resets WHERE user_id = $1 AND used_at IS NULL AND revoked_at IS NULL AND expires_at > now()`,
		env.UserID).Scan(&live))
	assert.Equal(t, 1, live)
}

// liveResetCount counts the live links of account userID.
func liveResetCount(t *testing.T, env *testsupport.Env, userID string) int {
	t.Helper()
	var n int
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM password_resets WHERE user_id = $1 AND used_at IS NULL AND revoked_at IS NULL AND expires_at > now()`,
		userID).Scan(&n))
	return n
}

// PR #107 review — removing an admin revokes the live reset links they made,
// as it revokes their pending invites (SPEC-household-manage-adults Q3).
// Otherwise the removed admin could still use a link they kept to take over
// the account it was made for. Other admins' links stay live.
func TestResetLink_RemovingAnAdminRevokesTheLinksTheyMade(t *testing.T) {
	logs := captureLog(t)
	env := resetEnv(t)
	alex, alexTok := addAdmin(t, env, "alex@homepad.test")
	other := seedOtherAdmin(t, env)

	code, theirs, b := createResetLink(t, env, alexTok, env.UserID)
	require.Equal(t, http.StatusCreated, code, string(b))
	mine := mustResetLink(t, env, other)

	code, b = removeAdult(t, env, "admin-session", alex)
	require.Equal(t, http.StatusNoContent, code, string(b))

	code, _ = lookupReset(t, env, theirs.Token)
	assert.Equal(t, http.StatusNotFound, code, "the removed admin's link is dead")
	r, _ := acceptReset(t, env, map[string]any{"token": theirs.Token, "password": newPassword})
	assert.Equal(t, http.StatusNotFound, r.StatusCode)
	assert.Equal(t, http.StatusOK, loginStatus(t, env, "user@homepad.test", userPassword), "the password is unchanged")

	code, _ = lookupReset(t, env, mine.Token)
	assert.Equal(t, http.StatusOK, code, "another admin's link stays live")
	assert.Contains(t, logs.String(), fmt.Sprintf("reset: revoked 1 live reset links made by removed adult %s", alex))
}

// PR #107 review — demoting an admin revokes the live reset links they made:
// a member can't make one, so a demoted admin must not keep one either.
// Promoting a member, or a role that doesn't change, revokes nothing.
func TestResetLink_DemotingAnAdminRevokesTheLinksTheyMade(t *testing.T) {
	logs := captureLog(t)
	env := resetEnv(t)
	alex, alexTok := addAdmin(t, env, "alex@homepad.test")
	other := seedOtherAdmin(t, env)

	code, theirs, b := createResetLink(t, env, alexTok, env.UserID)
	require.Equal(t, http.StatusCreated, code, string(b))
	mine := mustResetLink(t, env, other)

	// A no-op (already an admin) revokes nothing.
	code, b = patchRole(t, env, "admin-session", alex, map[string]any{"role": "admin"})
	require.Equal(t, http.StatusOK, code, string(b))
	require.Equal(t, 1, liveResetCount(t, env, env.UserID), "a no-op keeps the link")

	code, b = patchRole(t, env, "admin-session", alex, map[string]any{"role": "user"})
	require.Equal(t, http.StatusOK, code, string(b))

	code, _ = lookupReset(t, env, theirs.Token)
	assert.Equal(t, http.StatusNotFound, code, "the demoted admin's link is dead")
	r, _ := acceptReset(t, env, map[string]any{"token": theirs.Token, "password": newPassword})
	assert.Equal(t, http.StatusNotFound, r.StatusCode)
	code, _ = lookupReset(t, env, mine.Token)
	assert.Equal(t, http.StatusOK, code, "another admin's link stays live")
	assert.Contains(t, logs.String(), fmt.Sprintf("reset: revoked 1 live reset links made by demoted admin %s", alex))

	// Promoting them back revokes nothing.
	code, b = patchRole(t, env, "admin-session", alex, map[string]any{"role": "admin"})
	require.Equal(t, http.StatusOK, code, string(b))
	assert.Equal(t, 1, liveResetCount(t, env, other), "a promotion keeps other links")
	assert.Equal(t, 1, strings.Count(logs.String(), "live reset links made by"), "only the demotion logged a revoke")
}

// PR #107 review — changing your password revokes your live reset link in
// the same step, so whoever holds that link can't replace the password you
// just chose. Another account's link stays live.
func TestChangePassword_RevokesTheLiveResetLink(t *testing.T) {
	logs := captureLog(t)
	env := resetEnv(t)
	other := seedOtherAdmin(t, env)
	link := mustResetLink(t, env, env.UserID)
	theirs := mustResetLink(t, env, other)

	code, _, b := changePassword(t, env, "session-one", map[string]any{"currentPassword": userPassword, "newPassword": newPassword})
	require.Equal(t, http.StatusNoContent, code, string(b))

	code, _ = lookupReset(t, env, link.Token)
	assert.Equal(t, http.StatusNotFound, code, "the account's link is dead")
	r, _ := acceptReset(t, env, map[string]any{"token": link.Token, "password": "the attacker's password"})
	assert.Equal(t, http.StatusNotFound, r.StatusCode)
	assert.Equal(t, http.StatusOK, loginStatus(t, env, "user@homepad.test", newPassword), "the new password stands")

	code, _ = lookupReset(t, env, theirs.Token)
	assert.Equal(t, http.StatusOK, code, "another account's link stays live")
	assert.Contains(t, logs.String(), fmt.Sprintf("reset: link %s revoked by a password change", link.ID))
}
