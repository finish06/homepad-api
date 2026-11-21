package storage

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// SPEC-password-reset-link §6 — one-time password reset links (migration
// 0022). Only sha256(token) is ever stored; the token itself is the caller's
// to hand out once.

// ErrSSOAccount is returned when an account has no local password to reset:
// it signs in with single sign-on only (Q4).
var ErrSSOAccount = errors.New("storage: account signs in with single sign-on only")

// noLocalPassword is what an account made by single sign-on stores in
// password_hash (api's oidcNoPassword; a test in package api pins the two
// together). It is not a bcrypt hash, so password sign-in always fails.
const noLocalPassword = "oidc:no-local-password"

// HasLocalPassword reports whether u is an adult who signs in with a
// password: GET /api/me's and the Accounts list's hasPassword (AC-020,
// AC-021). Kids and accounts that only use single sign-on have none.
func (u User) HasLocalPassword() bool {
	return IsAdultRole(u.Role) && u.PasswordHash != "" && u.PasswordHash != noLocalPassword
}

// hasLocalPasswordSQL is HasLocalPassword for "users u".
const hasLocalPasswordSQL = `u.role IN ('admin', 'user') AND u.password_hash IS NOT NULL AND u.password_hash <> '` + noLocalPassword + `'`

// liveReset is the one definition of a live link (§6).
const liveReset = `p.used_at IS NULL AND p.revoked_at IS NULL AND p.expires_at > now()`

// PasswordReset is one reset link, with the account it is for.
type PasswordReset struct {
	ID        string
	UserID    string
	Email     string
	Name      string // the display name, or the email when there is none
	ExpiresAt time.Time
}

// CreatePasswordReset stores a link for userID by its token hash, made by
// createdBy ("" for the CLI), ending ttl from now. In the same transaction it
// revokes the account's earlier live links (Q6) and returns their ids. The
// account row is locked first, so racing creates leave one live link.
// ErrNotFound when userID is not an adult (unknown, malformed, a kid);
// ErrSSOAccount when the adult has no local password.
func (s *Store) CreatePasswordReset(ctx context.Context, tokenHash []byte, userID, createdBy string, ttl time.Duration) (PasswordReset, []string, error) {
	var link PasswordReset
	var revoked []string
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return link, nil, err
	}
	defer tx.Rollback(ctx)

	u, err := scanUser(tx.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID))
	switch {
	case errors.Is(err, pgx.ErrNoRows) || isInvalidText(err):
		return link, nil, ErrNotFound
	case err != nil:
		return link, nil, err
	case !IsAdultRole(u.Role):
		return link, nil, ErrNotFound
	case !u.HasLocalPassword():
		return link, nil, ErrSSOAccount
	}

	rows, err := tx.Query(ctx,
		`UPDATE password_resets p SET revoked_at = now() WHERE p.user_id = $1 AND `+liveReset+` RETURNING p.id::text`, userID)
	if err != nil {
		return link, nil, err
	}
	if revoked, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return link, nil, err
	}

	if err := tx.QueryRow(ctx,
		`INSERT INTO password_resets (token_hash, user_id, created_by, expires_at)
		 VALUES ($1, $2, NULLIF($3, '')::uuid, now() + make_interval(secs => $4))
		 RETURNING id::text, expires_at`,
		tokenHash, userID, createdBy, ttl.Seconds(),
	).Scan(&link.ID, &link.ExpiresAt); err != nil {
		return PasswordReset{}, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PasswordReset{}, nil, err
	}
	link.UserID, link.Email, link.Name = u.ID, u.Email, displayNameOrEmail(u)
	return link, revoked, nil
}

func displayNameOrEmail(u User) string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Email
}

// LookupPasswordReset returns the live link with tokenHash, for an account
// that still has a local password, or ErrNotFound for every other case, so
// the caller can answer every dead link the same way (AC-006).
func (s *Store) LookupPasswordReset(ctx context.Context, tokenHash []byte) (PasswordReset, error) {
	var link PasswordReset
	err := s.pool.QueryRow(ctx,
		`SELECT p.id::text, u.id::text, u.email, COALESCE(NULLIF(u.display_name, ''), u.email), p.expires_at
		   FROM password_resets p JOIN users u ON u.id = p.user_id
		  WHERE p.token_hash = $1 AND `+liveReset+` AND `+hasLocalPasswordSQL,
		tokenHash,
	).Scan(&link.ID, &link.UserID, &link.Email, &link.Name, &link.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PasswordReset{}, ErrNotFound
	}
	return link, err
}

// UsePasswordReset uses the live link with tokenHash: in one transaction it
// sets the account's password hash and marks the link used (§3.1). Both rows
// are locked, so racing accepts use it once. ErrNotFound when the link isn't
// live or its account no longer has a local password; nothing changes then.
func (s *Store) UsePasswordReset(ctx context.Context, tokenHash []byte, passwordHash string) (User, PasswordReset, error) {
	var link PasswordReset
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, link, err
	}
	defer tx.Rollback(ctx)

	err = tx.QueryRow(ctx,
		`SELECT p.id::text, p.user_id::text, p.expires_at FROM password_resets p
		  WHERE p.token_hash = $1 AND `+liveReset+` FOR UPDATE`, tokenHash,
	).Scan(&link.ID, &link.UserID, &link.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, PasswordReset{}, ErrNotFound
	}
	if err != nil {
		return User{}, PasswordReset{}, err
	}
	u, err := scanUser(tx.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1 FOR NO KEY UPDATE`, link.UserID))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, PasswordReset{}, ErrNotFound
	}
	if err != nil {
		return User{}, PasswordReset{}, err
	}
	if !u.HasLocalPassword() {
		return User{}, PasswordReset{}, ErrNotFound
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1`, u.ID, passwordHash); err != nil {
		return User{}, PasswordReset{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE password_resets SET used_at = now() WHERE id = $1`, link.ID); err != nil {
		return User{}, PasswordReset{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, PasswordReset{}, err
	}
	u.PasswordHash = passwordHash
	link.Email, link.Name = u.Email, displayNameOrEmail(u)
	return u, link, nil
}

// SetPasswordHash replaces an adult's password hash (Change password,
// AC-013). ErrNotFound when userID names no adult.
func (s *Store) SetPasswordHash(ctx context.Context, userID, passwordHash string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET password_hash = $2 WHERE id = $1 AND role IN ('admin', 'user')`, userID, passwordHash)
	if isInvalidText(err) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteStalePasswordResets deletes links that expired, were used or were
// revoked more than retain ago (§6: kept 30 days for the audit trail), and
// returns how many.
func (s *Store) DeleteStalePasswordResets(ctx context.Context, retain time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM password_resets
		  WHERE expires_at < now() - make_interval(secs => $1)
		     OR used_at    < now() - make_interval(secs => $1)
		     OR revoked_at < now() - make_interval(secs => $1)`, retain.Seconds())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
