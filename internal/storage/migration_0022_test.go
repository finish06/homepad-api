package storage_test

import (
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
