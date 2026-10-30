package api_test

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/session"
	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// #84 — an uploaded icon lives on a shared-catalog service, which is owned by
// the first admin. Every account reads that catalog, so every account must be
// able to read its icons: the owner, a plain user, and a second admin alike.

// seedSecondAdmin creates an admin who is NOT the shared-catalog owner (the
// owner is the first admin) and signs them in. It returns the session token.
func seedSecondAdmin(t *testing.T, env *testsupport.Env) string {
	t.Helper()
	ctx := context.Background()
	hash, err := bcrypt.GenerateFromPassword([]byte("second-admin-pw"), bcrypt.MinCost)
	require.NoError(t, err)
	u, err := env.Store.CreateUser(ctx, "admin2@homepad.test", string(hash), "admin")
	require.NoError(t, err)
	const token = "second-admin-session"
	require.NoError(t, env.Sessions.Bind(ctx, token, u.ID, session.AdultLifetime))

	owner, err := env.Store.SharedCatalogOwnerID(ctx)
	require.NoError(t, err)
	require.Equal(t, env.AdminID, owner, "the first admin stays the catalog owner")
	require.NotEqual(t, owner, u.ID, "the second admin must not be the owner")
	return token
}

// #84 — the owner uploads light+dark; the owner, a plain user and a second
// admin each GET 200 with the exact uploaded bytes for both variants.
func TestIcons_EveryAdultReadsOwnerUploads(t *testing.T) {
	env := testsupport.NewEnv(t)
	secondAdmin := seedSecondAdmin(t, env)
	id := firstServiceID(t, env.URL, "admin-session")

	uploads := map[string][]byte{"light": pngBytes(t, 48, 48), "dark": pngBytes(t, 64, 64)}
	for variant, body := range uploads {
		r := putIcon(t, env.URL, "admin-session", id, variant, body)
		r.Body.Close()
		require.Equal(t, http.StatusNoContent, r.StatusCode, "owner PUT %s", variant)
	}

	for _, who := range []struct{ name, token string }{
		{"catalog owner", "admin-session"},
		{"plain user", "non-admin-session"},
		{"second admin", secondAdmin},
	} {
		// The service the owner uploaded to is in this account's catalog.
		assert.Contains(t, idsOf(listServicesFull(t, env.URL, who.token)), id,
			"%s sees the service in the shared catalog", who.name)
		for variant, want := range uploads {
			code, got := getBody(t, env.URL+"/api/services/"+id+"/icon/"+variant, who.token)
			assert.Equal(t, http.StatusOK, code, "%s GET %s icon", who.name, variant)
			assert.True(t, bytes.Equal(want, got), "%s gets the uploaded %s bytes", who.name, variant)
		}
	}
}

// #84 — the conditional GET works for a non-owner too: the ETag a plain user
// receives revalidates to 304.
func TestIcons_NonOwnerETagConditional304(t *testing.T) {
	env := testsupport.NewEnv(t)
	id := firstServiceID(t, env.URL, "admin-session")
	r := putIcon(t, env.URL, "admin-session", id, "light", pngBytes(t, 32, 32))
	r.Body.Close()
	require.Equal(t, http.StatusNoContent, r.StatusCode)

	first := doJSON(t, http.MethodGet, env.URL+"/api/services/"+id+"/icon/light", "non-admin-session", nil)
	first.Body.Close()
	require.Equal(t, http.StatusOK, first.StatusCode)
	etag := first.Header.Get("ETag")
	require.NotEmpty(t, etag)

	req, _ := http.NewRequest(http.MethodGet, env.URL+"/api/services/"+id+"/icon/light", nil)
	req.AddCookie(&http.Cookie{Name: "homepad_session", Value: "non-admin-session"})
	req.Header.Set("If-None-Match", etag)
	second, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	second.Body.Close()
	assert.Equal(t, http.StatusNotModified, second.StatusCode)
}

// #84 — adults still get a plain 404 for a variant with no upload and for a
// service that does not exist.
func TestIcons_NonOwnerMissingIs404(t *testing.T) {
	env := testsupport.NewEnv(t)
	id := firstServiceID(t, env.URL, "admin-session")
	for _, url := range []string{
		env.URL + "/api/services/" + id + "/icon/dark",
		env.URL + "/api/services/00000000-0000-0000-0000-000000000000/icon/light",
		env.URL + "/api/services/not-a-uuid/icon/light",
	} {
		code, _ := getBody(t, url, "non-admin-session")
		assert.Equal(t, http.StatusNotFound, code, url)
	}
}

// #84 — the kid path is unchanged by the fix: an allowed service's icon is
// served, an off-list one is still 404 and reads exactly like a missing
// service, even though every adult can read that same off-list icon.
func TestIcons_KidPathUnchanged(t *testing.T) {
	w := newKidWorld(t)
	secondAdmin := seedSecondAdmin(t, w.env)
	allowedPNG := pngBytes(t, 32, 32)
	hiddenPNG := pngBytes(t, 40, 40)
	for id, body := range map[string][]byte{w.allowedID: allowedPNG, w.hiddenID: hiddenPNG} {
		r := putIcon(t, w.env.URL, "admin-session", id, "light", body)
		r.Body.Close()
		require.Equal(t, http.StatusNoContent, r.StatusCode)
	}

	code, body := getBody(t, w.env.URL+"/api/services/"+w.allowedID+"/icon/light", w.kidToken)
	assert.Equal(t, http.StatusOK, code, "the kid reads its allowed service's icon")
	assert.True(t, bytes.Equal(allowedPNG, body))

	hiddenCode, hiddenBody := getBody(t, w.env.URL+"/api/services/"+w.hiddenID+"/icon/light", w.kidToken)
	assert.Equal(t, http.StatusNotFound, hiddenCode, "AC-006: off-list icon is still 404 for the kid")
	_, missingBody := getBody(t, w.env.URL+"/api/services/00000000-0000-0000-0000-000000000000/icon/light", w.kidToken)
	assert.Equal(t, string(missingBody), string(hiddenBody), "hidden reads exactly like missing")

	for _, tok := range []string{"admin-session", "non-admin-session", secondAdmin} {
		c, b := getBody(t, w.env.URL+"/api/services/"+w.hiddenID+"/icon/light", tok)
		assert.Equal(t, http.StatusOK, c, "adult %s reads the kid-hidden icon", tok)
		assert.True(t, bytes.Equal(hiddenPNG, b))
	}
}
