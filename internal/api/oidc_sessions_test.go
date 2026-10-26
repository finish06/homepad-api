package api_test

import (
	"context"
	"crypto/sha256"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/testsupport"
)

// AC-010 for the OIDC path: a PocketID sign-in creates the same persistent,
// hashed session as a local login — a Postgres row keyed by sha256(token), a
// cookie with the adult Max-Age — and it survives an API restart.
func TestOIDCLoginCreatesPersistentSession(t *testing.T) {
	idp := newMockIdP(t, "persist@calebdunn.tech", []string{"everyone"})
	homepad, store := testsupport.NewOIDCServer(t, idp.config("homepad-admins"))

	cookie := runOIDCLogin(t, homepad, idp)
	assert.Equal(t, adultMaxAge, cookie.MaxAge, "the OIDC cookie carries the adult Max-Age")
	assert.True(t, cookie.HttpOnly)
	assert.Equal(t, http.SameSiteLaxMode, cookie.SameSite)

	ctx := context.Background()
	u, err := store.UserByEmail(ctx, "persist@calebdunn.tech")
	require.NoError(t, err)
	sum := sha256.Sum256([]byte(cookie.Value))
	var n int
	require.NoError(t, store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM sessions WHERE user_id = $1 AND token_hash = $2`, u.ID, sum[:]).Scan(&n))
	assert.Equal(t, 1, n, "the OIDC callback persisted a hashed session row")

	// Restart: a new process on the same DB still honours the cookie.
	homepad.Close()
	restarted := startAPI(t, openStore(t, store.DSN), nil)
	assert.Equal(t, http.StatusOK, meWithCookie(t, restarted, cookie).StatusCode,
		"an OIDC session survives an API restart")
}
