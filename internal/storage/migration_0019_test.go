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

// SPEC-household-basics §6 — migration 0019_invites and the storage half of
// invites, the account cap and #87's startup count.

var inviteSeq int

func inviteHash(t *testing.T) []byte {
	t.Helper()
	inviteSeq++
	sum := sha256.Sum256([]byte(fmt.Sprintf("invite-%s-%d-%d", t.Name(), inviteSeq, time.Now().UnixNano())))
	return sum[:]
}

// Migrate re-runs every migration on every boot: 0019 must be a no-op the
// second time, and invites must survive it.
func TestMigration0019_InvitesTableIsIdempotent(t *testing.T) {
	store, ctx := openStore(t)
	admin := seedStoreAdmin(t, store, ctx)
	inv, err := store.CreateInvite(ctx, inviteHash(t), "Alex", "user", admin, 7*24*time.Hour, 0)
	require.NoError(t, err)

	require.NoError(t, store.Migrate(ctx))
	require.NoError(t, store.Migrate(ctx))

	var n int
	require.NoError(t, store.Pool().QueryRow(ctx, `SELECT count(*) FROM invites WHERE id = $1`, inv.ID).Scan(&n))
	assert.Equal(t, 1, n, "a re-run keeps existing invites")

	var idx int
	require.NoError(t, store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes WHERE tablename = 'invites' AND indexname = 'invites_pending_idx'`).Scan(&idx))
	assert.Equal(t, 1, idx, "the pending index exists")
}

// The schema backs the handler up: a token_hash must be a 32-byte digest, and
// the role can only be admin or user — never kid.
func TestMigration0019_Checks(t *testing.T) {
	store, ctx := openStore(t)
	admin := seedStoreAdmin(t, store, ctx)
	conn := rawConn(t, ctx)

	_, err := conn.Exec(ctx,
		`INSERT INTO invites (token_hash, name, role, created_by, expires_at) VALUES ($1, 'x', 'user', $2, now() + interval '1 day')`,
		[]byte("not a digest"), admin)
	assert.Error(t, err, "a token_hash that isn't 32 bytes is refused")

	for _, role := range []string{"kid", "owner", ""} {
		_, err := conn.Exec(ctx,
			`INSERT INTO invites (token_hash, name, role, created_by, expires_at) VALUES ($1, 'x', $2, $3, now() + interval '1 day')`,
			inviteHash(t), role, admin)
		assert.Error(t, err, "role %q is refused by the CHECK", role)
	}

	h := inviteHash(t)
	_, err = conn.Exec(ctx,
		`INSERT INTO invites (token_hash, name, role, created_by, expires_at) VALUES ($1, 'x', 'admin', $2, now() + interval '1 day')`, h, admin)
	require.NoError(t, err)
	_, err = conn.Exec(ctx,
		`INSERT INTO invites (token_hash, name, role, created_by, expires_at) VALUES ($1, 'y', 'user', $2, now() + interval '1 day')`, h, admin)
	assert.Error(t, err, "token_hash is unique")
}

// §3.3 / §6 — the sweeper deletes rows whose expiry, use or revocation is more
// than 30 days old, and keeps everything else.
func TestStore_DeleteStaleInvites(t *testing.T) {
	store, ctx := openStore(t)
	admin := seedStoreAdmin(t, store, ctx)
	conn := rawConn(t, ctx)

	mk := func(set string) string {
		t.Helper()
		inv, err := store.CreateInvite(ctx, inviteHash(t), "Sweep", "user", admin, 7*24*time.Hour, 0)
		require.NoError(t, err)
		if set != "" {
			_, err = conn.Exec(ctx, `UPDATE invites SET `+set+` WHERE id = $1`, inv.ID)
			require.NoError(t, err)
		}
		return inv.ID
	}
	pending := mk("")
	oldExpired := mk(`created_at = now() - interval '40 days', expires_at = now() - interval '33 days'`)
	recentExpired := mk(`created_at = now() - interval '10 days', expires_at = now() - interval '3 days'`)
	oldUsed := mk(`used_at = now() - interval '31 days'`)
	recentUsed := mk(`used_at = now() - interval '29 days'`)
	oldRevoked := mk(`revoked_at = now() - interval '45 days'`)
	recentRevoked := mk(`revoked_at = now() - interval '1 day'`)

	n, err := store.DeleteStaleInvites(ctx, 30*24*time.Hour)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, int64(3))

	exists := func(id string) bool {
		var c int
		require.NoError(t, conn.QueryRow(ctx, `SELECT count(*) FROM invites WHERE id = $1`, id).Scan(&c))
		return c == 1
	}
	for name, id := range map[string]string{"pending": pending, "recently expired": recentExpired, "recently used": recentUsed, "recently revoked": recentRevoked} {
		assert.True(t, exists(id), "%s is kept", name)
	}
	for name, id := range map[string]string{"expired 33 days ago": oldExpired, "used 31 days ago": oldUsed, "revoked 45 days ago": oldRevoked} {
		assert.False(t, exists(id), "%s is swept", name)
	}
}

// Deleting the admin who made an invite keeps the row (created_by goes NULL),
// so the audit trail and a pending link outlive the account.
func TestStore_InviteSurvivesItsCreator(t *testing.T) {
	store, ctx := openStore(t)
	admin := seedStoreAdmin(t, store, ctx)
	inv, err := store.CreateInvite(ctx, inviteHash(t), "Alex", "user", admin, 7*24*time.Hour, 0)
	require.NoError(t, err)
	_, err = store.Pool().Exec(ctx, `DELETE FROM users WHERE id = $1`, admin)
	require.NoError(t, err)
	var createdBy *string
	require.NoError(t, store.Pool().QueryRow(ctx, `SELECT created_by::text FROM invites WHERE id = $1`, inv.ID).Scan(&createdBy))
	assert.Nil(t, createdBy)
}

// AC-026 — the startup count of catalog rows owned by an admin who is not the
// shared catalog owner (#87's invisible rows). Rows of the owner, and rows of
// plain users (the #224 era), are not counted.
func TestStore_NonOwnerAdminCatalogRows(t *testing.T) {
	store, ctx := openStore(t)
	// Make sure an owner exists that is older than the admin created below.
	seedStoreAdmin(t, store, ctx)
	beforeSvc, beforeCat, err := store.NonOwnerAdminCatalogRows(ctx)
	require.NoError(t, err)

	second := seedStoreAdmin(t, store, ctx)
	owner, err := store.SharedCatalogOwnerID(ctx)
	require.NoError(t, err)
	require.NotEqual(t, owner, second)
	seedStoreService(t, store, ctx, second)
	seedStoreService(t, store, ctx, second)
	_, err = store.CreateCategory(ctx, second, fmt.Sprintf("orphan-%d", time.Now().UnixNano()))
	require.NoError(t, err)
	// The owner's own rows don't count.
	seedStoreService(t, store, ctx, owner)
	// Nor do a plain user's.
	u, err := store.CreateUser(ctx, fmt.Sprintf("plain-%d@example.com", time.Now().UnixNano()), "x", "user")
	require.NoError(t, err)
	seedStoreService(t, store, ctx, u.ID)

	svc, cat, err := store.NonOwnerAdminCatalogRows(ctx)
	require.NoError(t, err)
	assert.Equal(t, beforeSvc+2, svc)
	assert.Equal(t, beforeCat+1, cat)
}

// AC-028 — CountAccounts is every account (kids too) plus pending invites.
func TestStore_CountAccounts(t *testing.T) {
	store, ctx := openStore(t)
	admin := seedStoreAdmin(t, store, ctx)
	before, err := store.CountAccounts(ctx)
	require.NoError(t, err)

	_, err = store.CreateKid(ctx, uniqueKidName(t), "blue")
	require.NoError(t, err)
	pending, err := store.CreateInvite(ctx, inviteHash(t), "P", "user", admin, 7*24*time.Hour, 0)
	require.NoError(t, err)
	revoked, err := store.CreateInvite(ctx, inviteHash(t), "R", "user", admin, 7*24*time.Hour, 0)
	require.NoError(t, err)
	_, err = store.RevokeInvite(ctx, revoked.ID)
	require.NoError(t, err)

	after, err := store.CountAccounts(ctx)
	require.NoError(t, err)
	assert.Equal(t, before+2, after, "the kid and the one pending invite")
	require.NotEmpty(t, pending.ID)

	_, err = store.CreateInvite(ctx, inviteHash(t), "Over", "user", admin, 7*24*time.Hour, after)
	assert.ErrorIs(t, err, storage.ErrAccountLimit, "at the cap, an invite is refused")
	_, err = store.CreateKidWithLimit(ctx, uniqueKidName(t), "blue", after)
	assert.ErrorIs(t, err, storage.ErrAccountLimit, "at the cap, a kid is refused")
	_, err = store.CreateKidWithLimit(ctx, uniqueKidName(t), "blue", after+1)
	assert.NoError(t, err, "under the cap, a kid fits")
}
