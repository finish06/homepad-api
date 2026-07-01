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

// ErrNameTaken is returned by category create/rename when the name collides
// with an existing category (names are UNIQUE).
var ErrNameTaken = errors.New("storage: category name already in use")

// ErrCategoryNotFound is returned by UpdateService when an assignment names a
// category that does not exist (so the handler can answer 400, distinct from a
// missing service's 404).
var ErrCategoryNotFound = errors.New("storage: category not found")

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
	// DisplayName is the optional human name (v7). The column is nullable; it
	// is scanned via COALESCE so an unset name reads as "" (the frontend then
	// falls back to the email's first letter for the avatar).
	DisplayName string
}

// Service is a catalog entry. GatusKey is empty when the service is unmonitored.
// CategoryID/CategoryName are nil when the service is Uncategorized (v4);
// CategoryName is denormalized from the joined category for render convenience.
type Service struct {
	ID           string
	Slug         string
	Name         string
	Description  string
	URL          string
	Icon         string
	GatusKey     string
	CategoryID   *string
	CategoryName *string
	// SourceLibraryID is provenance only (C1): the library offer a copy was
	// added from, or nil for a custom app. Set on add-from-library (v9.2) and
	// by the 0007 cutover; never changes behavior.
	SourceLibraryID *string
}

// Category is an admin-curated catalog section (v4). Ordering is the explicit
// admin-controlled SortIndex, not alphabetical. GridWidth (SPEC-app-grid §3B) is
// the App Grid box width 1–6 — it drives both the box's page-column span and its
// links-per-row; new categories default to 3.
type Category struct {
	ID        string
	Name      string
	SortIndex int
	GridWidth int
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
		 RETURNING id, email, password_hash, role, theme_pref, COALESCE(display_name, '')`,
		email, passwordHash, role,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Role, &u.ThemePref, &u.DisplayName)
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

// ListServices returns userID's OWN catalog (v9 — per-user, Invariant 2) in
// their personal layout order (A5/A6): services the user has placed come first
// by their saved sort_index, then any not-yet-placed in name order.
func (s *Store) ListServices(ctx context.Context, userID string) ([]Service, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT s.id, s.slug, s.name, s.description, s.url, s.icon, COALESCE(s.gatus_key, ''),
		        s.category_id, c.name, s.source_library_id
		   FROM services s
		   LEFT JOIN user_layout ul ON ul.service_id = s.id AND ul.user_id = $1
		   LEFT JOIN categories c   ON c.id = s.category_id
		  WHERE s.user_id = $1
		  ORDER BY ul.sort_index NULLS LAST, s.name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Service
	for rows.Next() {
		var sv Service
		if err := rows.Scan(&sv.ID, &sv.Slug, &sv.Name, &sv.Description, &sv.URL, &sv.Icon, &sv.GatusKey,
			&sv.CategoryID, &sv.CategoryName, &sv.SourceLibraryID); err != nil {
			return nil, err
		}
		out = append(out, sv)
	}
	return out, rows.Err()
}

// CreateService inserts a catalog entry owned by userID (v9 — per-user). An
// empty GatusKey is stored as NULL. Slug uniqueness is per-user (§5.2), so two
// users may each hold the same slug; the same user may not (ErrSlugTaken).
func (s *Store) CreateService(ctx context.Context, userID string, in Service) (Service, error) {
	var gatusKey *string
	if in.GatusKey != "" {
		gatusKey = &in.GatusKey
	}
	var sv Service
	err := s.pool.QueryRow(ctx,
		`INSERT INTO services (user_id, slug, name, description, url, icon, gatus_key)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING id, slug, name, description, url, icon, COALESCE(gatus_key, '')`,
		userID, in.Slug, in.Name, in.Description, in.URL, in.Icon, gatusKey,
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
	// Category is three-state (v4): SetCategory false leaves the assignment
	// unchanged; SetCategory true with CategoryID nil clears it to NULL
	// (Uncategorized); SetCategory true with a non-nil id assigns that category.
	SetCategory bool
	CategoryID  *string
}

// UpdateService applies a partial patch to userID's OWN service and returns the
// updated row (v9 — owner-scoped). Returns ErrNotFound when id names no service
// owned by userID (including a malformed UUID or another user's row → 404, D2)
// and ErrSlugTaken when the new slug collides with another of the caller's
// entries.
func (s *Store) UpdateService(ctx context.Context, id, userID string, in ServiceUpdate) (Service, error) {
	setGatus := in.GatusKey != nil
	var gatusKey *string
	if setGatus && *in.GatusKey != "" {
		gatusKey = in.GatusKey
	}

	// Validate an explicit category assignment up front: it must name one of
	// the CALLER's own categories (A7), else ErrCategoryNotFound (→ 400),
	// distinct from a missing service's 404. Clearing (nil CategoryID) is free.
	if in.SetCategory && in.CategoryID != nil {
		if err := s.categoryOwnedBy(ctx, *in.CategoryID, userID); err != nil {
			return Service{}, err
		}
	}

	var sv Service
	err := s.pool.QueryRow(ctx,
		`WITH updated AS (
		   UPDATE services SET
		     slug        = COALESCE($3, slug),
		     name        = COALESCE($4, name),
		     description = COALESCE($5, description),
		     url         = COALESCE($6, url),
		     icon        = COALESCE($7, icon),
		     gatus_key   = CASE WHEN $8 THEN $9 ELSE gatus_key END,
		     category_id = CASE WHEN $10 THEN $11 ELSE category_id END,
		     updated_at  = now()
		   WHERE id = $1 AND user_id = $2
		   RETURNING id, slug, name, description, url, icon, gatus_key, category_id, source_library_id
		 )
		 SELECT u.id, u.slug, u.name, u.description, u.url, u.icon, COALESCE(u.gatus_key, ''),
		        u.category_id, c.name, u.source_library_id
		   FROM updated u
		   LEFT JOIN categories c ON c.id = u.category_id`,
		id, userID, in.Slug, in.Name, in.Description, in.URL, in.Icon, setGatus, gatusKey, in.SetCategory, in.CategoryID,
	).Scan(&sv.ID, &sv.Slug, &sv.Name, &sv.Description, &sv.URL, &sv.Icon, &sv.GatusKey,
		&sv.CategoryID, &sv.CategoryName, &sv.SourceLibraryID)
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

// categoryOwnedBy returns nil when id names a category owned by userID,
// ErrCategoryNotFound otherwise (including a malformed UUID or another user's
// category — A7). This is the per-user replacement for the v4 global existence
// check.
func (s *Store) categoryOwnedBy(ctx context.Context, id, userID string) error {
	var one int
	err := s.pool.QueryRow(ctx, `SELECT 1 FROM categories WHERE id = $1 AND user_id = $2`, id, userID).Scan(&one)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22P02" { // malformed UUID
		return ErrCategoryNotFound
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCategoryNotFound
	}
	return err
}

// serviceOwnedBy returns nil when serviceID is owned by userID, ErrNotFound
// otherwise (malformed UUID, nonexistent, or another user's row → 404, D2). The
// per-user gate the icon/favorite/layout paths use to keep Invariant 2 tight.
func (s *Store) serviceOwnedBy(ctx context.Context, serviceID, userID string) error {
	var one int
	err := s.pool.QueryRow(ctx, `SELECT 1 FROM services WHERE id = $1 AND user_id = $2`, serviceID, userID).Scan(&one)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22P02" { // malformed UUID
		return ErrNotFound
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// DeleteService removes one of userID's OWN catalog entries (v9 — owner-scoped).
// Returns ErrNotFound when id names no service owned by userID (malformed UUID,
// nonexistent, or another user's row → 404, D2). Favorites/layout/icons cascade.
func (s *Store) DeleteService(ctx context.Context, id, userID string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM services WHERE id = $1 AND user_id = $2`, id, userID)
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

// GetIcon returns the stored PNG bytes and ETag for one of userID's OWN
// service's variants (v9 — owner-scoped). Returns ErrNotFound when the service
// is not the caller's (404, D2) or the variant has no upload.
func (s *Store) GetIcon(ctx context.Context, serviceID, userID, variant string) (Icon, error) {
	if err := s.serviceOwnedBy(ctx, serviceID, userID); err != nil {
		return Icon{}, err
	}
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

// PutIcon upserts an icon variant on one of userID's OWN services (v9 —
// owner-scoped). Returns ErrNotFound when the service is not the caller's (404,
// D2). The upsert handles both first upload and replace (PK is (service_id,
// variant)).
func (s *Store) PutIcon(ctx context.Context, serviceID, userID, variant string, bytes []byte, width, height int, etag string) error {
	if err := s.serviceOwnedBy(ctx, serviceID, userID); err != nil {
		return err
	}
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

// DeleteIcon removes a variant on one of userID's OWN services (v9 —
// owner-scoped). Returns ErrNotFound when the service is not the caller's (404,
// D2). Idempotent for the owner: deleting a variant that isn't set succeeds.
func (s *Store) DeleteIcon(ctx context.Context, serviceID, userID, variant string) error {
	if err := s.serviceOwnedBy(ctx, serviceID, userID); err != nil {
		return err
	}
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
	// v9 — a user may only favorite their OWN service; another user's (or a
	// nonexistent) service → ErrNotFound (404, D2 — Invariant 2).
	if err := s.serviceOwnedBy(ctx, serviceID, userID); err != nil {
		return err
	}
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
		// v9 — only the caller's OWN services may be placed: the INSERT is
		// guarded on ownership, so a foreign or nonexistent id places 0 rows →
		// ErrNotFound (404/unchanged, Invariant 2 / A14).
		tag, err := tx.Exec(ctx,
			`INSERT INTO user_layout (user_id, service_id, sort_index)
			 SELECT $1, $2, $3 WHERE EXISTS (SELECT 1 FROM services WHERE id = $2 AND user_id = $1)`,
			userID, id, i)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "23503" || pgErr.Code == "22P02") {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) userBy(ctx context.Context, where string, arg any) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, password_hash, role, theme_pref, COALESCE(display_name, '') FROM users `+where, arg,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Role, &u.ThemePref, &u.DisplayName)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	return u, nil
}
