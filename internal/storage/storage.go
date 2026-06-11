package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitea.kube.calebdunn.tech/code/homepad-api/migrations"
)

// ErrNotFound is returned by lookups when no matching row exists.
var ErrNotFound = errors.New("storage: not found")

// ErrEmailTaken is returned by CreateUser when the email is already registered.
var ErrEmailTaken = errors.New("storage: email already registered")

// ErrSlugTaken is returned by CreateService when the slug is already in use.
var ErrSlugTaken = errors.New("storage: service slug already in use")

type Store struct {
	DSN  string
	pool *pgxpool.Pool
}

type User struct {
	ID           string
	Email        string
	PasswordHash string
	Role         string
	ThemePref    string
}

// Service is a catalog entry. GatusKey is empty when the service is unmonitored.
type Service struct {
	ID          string
	Slug        string
	Name        string
	Description string
	URL         string
	Icon        string
	GatusKey    string
}

func Open(ctx context.Context, dsn string) (*Store, error) {
	if dsn == "" {
		return nil, errors.New("storage.Open: DATABASE_URL is empty")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("storage.Open: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("storage.Open: ping: %w", err)
	}
	return &Store{DSN: dsn, pool: pool}, nil
}

// migrateLockKey is an arbitrary fixed key for the advisory lock that
// serializes Migrate. It guards against concurrent migrators racing on
// CREATE EXTENSION (which is not concurrency-safe) — e.g. parallel test
// binaries sharing a DB, or multiple replicas migrating on boot.
const migrateLockKey = 0x686f6d6570616431 // "homepad1"

// Migrate applies every embedded *.up.sql migration in lexical order, inside a
// single transaction guarded by a session advisory lock so concurrent
// migrators serialize. The migrations use IF NOT EXISTS, so re-running is a
// no-op.
func (s *Store) Migrate(ctx context.Context) error {
	names, err := fs.Glob(migrations.FS, "*.up.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(migrateLockKey)); err != nil {
		return fmt.Errorf("storage.Migrate: acquire lock: %w", err)
	}
	for _, name := range names {
		sqlBytes, err := migrations.FS.ReadFile(name)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
			return fmt.Errorf("storage.Migrate: %s: %w", name, err)
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func (s *Store) Close() error {
	if s.pool != nil {
		s.pool.Close()
	}
	return nil
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Store) CreateUser(ctx context.Context, email, passwordHash, role string) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, role) VALUES ($1, $2, $3)
		 RETURNING id, email, password_hash, role, theme_pref`,
		email, passwordHash, role,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Role, &u.ThemePref)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return User{}, ErrEmailTaken
	}
	if err != nil {
		return User{}, err
	}
	return u, nil
}

func (s *Store) UserByEmail(ctx context.Context, email string) (User, error) {
	return s.userBy(ctx, `WHERE email = $1`, email)
}

func (s *Store) UserByID(ctx context.Context, id string) (User, error) {
	return s.userBy(ctx, `WHERE id = $1`, id)
}

// SetThemePref updates a single user's theme preference (v3). The handler
// validates pref against the allowed set first; the column CHECK is a backstop.
// Writes only userID's row. Returns ErrNotFound when userID names no user.
func (s *Store) SetThemePref(ctx context.Context, userID, pref string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET theme_pref = $2 WHERE id = $1`, userID, pref)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListServices returns the shared catalog in userID's personal layout order
// (A5): services the user has placed come first by their saved sort_index, and
// any not yet placed fall back to name order after them.
func (s *Store) ListServices(ctx context.Context, userID string) ([]Service, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT s.id, s.slug, s.name, s.description, s.url, s.icon, COALESCE(s.gatus_key, '')
		   FROM services s
		   LEFT JOIN user_layout ul ON ul.service_id = s.id AND ul.user_id = $1
		  ORDER BY ul.sort_index NULLS LAST, s.name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Service
	for rows.Next() {
		var sv Service
		if err := rows.Scan(&sv.ID, &sv.Slug, &sv.Name, &sv.Description, &sv.URL, &sv.Icon, &sv.GatusKey); err != nil {
			return nil, err
		}
		out = append(out, sv)
	}
	return out, rows.Err()
}

// CreateService inserts a catalog entry. An empty GatusKey is stored as NULL.
func (s *Store) CreateService(ctx context.Context, in Service) (Service, error) {
	var gatusKey *string
	if in.GatusKey != "" {
		gatusKey = &in.GatusKey
	}
	var sv Service
	err := s.pool.QueryRow(ctx,
		`INSERT INTO services (slug, name, description, url, icon, gatus_key)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, slug, name, description, url, icon, COALESCE(gatus_key, '')`,
		in.Slug, in.Name, in.Description, in.URL, in.Icon, gatusKey,
	).Scan(&sv.ID, &sv.Slug, &sv.Name, &sv.Description, &sv.URL, &sv.Icon, &sv.GatusKey)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Service{}, ErrSlugTaken
	}
	if err != nil {
		return Service{}, err
	}
	return sv, nil
}

// ServiceUpdate is a partial patch of a catalog entry. A nil field is left
// unchanged. GatusKey follows intent: nil keeps the current value; a non-nil
// pointer to "" clears it (unmonitors the service).
type ServiceUpdate struct {
	Slug        *string
	Name        *string
	Description *string
	URL         *string
	Icon        *string
	GatusKey    *string
}

// UpdateService applies a partial patch and returns the updated row. Returns
// ErrNotFound when id names no service (including a malformed UUID) and
// ErrSlugTaken when the new slug collides with another entry.
func (s *Store) UpdateService(ctx context.Context, id string, in ServiceUpdate) (Service, error) {
	setGatus := in.GatusKey != nil
	var gatusKey *string
	if setGatus && *in.GatusKey != "" {
		gatusKey = in.GatusKey
	}

	var sv Service
	err := s.pool.QueryRow(ctx,
		`UPDATE services SET
		   slug        = COALESCE($2, slug),
		   name        = COALESCE($3, name),
		   description = COALESCE($4, description),
		   url         = COALESCE($5, url),
		   icon        = COALESCE($6, icon),
		   gatus_key   = CASE WHEN $7 THEN $8 ELSE gatus_key END,
		   updated_at  = now()
		 WHERE id = $1
		 RETURNING id, slug, name, description, url, icon, COALESCE(gatus_key, '')`,
		id, in.Slug, in.Name, in.Description, in.URL, in.Icon, setGatus, gatusKey,
	).Scan(&sv.ID, &sv.Slug, &sv.Name, &sv.Description, &sv.URL, &sv.Icon, &sv.GatusKey)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if pgErr.Code == "23505" {
			return Service{}, ErrSlugTaken
		}
		if pgErr.Code == "22P02" { // malformed UUID: no such service
			return Service{}, ErrNotFound
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Service{}, ErrNotFound
	}
	if err != nil {
		return Service{}, err
	}
	return sv, nil
}

// DeleteService removes a catalog entry. Returns ErrNotFound when id names no
// service (including a malformed UUID). Favorites and layout rows referencing it
// are cleaned up by ON DELETE CASCADE.
func (s *Store) DeleteService(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM services WHERE id = $1`, id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22P02" {
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

// Icon is an uploaded PNG variant for a service, with the metadata needed to
// serve it conditionally (ETag) without re-hashing.
type Icon struct {
	Bytes []byte
	ETag  string
}

// IconFlags reports, for every service that has at least one uploaded icon,
// which variants exist. Services with no uploads are absent from the map. It
// reads only the (service_id, variant) keys — never the blob bytes — so the
// catalog list query stays cheap (spec A13).
type IconFlags struct {
	Light bool
	Dark  bool
}

// AllIconFlags returns the icon-variant presence map keyed by service id. Used
// to populate iconLight/iconDark on the catalog list without pulling bytes.
func (s *Store) AllIconFlags(ctx context.Context) (map[string]IconFlags, error) {
	rows, err := s.pool.Query(ctx, `SELECT service_id, variant FROM service_icons`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]IconFlags)
	for rows.Next() {
		var id, variant string
		if err := rows.Scan(&id, &variant); err != nil {
			return nil, err
		}
		f := out[id]
		switch variant {
		case "light":
			f.Light = true
		case "dark":
			f.Dark = true
		}
		out[id] = f
	}
	return out, rows.Err()
}

// GetIcon returns the stored PNG bytes and ETag for a service's variant.
// Returns ErrNotFound when that variant has no upload (or id is a malformed
// UUID — the serving handler treats both as 404).
func (s *Store) GetIcon(ctx context.Context, serviceID, variant string) (Icon, error) {
	var ic Icon
	err := s.pool.QueryRow(ctx,
		`SELECT bytes, etag FROM service_icons WHERE service_id = $1 AND variant = $2`,
		serviceID, variant,
	).Scan(&ic.Bytes, &ic.ETag)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22P02" { // malformed UUID
		return Icon{}, ErrNotFound
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Icon{}, ErrNotFound
	}
	if err != nil {
		return Icon{}, err
	}
	return ic, nil
}

// PutIcon upserts a service's icon variant — the same idempotent operation
// handles both first upload and replace (PK is (service_id, variant)). Returns
// ErrNotFound when serviceID does not name a real service (FK violation or
// malformed UUID).
func (s *Store) PutIcon(ctx context.Context, serviceID, variant string, bytes []byte, width, height int, etag string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO service_icons (service_id, variant, bytes, byte_size, width, height, etag, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, now())
		 ON CONFLICT (service_id, variant) DO UPDATE SET
		   bytes = EXCLUDED.bytes, byte_size = EXCLUDED.byte_size,
		   width = EXCLUDED.width, height = EXCLUDED.height,
		   etag = EXCLUDED.etag, updated_at = now()`,
		serviceID, variant, bytes, len(bytes), width, height, etag)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "23503" || pgErr.Code == "22P02") {
		return ErrNotFound
	}
	return err
}

// DeleteIcon removes a service's icon variant. Idempotent: deleting a variant
// that isn't set (or a malformed id) succeeds with no effect (spec A11).
func (s *Store) DeleteIcon(ctx context.Context, serviceID, variant string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM service_icons WHERE service_id = $1 AND variant = $2`,
		serviceID, variant)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22P02" { // malformed UUID: nothing to delete
		return nil
	}
	return err
}

// AddFavorite marks serviceID as a favorite for userID. Idempotent: marking an
// already-favorited service is a no-op. Returns ErrNotFound when serviceID does
// not name a real service (FK violation or malformed UUID).
func (s *Store) AddFavorite(ctx context.Context, userID, serviceID string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO favorites (user_id, service_id) VALUES ($1, $2)
		 ON CONFLICT DO NOTHING`,
		userID, serviceID)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "23503" || pgErr.Code == "22P02") {
		return ErrNotFound
	}
	return err
}

// RemoveFavorite unmarks serviceID for userID. Idempotent: removing a favorite
// that isn't set (or a malformed id) succeeds with no effect.
func (s *Store) RemoveFavorite(ctx context.Context, userID, serviceID string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM favorites WHERE user_id = $1 AND service_id = $2`,
		userID, serviceID)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22P02" { // malformed UUID: nothing to delete
		return nil
	}
	return err
}

// FavoriteIDs returns the set of service ids userID has favorited.
func (s *Store) FavoriteIDs(ctx context.Context, userID string) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT service_id FROM favorites WHERE user_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	set := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		set[id] = true
	}
	return set, rows.Err()
}

// SetLayout replaces userID's personal sort order (A5) with orderedIDs, where
// position 0 sorts first. It is a full replacement: ids the user previously
// placed but omitted here are dropped back to default ordering. Returns
// ErrNotFound when any id does not name a real service (FK violation or
// malformed UUID). The swap runs in one transaction so a failure leaves the
// prior order intact.
func (s *Store) SetLayout(ctx context.Context, userID string, orderedIDs []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `DELETE FROM user_layout WHERE user_id = $1`, userID); err != nil {
		return err
	}
	for i, id := range orderedIDs {
		_, err := tx.Exec(ctx,
			`INSERT INTO user_layout (user_id, service_id, sort_index) VALUES ($1, $2, $3)`,
			userID, id, i)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "23503" || pgErr.Code == "22P02") {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) userBy(ctx context.Context, where string, arg any) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, password_hash, role, theme_pref FROM users `+where, arg,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Role, &u.ThemePref)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	return u, nil
}
