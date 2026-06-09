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

// Migrate applies every embedded *.up.sql migration in lexical order. The
// migrations are written with IF NOT EXISTS so re-running on boot is a no-op.
func (s *Store) Migrate(ctx context.Context) error {
	names, err := fs.Glob(migrations.FS, "*.up.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		sqlBytes, err := migrations.FS.ReadFile(name)
		if err != nil {
			return err
		}
		if _, err := s.pool.Exec(ctx, string(sqlBytes)); err != nil {
			return fmt.Errorf("storage.Migrate: %s: %w", name, err)
		}
	}
	return nil
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
		 RETURNING id, email, password_hash, role`,
		email, passwordHash, role,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Role)
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

// ListServices returns the shared catalog ordered by name.
func (s *Store) ListServices(ctx context.Context) ([]Service, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, slug, name, description, url, icon, COALESCE(gatus_key, '')
		   FROM services ORDER BY name`)
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

func (s *Store) userBy(ctx context.Context, where string, arg any) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, password_hash, role FROM users `+where, arg,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	return u, nil
}
