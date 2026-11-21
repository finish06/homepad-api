package storage_test

import (
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// SPEC-password-reset-link §6 — migration 0022_password_resets and the
// storage half of reset links.

var resetSeq int

func resetHash(t *testing.T) []byte {
	t.Helper()
	resetSeq++
	sum := sha256.Sum256([]byte(fmt.Sprintf("reset-%s-%d-%d", t.Name(), resetSeq, time.Now().UnixNano())))
	return sum[:]
}

// The new runner runs 0022 once; it is still written to be re-run, and a
// re-run keeps every row.
func TestMigration0022_PasswordResetsTableIsIdempotent(t *testing.T) {
	store, ctx := openStore(t)
	admin := seedStoreAdmin(t, store, ctx)
	user := seedStoreUser(t, store, ctx)
	link, _, err := store.CreatePasswordReset(ctx, resetHash(t), user, admin, 24*time.Hour)
	require.NoError(t, err)

	rerunMigration(t, ctx, store, "0022_password_resets.up.sql")
	rerunMigration(t, ctx, store, "0022_password_resets.up.sql")

	var n int
	require.NoError(t, store.Pool().QueryRow(ctx, `SELECT count(*) FROM password_resets WHERE id = $1`, link.ID).Scan(&n))
	assert.Equal(t, 1, n, "a re-run keeps existing links")

	var idx int
	require.NoError(t, store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes WHERE tablename = 'password_resets' AND indexname = 'password_resets_live_idx'`).Scan(&idx))
	assert.Equal(t, 1, idx, "the live index exists")
}

// The schema backs the handler up: a 32-byte digest only, unique; removing
// the account removes its links; removing the admin who made one keeps it.
func TestMigration0022_Checks(t *testing.T) {
	store, ctx := openStore(t)
	admin := seedStoreAdmin(t, store, ctx)
	user := seedStoreUser(t, store, ctx)
	conn := rawConn(t, ctx)

	_, err := conn.Exec(ctx,
		`INSERT INTO password_resets (token_hash, user_id, created_by, expires_at) VALUES ($1, $2, $3, now() + interval '1 day')`,
		[]byte("not a digest"), user, admin)
	assert.Error(t, err, "a token_hash that isn't 32 bytes is refused")

	h := resetHash(t)
	_, err = conn.Exec(ctx,
		`INSERT INTO password_resets (token_hash, user_id, created_by, expires_at) VALUES ($1, $2, $3, now() + interval '1 day')`, h, user, admin)
	require.NoError(t, err)
	_, err = conn.Exec(ctx,
		`INSERT INTO password_resets (token_hash, user_id, created_by, expires_at) VALUES ($1, $2, NULL, now() + interval '1 day')`, h, user)
	assert.Error(t, err, "token_hash is unique")

	_, err = conn.Exec(ctx, `DELETE FROM users WHERE id = $1`, admin)
	require.NoError(t, err)
	var createdBy *string
	require.NoError(t, conn.QueryRow(ctx, `SELECT created_by::text FROM password_resets WHERE token_hash = $1`, h).Scan(&createdBy))
	assert.Nil(t, createdBy, "the maker's removal keeps the link, with no maker")

	_, err = conn.Exec(ctx, `DELETE FROM users WHERE id = $1`, user)
	require.NoError(t, err)
	var n int
	require.NoError(t, conn.QueryRow(ctx, `SELECT count(*) FROM password_resets WHERE token_hash = $1`, h).Scan(&n))
	assert.Zero(t, n, "removing the account removes its links")
}

// §6 — the hourly sweep deletes links whose expiry, use or revocation is
// more than 30 days old, and keeps everything else.
func TestStore_DeleteStalePasswordResets(t *testing.T) {
	store, ctx := openStore(t)
	admin := seedStoreAdmin(t, store, ctx)
	_, err := store.Pool().Exec(ctx, `DELETE FROM password_resets`)
	require.NoError(t, err)

	mk := func(set string) string {
		user := seedStoreUser(t, store, ctx)
		link, _, err := store.CreatePasswordReset(ctx, resetHash(t), user, admin, 24*time.Hour)
		require.NoError(t, err)
		if set != "" {
			_, err = store.Pool().Exec(ctx, `UPDATE password_resets SET `+set+` WHERE id = $1`, link.ID)
			require.NoError(t, err)
		}
		return link.ID
	}
	live := mk("")
	recentlyUsed := mk("used_at = now() - interval '29 days'")
	oldUsed := mk("used_at = now() - interval '31 days'")
	oldRevoked := mk("revoked_at = now() - interval '31 days'")
	oldExpired := mk("created_at = now() - interval '32 days', expires_at = now() - interval '31 days'")
	recentlyExpired := mk("created_at = now() - interval '3 days', expires_at = now() - interval '2 days'")

	n, err := store.DeleteStalePasswordResets(ctx, 30*24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, int64(3), n)
	for id, want := range map[string]int{live: 1, recentlyUsed: 1, recentlyExpired: 1, oldUsed: 0, oldRevoked: 0, oldExpired: 0} {
		var got int
		require.NoError(t, store.Pool().QueryRow(ctx, `SELECT count(*) FROM password_resets WHERE id = $1`, id).Scan(&got))
		assert.Equal(t, want, got, id)
	}
}

// Who a link may be for, checked under the account row lock: an adult with a
// local password. A kid, an unknown or malformed id is ErrNotFound; an
// SSO-only adult is ErrSSOAccount. Nothing is stored for a refusal.
func TestStore_CreatePasswordResetTargets(t *testing.T) {
	store, ctx := openStore(t)
	admin := seedStoreAdmin(t, store, ctx)
	kid, err := store.CreateKid(ctx, fmt.Sprintf("Kid %d", time.Now().UnixNano()), "blue")
	require.NoError(t, err)
	sso, err := store.CreateUser(ctx, fmt.Sprintf("sso-%d@example.com", time.Now().UnixNano()), "oidc:no-local-password", "user")
	require.NoError(t, err)

	for name, id := range map[string]string{"kid": kid.ID, "unknown": "00000000-0000-0000-0000-000000000000", "malformed": "nope"} {
		_, _, err := store.CreatePasswordReset(ctx, resetHash(t), id, admin, time.Hour)
		assert.ErrorIs(t, err, storage.ErrNotFound, name)
	}
	_, _, err = store.CreatePasswordReset(ctx, resetHash(t), sso.ID, admin, time.Hour)
	assert.ErrorIs(t, err, storage.ErrSSOAccount)

	var n int
	require.NoError(t, store.Pool().QueryRow(ctx, `SELECT count(*) FROM password_resets WHERE user_id IN ($1, $2)`, kid.ID, sso.ID).Scan(&n))
	assert.Zero(t, n)
}

// The link carries the account's display name, or its email when it has none.
func TestStore_PasswordResetName(t *testing.T) {
	store, ctx := openStore(t)
	user := seedStoreUser(t, store, ctx)
	_, err := store.Pool().Exec(ctx, `UPDATE users SET display_name = 'Jo' WHERE id = $1`, user)
	require.NoError(t, err)
	h := resetHash(t)
	link, revoked, err := store.CreatePasswordReset(ctx, h, user, "", time.Hour)
	require.NoError(t, err)
	assert.Empty(t, revoked)
	assert.Equal(t, "Jo", link.Name)
	got, err := store.LookupPasswordReset(ctx, h)
	require.NoError(t, err)
	assert.Equal(t, "Jo", got.Name)
	assert.Equal(t, link.ID, got.ID)

	_, _, err = store.UsePasswordReset(ctx, resetHash(t), "$2a$04$whatever")
	assert.ErrorIs(t, err, storage.ErrNotFound, "an unknown hash uses nothing")
	u, used, err := store.UsePasswordReset(ctx, h, "$2a$04$new-hash")
	require.NoError(t, err)
	assert.Equal(t, "Jo", used.Name)
	assert.Equal(t, "$2a$04$new-hash", u.PasswordHash)
}

// Change password writes only an adult's row.
func TestStore_SetPasswordHash(t *testing.T) {
	store, ctx := openStore(t)
	user := seedStoreUser(t, store, ctx)
	require.NoError(t, store.SetPasswordHash(ctx, user, "$2a$04$changed"))
	var got string
	require.NoError(t, store.Pool().QueryRow(ctx, `SELECT password_hash FROM users WHERE id = $1`, user).Scan(&got))
	assert.Equal(t, "$2a$04$changed", got)

	kid, err := store.CreateKid(ctx, fmt.Sprintf("Kid %d", time.Now().UnixNano()), "blue")
	require.NoError(t, err)
	for name, id := range map[string]string{"kid": kid.ID, "unknown": "00000000-0000-0000-0000-000000000000", "malformed": "nope"} {
		assert.ErrorIs(t, store.SetPasswordHash(ctx, id, "$2a$04$x"), storage.ErrNotFound, name)
	}
}
