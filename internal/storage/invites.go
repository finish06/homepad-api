package storage

import (
	"context"
	"errors"
	"time"
)

// SPEC-household-basics §6 — invites: one-time links an admin makes to add an
// adult (migration 0019).

// errNotImplemented marks the RED skeleton; GREEN replaces every use.
var errNotImplemented = errors.New("storage: not implemented")

// ErrAccountLimit is returned when creating an account or an invite would
// pass HOMEPAD_MAX_ACCOUNTS.
var ErrAccountLimit = errors.New("storage: account limit reached")

// Invite is one invite row, as the admin list and the lookup see it.
type Invite struct {
	ID            string
	Name          string
	Role          string
	CreatedByID   string
	CreatedByName string
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

// CreateInvite stores a new invite by its token hash.
func (s *Store) CreateInvite(ctx context.Context, tokenHash []byte, name, role, createdBy string, ttl time.Duration, limit int) (Invite, error) {
	return Invite{}, errNotImplemented
}

// RevokeInvite revokes a pending invite.
func (s *Store) RevokeInvite(ctx context.Context, id string) (Invite, error) {
	return Invite{}, errNotImplemented
}

// DeleteStaleInvites deletes invites expired, used or revoked more than
// retain ago.
func (s *Store) DeleteStaleInvites(ctx context.Context, retain time.Duration) (int64, error) {
	return 0, errNotImplemented
}

// CountAccounts counts every account plus pending invites.
func (s *Store) CountAccounts(ctx context.Context) (int, error) {
	return 0, errNotImplemented
}

// CreateKidWithLimit is CreateKid under the account cap.
func (s *Store) CreateKidWithLimit(ctx context.Context, name, avatarColor string, limit int) (User, error) {
	return User{}, errNotImplemented
}

// NonOwnerAdminCatalogRows counts catalog rows owned by an admin who is not
// the shared catalog owner.
func (s *Store) NonOwnerAdminCatalogRows(ctx context.Context) (services, categories int, err error) {
	return 0, 0, errNotImplemented
}
