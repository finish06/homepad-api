package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// SPEC-kid-accounts — kid accounts and their allow-lists.

// ErrKidNameTaken is returned by CreateKid / UpdateKid when another kid already
// has the name (compared case-insensitively, UX §3).
var ErrKidNameTaken = errors.New("storage: a kid with that name already exists")

// ErrUnknownService is returned by SetKidServices when an id names no service
// in the shared catalog.
var ErrUnknownService = errors.New("storage: unknown service id")

// RoleKid is the kid role (migration 0018).
const RoleKid = "kid"

// IsAdultRole reports whether role is one of the two adult roles. Everything
// else — the kid role and any role this code does not know — is treated as a
// kid by the filters and the write lock (fail closed).
func IsAdultRole(role string) bool {
	return role == "admin" || role == "user"
}

// ServiceSet is the set of services one account may see. A nil *ServiceSet
// means "every service": that is what VisibleServiceIDs returns for adults. A
// non-nil set, even an empty one, allows exactly its members.
type ServiceSet struct {
	ids map[string]struct{}
}

// Allows reports whether id is visible. A nil set allows everything.
func (v *ServiceSet) Allows(id string) bool {
	if v == nil {
		return true
	}
	_, ok := v.ids[id]
	return ok
}

// Len is the number of ids in a non-nil set. It is 0 for nil too — but nil
// means "all", so check for nil before reading Len as a count.
func (v *ServiceSet) Len() int {
	if v == nil {
		return 0
	}
	return len(v.ids)
}

// VisibleServiceIDs is the single choke point for kid filtering (spec §7): it
// returns nil (everything) for an admin or a user, and the allow-list for a
// kid. Any role that is not exactly admin or user takes the allow-list path, so
// an unknown role sees what its (normally empty) allow-list holds, never the
// whole catalog. Every handler that returns catalog data filters through this.
// An error is an error: callers must fail the request, never fall back to nil.
func (s *Store) VisibleServiceIDs(ctx context.Context, u User) (*ServiceSet, error) {
	if IsAdultRole(u.Role) {
		return nil, nil
	}
	set := &ServiceSet{ids: map[string]struct{}{}}
	rows, err := s.pool.Query(ctx, `SELECT service_id FROM kid_allowed_services WHERE user_id = $1`, u.ID)
	if err != nil {
		return nil, fmt.Errorf("storage: visible services: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("storage: visible services: %w", err)
		}
		set.ids[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: visible services: %w", err)
	}
	return set, nil
}

// CreateKid creates a kid account with only a name and an avatar color (AC-001).
// It has no email and no password. Per OQ-3 and UX K-3 the health bar starts
// off and the density starts at large; the uptime display follows the system
// default, as it does for every new account. The caller validates the name and
// color; the schema backs both up.
func (s *Store) CreateKid(ctx context.Context, name, avatarColor string) (User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, role, display_name, avatar_color,
		                    show_health_bar, density_pref, show_uptime_display)
		 VALUES (NULL, NULL, 'kid', $1, $2, FALSE, 'large',
		         COALESCE((SELECT show_uptime_display FROM system_settings WHERE id = 1), TRUE))
		 RETURNING `+userColumns,
		name, avatarColor))
	if isUniqueViolation(err, "users_kid_name_key") {
		return User{}, ErrKidNameTaken
	}
	if err != nil {
		return User{}, err
	}
	return u, nil
}

// KidByID returns the kid with id, or ErrNotFound when id names no kid — an
// adult's id, an unknown id and a malformed one all read the same.
func (s *Store) KidByID(ctx context.Context, id string) (User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users WHERE id = $1 AND role = 'kid'`, id))
	if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	return u, nil
}

// UpdateKid changes a kid's name and/or avatar color (nil = unchanged) and
// returns the updated kid. ErrNotFound when id names no kid; ErrKidNameTaken
// on a name collision. It can never touch an adult row.
func (s *Store) UpdateKid(ctx context.Context, id string, name, avatarColor *string) (User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx,
		`UPDATE users SET display_name = COALESCE($2, display_name),
		                  avatar_color = COALESCE($3, avatar_color)
		  WHERE id = $1 AND role = 'kid'
		 RETURNING `+userColumns,
		id, name, avatarColor))
	if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
		return User{}, ErrNotFound
	}
	if isUniqueViolation(err, "users_kid_name_key") {
		return User{}, ErrKidNameTaken
	}
	if err != nil {
		return User{}, err
	}
	return u, nil
}

// DeleteKid deletes a kid account. Its sessions, favorites, layout, collapsed
// sections and allow-list go with it by FK cascade (AC-014). ErrNotFound when
// id names no kid: this can never delete an adult.
func (s *Store) DeleteKid(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM users WHERE id = $1 AND role = 'kid'`, id)
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

// sharedOwnerSQL is SharedCatalogOwnerID as a subquery.
const sharedOwnerSQL = `(SELECT id FROM users WHERE role = 'admin' ORDER BY created_at, id LIMIT 1)`

// KidServiceIDs returns the kid's allow-list, restricted to services in the
// shared catalog (what the kid can actually see), ordered by service name.
func (s *Store) KidServiceIDs(ctx context.Context, kidID string) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT k.service_id
		   FROM kid_allowed_services k
		   JOIN services sv ON sv.id = k.service_id
		  WHERE k.user_id = $1
		    AND sv.user_id = `+sharedOwnerSQL+`
		  ORDER BY sv.name, sv.id`, kidID)
	if isInvalidText(err) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SetKidServices replaces the kid's allow-list with serviceIDs (whole-set
// replace, like SetLayout). Every id must name a service in the shared catalog,
// else ErrUnknownService and nothing changes. Duplicates collapse. ErrNotFound
// when kidID names no kid, so an adult can never be given an allow-list.
func (s *Store) SetKidServices(ctx context.Context, kidID string, serviceIDs []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Lock the kid row so a concurrent delete cannot interleave.
	var one int
	err = tx.QueryRow(ctx, `SELECT 1 FROM users WHERE id = $1 AND role = 'kid' FOR UPDATE`, kidID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}

	uniq := make([]string, 0, len(serviceIDs))
	seen := make(map[string]bool, len(serviceIDs))
	for _, id := range serviceIDs {
		if !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	// Compare as text so a malformed id counts as unknown instead of aborting
	// the transaction on a cast error.
	var known int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM services
		  WHERE id::text = ANY($1::text[]) AND user_id = `+sharedOwnerSQL,
		uniq).Scan(&known); err != nil {
		return err
	}
	if known != len(uniq) {
		return ErrUnknownService
	}

	if _, err := tx.Exec(ctx, `DELETE FROM kid_allowed_services WHERE user_id = $1`, kidID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO kid_allowed_services (user_id, service_id)
		 SELECT $1::uuid, sv.id FROM services sv WHERE sv.id::text = ANY($2::text[])`,
		kidID, uniq); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// isInvalidText reports a Postgres "invalid input syntax" error: an id that is
// not a UUID, which can name no row.
func isInvalidText(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}
