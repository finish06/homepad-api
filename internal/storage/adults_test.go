package storage_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// SPEC-household-manage-adults §2.3 and §6 — the storage half of removing an
// adult and changing an adult's role. The last-admin rule counts every admin
// in the database, so these run in a database of their own (freshDB): the
// shared test database holds admins from other tests.

func adultsStore(t *testing.T, name string) (context.Context, *storage.Store) {
	t.Helper()
	ctx, conn := freshDB(t, name)
	dsn := conn.Config().ConnString()
	store, err := storage.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	require.NoError(t, store.Migrate(ctx))
	return ctx, store
}

func mkAdult(t *testing.T, ctx context.Context, store *storage.Store, email, role string) string {
	t.Helper()
	u, err := store.CreateUser(ctx, email, "x", role)
	require.NoError(t, err)
	return u.ID
}

func roleOf(t *testing.T, ctx context.Context, store *storage.Store, id string) string {
	t.Helper()
	u, err := store.UserByID(ctx, id)
	require.NoError(t, err)
	return u.Role
}

func countAdmins(t *testing.T, ctx context.Context, store *storage.Store) int {
	t.Helper()
	var n int
	require.NoError(t, store.Pool().QueryRow(ctx, `SELECT count(*) FROM users WHERE role = 'admin'`).Scan(&n))
	return n
}

func mkInvite(t *testing.T, ctx context.Context, store *storage.Store, createdBy, name string) storage.Invite {
	t.Helper()
	h := sha256.Sum256([]byte(fmt.Sprintf("invite-%s-%s-%d", createdBy, name, time.Now().UnixNano())))
	inv, err := store.CreateInvite(ctx, h[:], name, "user", createdBy, 7*24*time.Hour, 0)
	require.NoError(t, err)
	return inv
}

// AC-001, AC-004 — removing an adult deletes the row and revokes their
// pending invites; another admin's invites are untouched.
func TestRemoveAdult_DeletesAndRevokesTheirPendingInvites(t *testing.T) {
	ctx, store := adultsStore(t, "homepad_adults_remove_test")
	caleb := mkAdult(t, ctx, store, "caleb@adults.test", "admin")
	alex := mkAdult(t, ctx, store, "alex@adults.test", "admin")
	jo := mkAdult(t, ctx, store, "jo@adults.test", "user")

	mkInvite(t, ctx, store, alex, "Pat")
	mkInvite(t, ctx, store, alex, "Sam")
	mine := mkInvite(t, ctx, store, caleb, "Lee")

	removed, revoked, _, err := store.RemoveAdult(ctx, caleb, alex)
	require.NoError(t, err)
	assert.Equal(t, alex, removed.ID)
	assert.Equal(t, "admin", removed.Role, "the removed row is returned for the audit line")
	assert.Equal(t, int64(2), revoked, "Alex's two pending invites are revoked (Q3)")

	_, err = store.UserByID(ctx, alex)
	assert.True(t, errors.Is(err, storage.ErrNotFound), "the row is gone")

	pending, err := store.PendingInvites(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1, "only Caleb's invite is still pending")
	assert.Equal(t, mine.ID, pending[0].ID)

	// A member can be removed too (Q5), and has no invites to revoke.
	_, revoked, _, err = store.RemoveAdult(ctx, caleb, jo)
	require.NoError(t, err)
	assert.Zero(t, revoked)
}

// AC-004 — a used invite keeps its history: used_by stays, created_by goes to
// NULL with the deleted account (ON DELETE SET NULL).
func TestRemoveAdult_UsedInviteKeepsItsHistory(t *testing.T) {
	ctx, store := adultsStore(t, "homepad_adults_used_invite_test")
	caleb := mkAdult(t, ctx, store, "caleb@adults.test", "admin")
	alex := mkAdult(t, ctx, store, "alex@adults.test", "admin")

	h := sha256.Sum256([]byte("used-invite-token"))
	inv, err := store.CreateInvite(ctx, h[:], "Pat", "user", alex, 7*24*time.Hour, 0)
	require.NoError(t, err)
	pat, _, err := store.AcceptInvite(ctx, h[:], "pat@adults.test", "x", "")
	require.NoError(t, err)

	_, revoked, _, err := store.RemoveAdult(ctx, caleb, alex)
	require.NoError(t, err)
	assert.Zero(t, revoked, "a used invite is not pending, so it is not revoked")

	var usedBy *string
	var createdBy *string
	var revokedAt *time.Time
	require.NoError(t, store.Pool().QueryRow(ctx,
		`SELECT used_by::text, created_by::text, revoked_at FROM invites WHERE id = $1`, inv.ID,
	).Scan(&usedBy, &createdBy, &revokedAt))
	require.NotNil(t, usedBy)
	assert.Equal(t, pat.ID, *usedBy, "used_by keeps the person who joined")
	assert.Nil(t, createdBy, "created_by is SET NULL with the removed adult")
	assert.Nil(t, revokedAt, "a used invite is never marked revoked")
}

// AC-003, AC-005 — not an adult is ErrNotFound; acting on yourself is ErrSelf.
// Neither changes anything.
func TestAdults_NotFoundAndSelf(t *testing.T) {
	ctx, store := adultsStore(t, "homepad_adults_notfound_test")
	caleb := mkAdult(t, ctx, store, "caleb@adults.test", "admin")
	mkAdult(t, ctx, store, "alex@adults.test", "admin")
	kid, err := store.CreateKid(ctx, "Sam", "blue")
	require.NoError(t, err)

	for _, id := range []string{kid.ID, "00000000-0000-0000-0000-0000000000ff", "not-a-uuid", ""} {
		_, _, _, err := store.RemoveAdult(ctx, caleb, id)
		assert.True(t, errors.Is(err, storage.ErrNotFound), "RemoveAdult(%q) = %v", id, err)
		_, _, _, err = store.SetAdultRole(ctx, caleb, id, "user")
		assert.True(t, errors.Is(err, storage.ErrNotFound), "SetAdultRole(%q) = %v", id, err)
	}
	_, err = store.KidByID(ctx, kid.ID)
	assert.NoError(t, err, "the kid is untouched")

	_, _, _, err = store.RemoveAdult(ctx, caleb, caleb)
	assert.True(t, errors.Is(err, storage.ErrSelf), "RemoveAdult(self) = %v", err)
	_, _, _, err = store.SetAdultRole(ctx, caleb, caleb, "user")
	assert.True(t, errors.Is(err, storage.ErrSelf), "SetAdultRole(self) = %v", err)
	_, _, _, err = store.SetAdultRole(ctx, caleb, caleb, "admin")
	assert.True(t, errors.Is(err, storage.ErrSelf), "SetAdultRole(self, same role) = %v", err)
	assert.Equal(t, "admin", roleOf(t, ctx, store, caleb))
}

// AC-006 — admin ⇄ user both ways; the same role is a no-op that reports
// changed=false; any other role is refused.
func TestSetAdultRole_ChangesAndNoOp(t *testing.T) {
	ctx, store := adultsStore(t, "homepad_adults_role_test")
	caleb := mkAdult(t, ctx, store, "caleb@adults.test", "admin")
	jo := mkAdult(t, ctx, store, "jo@adults.test", "user")

	old, changed, _, err := store.SetAdultRole(ctx, caleb, jo, "admin")
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, "user", old.Role, "the old row is returned for the audit line")
	assert.Equal(t, "admin", roleOf(t, ctx, store, jo))

	old, changed, _, err = store.SetAdultRole(ctx, caleb, jo, "admin")
	require.NoError(t, err)
	assert.False(t, changed, "setting the role it already has changes nothing")
	assert.Equal(t, "admin", old.Role)

	_, changed, _, err = store.SetAdultRole(ctx, caleb, jo, "user")
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, "user", roleOf(t, ctx, store, jo))

	for _, bad := range []string{"kid", "", "Admin", "owner"} {
		_, _, _, err = store.SetAdultRole(ctx, caleb, jo, bad)
		assert.True(t, errors.Is(err, storage.ErrBadRole), "SetAdultRole(%q) = %v", bad, err)
	}
	assert.Equal(t, "user", roleOf(t, ctx, store, jo))
}

// AC-009 — a change that would leave no admin is refused and changes nothing.
// Through the routes this is reachable only from a race (an admin demoted a
// moment before acting), so storage is where it is proven directly.
func TestAdults_LastAdminIsRefused(t *testing.T) {
	ctx, store := adultsStore(t, "homepad_adults_last_admin_test")
	caleb := mkAdult(t, ctx, store, "caleb@adults.test", "admin")
	alex := mkAdult(t, ctx, store, "alex@adults.test", "admin")

	_, _, _, err := store.SetAdultRole(ctx, caleb, alex, "user")
	require.NoError(t, err)
	// Alex, a member now, still holds a request that passed its admin check.
	_, _, _, err = store.SetAdultRole(ctx, alex, caleb, "user")
	assert.True(t, errors.Is(err, storage.ErrLastAdmin), "demoting the last admin = %v", err)
	_, _, _, err = store.RemoveAdult(ctx, alex, caleb)
	assert.True(t, errors.Is(err, storage.ErrLastAdmin), "removing the last admin = %v", err)
	assert.Equal(t, "admin", roleOf(t, ctx, store, caleb), "nothing changed")
	assert.Equal(t, 1, countAdmins(t, ctx, store))
}

// §2.3 — the actor is checked again inside the lock: an admin demoted a moment
// ago can't act on a third account, even when admins would remain.
func TestAdults_ActorMustStillBeAdminInsideTheLock(t *testing.T) {
	ctx, store := adultsStore(t, "homepad_adults_actor_test")
	caleb := mkAdult(t, ctx, store, "caleb@adults.test", "admin")
	alex := mkAdult(t, ctx, store, "alex@adults.test", "admin")
	jo := mkAdult(t, ctx, store, "jo@adults.test", "user")

	_, _, _, err := store.SetAdultRole(ctx, caleb, alex, "user")
	require.NoError(t, err)
	_, _, _, err = store.RemoveAdult(ctx, alex, jo)
	assert.True(t, errors.Is(err, storage.ErrNotAdmin), "a demoted actor's removal = %v", err)
	_, _, _, err = store.SetAdultRole(ctx, alex, jo, "admin")
	assert.True(t, errors.Is(err, storage.ErrNotAdmin), "a demoted actor's promotion = %v", err)
	assert.Equal(t, "user", roleOf(t, ctx, store, jo))
	_, err = store.UserByID(ctx, jo)
	assert.NoError(t, err, "Jo is still there")
}

// AC-009 / TC-004 — two admins demote each other at the same moment: exactly
// one wins, the other gets ErrLastAdmin, and one admin remains. Repeated, so
// the lock is what decides it, not the scheduling of one lucky run.
func TestAdults_OppositeDemotionsRace(t *testing.T) {
	ctx, store := adultsStore(t, "homepad_adults_race_test")
	a := mkAdult(t, ctx, store, "a@adults.test", "admin")
	b := mkAdult(t, ctx, store, "b@adults.test", "admin")

	for round := 0; round < 20; round++ {
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, 2)
		for i, pair := range [][2]string{{a, b}, {b, a}} {
			wg.Add(1)
			go func(i int, actor, target string) {
				defer wg.Done()
				<-start
				if round%2 == 0 {
					_, _, _, errs[i] = store.SetAdultRole(ctx, actor, target, "user")
				} else {
					_, _, _, errs[i] = store.RemoveAdult(ctx, actor, target)
				}
			}(i, pair[0], pair[1])
		}
		close(start)
		wg.Wait()

		ok, lost := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, storage.ErrLastAdmin):
				lost++
			default:
				t.Fatalf("round %d: unexpected error %v", round, err)
			}
		}
		require.Equal(t, 1, ok, "round %d: exactly one succeeds", round)
		require.Equal(t, 1, lost, "round %d: the other is refused as last_admin", round)
		require.Equal(t, 1, countAdmins(t, ctx, store), "round %d: one admin remains", round)

		// Reset: put both back as admins for the next round.
		if round%2 == 1 {
			var n int
			require.NoError(t, store.Pool().QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n))
			if n == 1 {
				var survivor string
				require.NoError(t, store.Pool().QueryRow(ctx, `SELECT id::text FROM users`).Scan(&survivor))
				other := mkAdult(t, ctx, store, fmt.Sprintf("r%d@adults.test", round), "admin")
				a, b = survivor, other
			}
		}
		_, err := store.Pool().Exec(ctx, `UPDATE users SET role = 'admin' WHERE id = ANY($1::uuid[])`, []string{a, b})
		require.NoError(t, err)
	}
}

// PR #107 review — removing an admin, or demoting one, revokes the live reset
// links they made, in the same transaction, and reports how many. Links made
// by someone else, and used ones, are untouched; a promotion revokes nothing.
func TestAdults_RevokeTheResetLinksTheyMade(t *testing.T) {
	ctx, store := adultsStore(t, "homepad_adults_resets_test")
	caleb := mkAdult(t, ctx, store, "caleb@adults.test", "admin")
	alex := mkAdult(t, ctx, store, "alex@adults.test", "admin")
	sam := mkAdult(t, ctx, store, "sam@adults.test", "admin")
	jo := mkAdult(t, ctx, store, "jo@adults.test", "user")
	lee := mkAdult(t, ctx, store, "lee@adults.test", "user")

	link := func(target, by string) storage.PasswordReset {
		t.Helper()
		l, _, err := store.CreatePasswordReset(ctx, resetHash(t), target, by, time.Hour)
		require.NoError(t, err)
		return l
	}
	live := func(id string) bool {
		t.Helper()
		var ok bool
		require.NoError(t, store.Pool().QueryRow(ctx,
			`SELECT used_at IS NULL AND revoked_at IS NULL FROM password_resets WHERE id = $1`, id).Scan(&ok))
		return ok
	}

	byAlex, byCaleb := link(jo, alex), link(lee, caleb)
	_, _, revoked, err := store.SetAdultRole(ctx, caleb, jo, "admin")
	require.NoError(t, err)
	assert.Zero(t, revoked, "a promotion revokes nothing")
	_, _, revoked, err = store.SetAdultRole(ctx, caleb, alex, "user")
	require.NoError(t, err)
	assert.Equal(t, int64(1), revoked)
	assert.False(t, live(byAlex.ID), "the demoted admin's link is revoked")
	assert.True(t, live(byCaleb.ID), "another admin's link stays live")

	bySam := link(lee, sam) // revokes Caleb's earlier link for Lee (Q6)
	_, _, revoked, err = store.RemoveAdult(ctx, caleb, sam)
	require.NoError(t, err)
	assert.Equal(t, int64(1), revoked)
	assert.False(t, live(bySam.ID), "the removed admin's link is revoked")
	var createdBy *string
	require.NoError(t, store.Pool().QueryRow(ctx, `SELECT created_by::text FROM password_resets WHERE id = $1`, bySam.ID).Scan(&createdBy))
	assert.Nil(t, createdBy, "the row stays for the audit trail, created_by SET NULL")
}
