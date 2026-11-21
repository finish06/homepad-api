package storage

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// SPEC-household-manage-adults §2.3, §6 — an admin removes an adult or
// changes an adult's role. Both run in one transaction under the account lock
// (accountsLockKey), so they serialize with each other and with every account
// insert: the admin count read inside the lock is the count the change
// commits against, and two admins demoting each other at once can't both win.

// ErrSelf is returned when an admin acts on their own account (Q1).
var ErrSelf = errors.New("storage: an admin can't remove or change their own account")

// ErrLastAdmin is returned when the change would leave no admin.
var ErrLastAdmin = errors.New("storage: the change would leave no admin")

// ErrNotAdmin is returned when the actor is no longer an admin by the time
// the change holds the lock (another admin demoted them a moment before).
var ErrNotAdmin = errors.New("storage: the actor is not an admin")

// ErrBadRole is returned for a role other than "admin" or "user".
var ErrBadRole = errors.New("storage: role must be admin or user")

// lockedAdult reads an adult under the account lock, or ErrNotFound when id
// names no adult: a kid's id, an unknown id and a malformed one read the same.
func lockedAdult(ctx context.Context, tx pgx.Tx, id string) (User, error) {
	u, err := scanUser(tx.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users WHERE id = $1 AND role IN ('admin', 'user') FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
		return User{}, ErrNotFound
	}
	return u, err
}

// lockedTarget reads the target adult under the lock and refuses the
// actor's own account (ErrSelf).
func lockedTarget(ctx context.Context, tx pgx.Tx, actorID, targetID string) (User, error) {
	target, err := lockedAdult(ctx, tx, targetID)
	if err != nil {
		return User{}, err
	}
	if target.ID == actorID {
		return User{}, ErrSelf
	}
	return target, nil
}

// checkStillAdmin applies the two rules read from the whole table, inside the
// lock: the change must leave an admin (ErrLastAdmin), and the actor must
// still be one (ErrNotAdmin). losesAdmin says whether the change takes an
// admin away. The last-admin check comes first, so two admins demoting each
// other at once see last_admin (TC-004).
func checkStillAdmin(ctx context.Context, tx pgx.Tx, actorID string, losesAdmin bool) error {
	var admins int
	var actorIsAdmin bool
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE role = 'admin'),
		        COALESCE(bool_or(id::text = $1 AND role = 'admin'), FALSE)
		   FROM users`, actorID).Scan(&admins, &actorIsAdmin); err != nil {
		return err
	}
	if losesAdmin && admins <= 1 {
		return ErrLastAdmin
	}
	if !actorIsAdmin {
		return ErrNotAdmin
	}
	return nil
}

// RemoveAdult deletes the adult targetID on behalf of the admin actorID and
// returns the removed row and how many of their pending invites it revoked
// (Q3). Their sessions, favorites, order, folded sections and preferences go
// by FK cascade; invites they made or used keep their rows, with the id SET
// NULL. The catalog has no owner, so nothing in it changes
// (SPEC-household-catalog-owner). Errors: ErrNotFound, ErrSelf,
// ErrLastAdmin, ErrNotAdmin.
func (s *Store) RemoveAdult(ctx context.Context, actorID, targetID string) (removed User, revokedInvites int64, err error) {
	err = s.withAccountLock(ctx, func(tx pgx.Tx) error {
		target, err := lockedTarget(ctx, tx, actorID, targetID)
		if err != nil {
			return err
		}
		if err := checkStillAdmin(ctx, tx, actorID, target.Role == "admin"); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx,
			`UPDATE invites i SET revoked_at = now() WHERE i.created_by = $1 AND `+pendingInvite, target.ID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, target.ID); err != nil {
			return err
		}
		removed, revokedInvites = target, tag.RowsAffected()
		return nil
	})
	if err != nil {
		return User{}, 0, err
	}
	return removed, revokedInvites, nil
}

// SetAdultRole sets the adult targetID's role to "admin" or "user" on behalf
// of the admin actorID. It returns the row as it was before, and whether the
// role changed: setting the role an adult already has changes nothing.
// Errors: ErrBadRole, ErrNotFound, ErrSelf, ErrLastAdmin, ErrNotAdmin. The
// caller ends the target's sessions when changed is true (Q2).
func (s *Store) SetAdultRole(ctx context.Context, actorID, targetID, role string) (old User, changed bool, err error) {
	if role != "admin" && role != "user" {
		return User{}, false, ErrBadRole
	}
	err = s.withAccountLock(ctx, func(tx pgx.Tx) error {
		target, err := lockedTarget(ctx, tx, actorID, targetID)
		if err != nil {
			return err
		}
		if target.Role == role {
			old = target
			return nil // a no-op writes nothing
		}
		if err := checkStillAdmin(ctx, tx, actorID, target.Role == "admin"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET role = $2 WHERE id = $1`, target.ID, role); err != nil {
			return err
		}
		old, changed = target, true
		return nil
	})
	if err != nil {
		return User{}, false, err
	}
	return old, changed, nil
}
