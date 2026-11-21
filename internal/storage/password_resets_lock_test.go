package storage_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitea.kube.calebdunn.tech/code/homepad-api/internal/storage"
)

// PR #107 review — CreatePasswordReset locks the account row and then the
// live link; UsePasswordReset must take the same two locks in the same order
// (account first), or a create racing an accept deadlocks (SQLSTATE 40P01)
// and one request gets a 500.

func isDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

// waitForLockWaiter waits until another backend of this database is blocked
// on a row lock.
func waitForLockWaiter(t *testing.T, ctx context.Context, store *storage.Store) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		require.NoError(t, store.Pool().QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity
			  WHERE datname = current_database() AND pid <> pg_backend_pid() AND wait_event_type = 'Lock'`).Scan(&n))
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("accept never waited for the account lock")
}

// Deterministic: the test plays CreatePasswordReset by hand. It locks the
// account row, lets an accept start and block, then revokes the live link
// as a newer link would. Accept must not be holding the link while it waits
// for the account, so the revoke goes through at once; once the account is
// free, accept re-reads the link, finds it revoked and changes nothing.
func TestUsePasswordReset_LocksTheAccountBeforeTheLink(t *testing.T) {
	store, ctx := openStore(t)
	admin := seedStoreAdmin(t, store, ctx)
	user := seedStoreUser(t, store, ctx)
	h := resetHash(t)
	link, _, err := store.CreatePasswordReset(ctx, h, user, admin, time.Hour)
	require.NoError(t, err)

	tx, err := store.Pool().Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR NO KEY UPDATE`, user)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, _, err := store.UsePasswordReset(ctx, h, "$2a$04$set-by-the-stale-link")
		done <- err
	}()
	waitForLockWaiter(t, ctx, store)

	_, err = tx.Exec(ctx, `UPDATE password_resets SET revoked_at = now() WHERE id = $1`, link.ID)
	require.NoError(t, err, "create's second step must not wait on accept (no 40P01)")
	require.NoError(t, tx.Commit(ctx))

	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("accept never finished")
	}
	assert.False(t, isDeadlock(err), "accept hit a deadlock: %v", err)
	assert.ErrorIs(t, err, storage.ErrNotFound, "accept re-checks the link under the account lock and finds it revoked")

	u, err := store.UserByID(ctx, user)
	require.NoError(t, err)
	assert.Equal(t, "x", u.PasswordHash, "the revoked link set nothing")
}

// The real functions, racing: a new link for an account at the moment its
// live link is accepted. Neither may fail with 40P01; accept either wins
// (the password is set) or finds the link revoked.
func TestPasswordReset_CreateAndAcceptRaceWithoutDeadlock(t *testing.T) {
	store, ctx := openStore(t)
	admin := seedStoreAdmin(t, store, ctx)
	user := seedStoreUser(t, store, ctx)

	for round := 0; round < 200; round++ {
		h, next := resetHash(t), resetHash(t) // resetHash isn't safe to call from the goroutines
		_, _, err := store.CreatePasswordReset(ctx, h, user, admin, time.Hour)
		require.NoError(t, err)

		var wg sync.WaitGroup
		start := make(chan struct{})
		var createErr, useErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _, createErr = store.CreatePasswordReset(ctx, next, user, admin, time.Hour)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, _, useErr = store.UsePasswordReset(ctx, h, "$2a$04$raced")
		}()
		close(start)
		wg.Wait()

		require.NoError(t, createErr, "round %d: create", round)
		if useErr != nil {
			require.ErrorIs(t, useErr, storage.ErrNotFound, "round %d: accept", round)
		}
	}
}
