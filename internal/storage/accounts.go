package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// SPEC-household-basics — creating accounts safely: one lock serializes every
// account insert (register, OIDC auto-create, kid create, invite create and
// accept), which makes the first-admin bootstrap atomic (AC-003) and the
// HOMEPAD_MAX_ACCOUNTS cap exact (AC-028).

// ErrAccountLimit is returned when creating an account or an invite would
// pass HOMEPAD_MAX_ACCOUNTS.
var ErrAccountLimit = errors.New("storage: account limit reached")

// ErrRegistrationClosed is returned by RegisterUser when adults already exist
// and self-registration is not open.
var ErrRegistrationClosed = errors.New("storage: registration is invite-only")

// accountsLockKey is the transaction-scoped advisory lock every account
// insert takes. Distinct from migrateLockKey.
const accountsLockKey = 0x686f6d6570616432 // "homepad2"

// rowQuerier is what the insert helpers need: the pool or a transaction.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// withAccountLock runs fn in a transaction that holds the account lock, and
// commits only when fn succeeds.
func (s *Store) withAccountLock(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(accountsLockKey)); err != nil {
		return fmt.Errorf("storage: account lock: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// countAccountsSQL is every account (admin, member, kid) plus every pending,
// unexpired invite: an invite holds its place until it is used, revoked or
// expires (§2.4).
const countAccountsSQL = `SELECT (SELECT count(*) FROM users)
	+ (SELECT count(*) FROM invites WHERE used_at IS NULL AND revoked_at IS NULL AND expires_at > now())`

// CountAccounts is the number HOMEPAD_MAX_ACCOUNTS caps.
func (s *Store) CountAccounts(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, countAccountsSQL).Scan(&n)
	return n, err
}

// checkLimit returns ErrAccountLimit when one more account would pass limit.
// A limit of 0 or less is no limit.
func checkLimit(ctx context.Context, q rowQuerier, limit int) error {
	if limit <= 0 {
		return nil
	}
	var n int
	if err := q.QueryRow(ctx, countAccountsSQL).Scan(&n); err != nil {
		return err
	}
	if n >= limit {
		return ErrAccountLimit
	}
	return nil
}

func countAdults(ctx context.Context, q rowQuerier) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM users WHERE role <> 'kid'`).Scan(&n)
	return n, err
}

// insertUser inserts an adult. displayName "" is stored as NULL.
func insertUser(ctx context.Context, q rowQuerier, email, passwordHash, role, displayName string) (User, error) {
	u, err := scanUser(q.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, role, display_name, show_uptime_display)
		 VALUES ($1, $2, $3, NULLIF($4, ''),
		         COALESCE((SELECT show_uptime_display FROM system_settings WHERE id = 1), TRUE))
		 RETURNING `+userColumns,
		email, passwordHash, role, displayName,
	))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return User{}, ErrEmailTaken
	}
	if err != nil {
		return User{}, err
	}
	return u, nil
}

// RegisterUser is POST /api/register, atomically: under the account lock, the
// first adult becomes admin in every mode (AC-002) and is never refused by
// the cap. After that, a closed install returns ErrRegistrationClosed and an
// open one creates a user, within the cap. first reports the bootstrap.
func (s *Store) RegisterUser(ctx context.Context, email, passwordHash string, open bool, limit int) (u User, first bool, err error) {
	err = s.withAccountLock(ctx, func(tx pgx.Tx) error {
		adults, err := countAdults(ctx, tx)
		if err != nil {
			return err
		}
		role := "user"
		if adults == 0 {
			role, first = "admin", true
		} else {
			if !open {
				return ErrRegistrationClosed
			}
			if err := checkLimit(ctx, tx, limit); err != nil {
				return err
			}
		}
		u, err = insertUser(ctx, tx, email, passwordHash, role, "")
		return err
	})
	if err != nil {
		return User{}, false, err
	}
	return u, first, nil
}

// CreateOIDCUser creates the account for a new OIDC identity, under the
// account lock and the cap. The first adult is never refused.
func (s *Store) CreateOIDCUser(ctx context.Context, email, passwordHash, role string, limit int) (User, error) {
	var u User
	err := s.withAccountLock(ctx, func(tx pgx.Tx) error {
		adults, err := countAdults(ctx, tx)
		if err != nil {
			return err
		}
		if adults > 0 {
			if err := checkLimit(ctx, tx, limit); err != nil {
				return err
			}
		}
		u, err = insertUser(ctx, tx, email, passwordHash, role, "")
		return err
	})
	return u, err
}

// NonOwnerAdminCatalogRows counts the catalog rows owned by an admin who is
// not the shared catalog owner: what a second admin "created" before #87,
// which nobody's dashboard shows (AC-026). Rows of plain users (the #224 era)
// are not counted; they were never admin writes.
func (s *Store) NonOwnerAdminCatalogRows(ctx context.Context) (services, categories int, err error) {
	err = s.pool.QueryRow(ctx,
		`WITH owner AS (
		   SELECT id FROM users WHERE role = 'admin' ORDER BY created_at, id LIMIT 1
		 ), others AS (
		   SELECT id FROM users WHERE role = 'admin' AND id <> (SELECT id FROM owner)
		 )
		 SELECT (SELECT count(*) FROM services   WHERE user_id IN (SELECT id FROM others)),
		        (SELECT count(*) FROM categories WHERE user_id IN (SELECT id FROM others))`,
	).Scan(&services, &categories)
	return services, categories, err
}
