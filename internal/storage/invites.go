package storage

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// SPEC-household-basics §6 — invites: one-time links an admin makes to add an
// adult (migration 0019). Only sha256(token) is ever stored; the token itself
// is the caller's to hand out once.

// Invite is one invite row, as the admin list and the lookup see it.
// CreatedByName is the creator's display name, or their email when they have
// none, and "" when the creator's account is gone.
type Invite struct {
	ID            string
	Name          string
	Role          string
	CreatedByID   string
	CreatedByName string
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

// pendingInvite is the one definition of a live invite (§6).
const pendingInvite = `i.used_at IS NULL AND i.revoked_at IS NULL AND i.expires_at > now()`

// inviteSelect reads an Invite from "invites i" joined to its creator.
const inviteSelect = `SELECT i.id, i.name, i.role, COALESCE(i.created_by::text, ''),
	COALESCE(NULLIF(cb.display_name, ''), cb.email, ''), i.created_at, i.expires_at
	FROM invites i LEFT JOIN users cb ON cb.id = i.created_by`

func scanInvite(row pgx.Row) (Invite, error) {
	var inv Invite
	err := row.Scan(&inv.ID, &inv.Name, &inv.Role, &inv.CreatedByID, &inv.CreatedByName, &inv.CreatedAt, &inv.ExpiresAt)
	return inv, err
}

// CreateInvite stores an invite by its token hash, made by createdBy, ending
// ttl from now. Under the account lock it counts against limit like an
// account (ErrAccountLimit), since it reserves a place (§2.4). The caller
// validates name and role; the table CHECK backs the role up.
func (s *Store) CreateInvite(ctx context.Context, tokenHash []byte, name, role, createdBy string, ttl time.Duration, limit int) (Invite, error) {
	var inv Invite
	err := s.withAccountLock(ctx, func(tx pgx.Tx) error {
		if err := checkLimit(ctx, tx, limit); err != nil {
			return err
		}
		var id string
		if err := tx.QueryRow(ctx,
			`INSERT INTO invites (token_hash, name, role, created_by, expires_at)
			 VALUES ($1, $2, $3, $4, now() + make_interval(secs => $5))
			 RETURNING id`,
			tokenHash, name, role, createdBy, ttl.Seconds(),
		).Scan(&id); err != nil {
			return err
		}
		var err error
		inv, err = scanInvite(tx.QueryRow(ctx, inviteSelect+` WHERE i.id = $1`, id))
		return err
	})
	return inv, err
}

// PendingInvites lists the live invites, newest first (AC-011).
func (s *Store) PendingInvites(ctx context.Context) ([]Invite, error) {
	rows, err := s.pool.Query(ctx, inviteSelect+` WHERE `+pendingInvite+` ORDER BY i.created_at DESC, i.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Invite, 0)
	for rows.Next() {
		inv, err := scanInvite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// RevokeInvite revokes a pending invite. ErrNotFound when id names no pending
// invite: unknown, malformed, already used, already revoked or expired.
func (s *Store) RevokeInvite(ctx context.Context, id string) (Invite, error) {
	var inv Invite
	err := s.pool.QueryRow(ctx,
		`UPDATE invites i SET revoked_at = now() WHERE i.id = $1 AND `+pendingInvite+`
		 RETURNING i.id, i.name, i.role`, id,
	).Scan(&inv.ID, &inv.Name, &inv.Role)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
		return Invite{}, ErrNotFound
	}
	return inv, err
}

// LookupInvite returns the pending invite with tokenHash, or ErrNotFound for
// every other case, so the caller can answer every dead link the same way.
func (s *Store) LookupInvite(ctx context.Context, tokenHash []byte) (Invite, error) {
	inv, err := scanInvite(s.pool.QueryRow(ctx, inviteSelect+` WHERE i.token_hash = $1 AND `+pendingInvite, tokenHash))
	if errors.Is(err, pgx.ErrNoRows) {
		return Invite{}, ErrNotFound
	}
	return inv, err
}

// AcceptInvite uses the pending invite with tokenHash: in one transaction it
// creates the adult with the invite's role, and marks the invite used by
// them (AC-010). name "" takes the invite's name. ErrNotFound when the invite
// isn't pending (including a racing accept that won first); ErrEmailTaken
// when the email has an account, in which case nothing changes and the
// invite stays usable (AC-014). Accepting is never refused by the account
// cap: the invite already holds its place.
func (s *Store) AcceptInvite(ctx context.Context, tokenHash []byte, email, passwordHash, name string) (User, Invite, error) {
	var u User
	var inv Invite
	err := s.withAccountLock(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`SELECT i.id, i.name, i.role FROM invites i WHERE i.token_hash = $1 AND `+pendingInvite+` FOR UPDATE`,
			tokenHash,
		).Scan(&inv.ID, &inv.Name, &inv.Role)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if name == "" {
			name = inv.Name
		}
		u, err = insertUser(ctx, tx, email, passwordHash, inv.Role, name)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE invites SET used_at = now(), used_by = $2 WHERE id = $1`, inv.ID, u.ID)
		return err
	})
	if err != nil {
		return User{}, Invite{}, err
	}
	return u, inv, nil
}

// DeleteStaleInvites deletes invites that expired, were used or were revoked
// more than retain ago (§3.3: kept 30 days for the audit trail), and returns
// how many.
func (s *Store) DeleteStaleInvites(ctx context.Context, retain time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM invites
		  WHERE expires_at < now() - make_interval(secs => $1)
		     OR used_at    < now() - make_interval(secs => $1)
		     OR revoked_at < now() - make_interval(secs => $1)`, retain.Seconds())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
