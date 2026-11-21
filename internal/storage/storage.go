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
	// DensityPref is the v16 tile density (large|compact|list), per user
	// (decision record 2026-09-12, OQ-9). DB default 'compact'.
	DensityPref string
	// ShowHealthBar is the per-user visibility of the health panel's
	// distribution bar (SPEC-health-bar-visibility-toggle). Column is
	// NOT NULL DEFAULT TRUE, so an existing user reads back true.
	ShowHealthBar bool
	// ShowUptimeDisplay is the per-user visibility of the per-tile uptime
	// sparkline (cap6 v2). Seeded at account creation from
	// system_settings.show_uptime_display, which is the DEFAULT for new
	// accounts — not a live override (AC-026/AC-027).
	ShowUptimeDisplay bool
	// DisplayName is the optional human name (v7). The column is nullable; it
	// is scanned via COALESCE so an unset name reads as "" (the frontend then
	// falls back to the email's first letter for the avatar). A kid always has
	// one (the users_kid_credentials_check constraint).
	DisplayName string
	// AvatarColor is the account's avatar color, one of the frontend accent ids
	// (SPEC-kid-accounts §2.3); "" when unset, as it is for every adult today.
	AvatarColor string
	// ShowWidgets is the per-user "Show widgets" switch (SPEC-status-widgets
	// AC-031): tile widget lines and the glance strip. NOT NULL DEFAULT TRUE.
	ShowWidgets bool
}

// userColumns is the one SELECT list every User read uses. email and
// password_hash are NULL for a kid (migration 0018), so they are COALESCEd to
// "": a kid reads back with no email and an empty hash, which bcrypt can never
// match.
const userColumns = `id, COALESCE(email, ''), COALESCE(password_hash, ''), role, theme_pref, density_pref,
	show_health_bar, show_uptime_display, COALESCE(display_name, ''), COALESCE(avatar_color, ''), show_widgets`

// scanUser scans a row selected with userColumns first; extra receives any
// columns selected after them.
func scanUser(row pgx.Row, extra ...any) (User, error) {
	var u User
	dest := append([]any{&u.ID, &u.Email, &u.PasswordHash, &u.Role, &u.ThemePref, &u.DensityPref,
		&u.ShowHealthBar, &u.ShowUptimeDisplay, &u.DisplayName, &u.AvatarColor, &u.ShowWidgets}, extra...)
	err := row.Scan(dest...)
	return u, err
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
	// ClickAction (v23) is the tile's click behavior: 'new_tab' | 'same_tab' |
	// 'iframe'. Always populated — the DB column is NOT NULL DEFAULT 'new_tab'.
	ClickAction string
	// IframeSameOrigin (SPEC-iframe-sandbox-optin) is "Let this app keep its
	// sign-in": the overlay adds allow-same-origin to its sandbox only when it
	// is on. NOT NULL DEFAULT FALSE.
	IframeSameOrigin bool
}

// Category is an admin-curated catalog section (v4). Ordering is the explicit
// admin-controlled SortIndex, not alphabetical. GridWidth (SPEC-app-grid §3B) is
// the App Grid box width 1–8 — it drives both the box's page-column span and its
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

// householdCatalogMigration is 0021, the one-way move of the catalog from
// the first admin to the household (SPEC-household-catalog-owner §6.2).
const householdCatalogMigration = "0021_household_catalog.up.sql"

// MigrateReport says what one migration run did.
type MigrateReport struct {
	// Applied is every file this run executed, in order. Empty when the
	// database was up to date.
	Applied []string
	// ArchivedServices and ArchivedSections are the rows 0021 moved to
	// catalog_archive, when this run applied 0021.
	ArchivedServices, ArchivedSections int
}

// ArchiveLine is the one startup log line for the rows 0021 archived (AC-005),
// or "" when this run archived nothing, as on every boot after the first.
func (r MigrateReport) ArchiveLine() string {
	if r.ArchivedServices+r.ArchivedSections == 0 {
		return ""
	}
	return fmt.Sprintf("catalog: archived %d services and %d sections that no dashboard showed (catalog_archive)",
		r.ArchivedServices, r.ArchivedSections)
}

// MigrationMessageError is a migration that stopped with RAISE EXCEPTION: a
// message written for the person running the upgrade, such as 0021's "the
// catalog has apps but no admin account exists" (AC-009). Error is that
// message, unchanged; File names the migration; Unwrap gives the Postgres
// error.
type MigrationMessageError struct {
	File string
	Err  *pgconn.PgError
}

func (e *MigrationMessageError) Error() string { return e.Err.Message }

func (e *MigrationMessageError) Unwrap() error { return e.Err }

// Migrate applies the embedded migrations that have not run yet. See
// MigrateReport.
func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.MigrateReport(ctx)
	return err
}

// MigrateReport applies every embedded *.up.sql not yet recorded in
// schema_migrations, in lexical order, and records each one
// (SPEC-household-catalog-owner §6.1). It all runs in one transaction under an
// advisory lock, so concurrent migrators serialize, and a failed file records
// nothing: the next boot tries again. A database from before
// schema_migrations existed (17.x) re-runs every file once; they were written
// to be re-run.
func (s *Store) MigrateReport(ctx context.Context) (MigrateReport, error) {
	return s.migrate(ctx, migrations.FS)
}

func (s *Store) migrate(ctx context.Context, fsys fs.FS) (MigrateReport, error) {
	var rep MigrateReport
	names, err := fs.Glob(fsys, "*.up.sql")
	if err != nil {
		return rep, err
	}
	sort.Strings(names)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return rep, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(migrateLockKey)); err != nil {
		return rep, fmt.Errorf("storage.Migrate: acquire lock: %w", err)
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		    name        TEXT        PRIMARY KEY,
		    applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return rep, fmt.Errorf("storage.Migrate: schema_migrations: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT name FROM schema_migrations`)
	if err != nil {
		return rep, fmt.Errorf("storage.Migrate: read schema_migrations: %w", err)
	}
	done, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return rep, fmt.Errorf("storage.Migrate: read schema_migrations: %w", err)
	}
	recorded := make(map[string]bool, len(done))
	for _, name := range done {
		recorded[name] = true
	}

	for _, name := range names {
		if recorded[name] {
			continue
		}
		sqlBytes, err := fs.ReadFile(fsys, name)
		if err != nil {
			return rep, err
		}
		if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
			// A RAISE EXCEPTION in a migration is a message for the person
			// running the upgrade (AC-009): its text is passed on as it is.
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "P0001" {
				return rep, &MigrationMessageError{File: name, Err: pgErr}
			}
			return rep, fmt.Errorf("storage.Migrate: %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, name); err != nil {
			return rep, fmt.Errorf("storage.Migrate: record %s: %w", name, err)
		}
		rep.Applied = append(rep.Applied, name)
		if name == householdCatalogMigration {
			// 0021 runs once, so every archived row is from this run.
			if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE kind = 'service'),
				count(*) FILTER (WHERE kind = 'category') FROM catalog_archive`).
				Scan(&rep.ArchivedServices, &rep.ArchivedSections); err != nil {
				return rep, fmt.Errorf("storage.Migrate: count catalog_archive: %w", err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return MigrateReport{}, err
	}
	return rep, nil
}

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Pool exposes the connection pool so the session store (internal/session)
// shares it rather than opening a second one.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

func (s *Store) Close() error {
	if s.pool != nil {
		s.pool.Close()
	}
	return nil
}

// CountUsers counts the ADULT accounts (admin and user). It backs the
// first-account-becomes-admin rule, which kids must not influence: a kid is
// never "the first account", and its existence must not close the bootstrap.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE role <> 'kid'`).Scan(&n)
	return n, err
}

// CreateUser creates an adult account (register, OIDC). It cannot create a
// kid: a kid has no email, and the users_kid_credentials_check constraint
// refuses a kid row that carries one. Kids are created by CreateKid only.
//
// It takes no lock and no cap: it is the seeding and test path. The request
// paths use RegisterUser, CreateOIDCUser and AcceptInvite (accounts.go).
func (s *Store) CreateUser(ctx context.Context, email, passwordHash, role string) (User, error) {
	return insertUser(ctx, s.pool, email, passwordHash, role, "")
}

// UserByEmail resolves an adult by email (login, OIDC). It never returns a
// kid: kids have no email, and the role filter makes that explicit rather than
// a consequence of the schema.
func (s *Store) UserByEmail(ctx context.Context, email string) (User, error) {
	return s.userBy(ctx, `WHERE email = $1 AND role <> 'kid'`, email)
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

// SetDensityPref updates a single user's tile density (v16, OQ-9). Same shape
// as SetThemePref: the handler validates first, the column CHECK is a backstop,
// only userID's row is written, ErrNotFound when it names no user.
func (s *Store) SetDensityPref(ctx context.Context, userID, pref string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET density_pref = $2 WHERE id = $1`, userID, pref)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetShowHealthBar updates a single user's health-bar visibility
// (SPEC-health-bar-visibility-toggle AC-006/AC-008). Scoped to one row by id —
// the handler supplies the session's user, so a user can only write their own.
// SetShowUptimeDisplay updates a single user's uptime-display preference
// (cap6 v2, AC-016/AC-018). Scoped to one row by id — the handler supplies the
// session's user, so a user can only write their own.
func (s *Store) SetShowUptimeDisplay(ctx context.Context, userID string, show bool) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET show_uptime_display = $2 WHERE id = $1`, userID, show)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetShowHealthBar(ctx context.Context, userID string, show bool) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET show_health_bar = $2 WHERE id = $1`, userID, show)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListServices returns the household's catalog decorated with viewerID's
// personal layout order (A5/A6): services the viewer has placed come first by
// their saved sort_index, then the rest in name order. Every user reads the
// same set and keeps their OWN ordering of it (SPEC-household-catalog-owner).
func (s *Store) ListServices(ctx context.Context, viewerID string) ([]Service, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT s.id, s.slug, s.name, s.description, s.url, s.icon, COALESCE(s.gatus_key, ''),
		        s.category_id, c.name, s.source_library_id, s.click_action, s.iframe_same_origin
		   FROM services s
		   LEFT JOIN user_layout ul ON ul.service_id = s.id AND ul.user_id = $1
		   LEFT JOIN categories c   ON c.id = s.category_id
		  ORDER BY ul.sort_index NULLS LAST, s.name`, viewerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Service
	for rows.Next() {
		var sv Service
		if err := rows.Scan(&sv.ID, &sv.Slug, &sv.Name, &sv.Description, &sv.URL, &sv.Icon, &sv.GatusKey,
			&sv.CategoryID, &sv.CategoryName, &sv.SourceLibraryID, &sv.ClickAction, &sv.IframeSameOrigin); err != nil {
			return nil, err
		}
		out = append(out, sv)
	}
	return out, rows.Err()
}

// CreateService adds an app to the catalog. An empty GatusKey is stored as
// NULL. Slugs are unique across the catalog (ErrSlugTaken).
func (s *Store) CreateService(ctx context.Context, in Service) (Service, error) {
	var gatusKey *string
	if in.GatusKey != "" {
		gatusKey = &in.GatusKey
	}
	// v23: an empty ClickAction rides on the column DEFAULT 'new_tab' (the API
	// leaves it "" when the caller omits clickAction, or supplies a validated
	// enum value). COALESCE NULLIF keeps the default authoritative in one place.
	clickAction := in.ClickAction
	if clickAction == "" {
		clickAction = "new_tab"
	}
	var sv Service
	err := s.pool.QueryRow(ctx,
		`INSERT INTO services (slug, name, description, url, icon, gatus_key, click_action, iframe_same_origin)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING id, slug, name, description, url, icon, COALESCE(gatus_key, ''), click_action, iframe_same_origin`,
		in.Slug, in.Name, in.Description, in.URL, in.Icon, gatusKey, clickAction, in.IframeSameOrigin,
	).Scan(&sv.ID, &sv.Slug, &sv.Name, &sv.Description, &sv.URL, &sv.Icon, &sv.GatusKey, &sv.ClickAction, &sv.IframeSameOrigin)
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
	// ClickAction (v23) is nil = leave unchanged; a non-nil pointer sets the tile's
	// click behavior. The API validates the enum before it reaches here.
	ClickAction *string
	// IframeSameOrigin (SPEC-iframe-sandbox-optin) is nil = leave unchanged.
	IframeSameOrigin *bool
}

// UpdateService applies a partial patch to a catalog app and returns the
// updated row. Returns ErrNotFound when id names no app (including a malformed
// UUID) and ErrSlugTaken when the new slug collides with another app.
func (s *Store) UpdateService(ctx context.Context, id string, in ServiceUpdate) (Service, error) {
	setGatus := in.GatusKey != nil
	var gatusKey *string
	if setGatus && *in.GatusKey != "" {
		gatusKey = in.GatusKey
	}

	// Validate an explicit category assignment up front: it must name a
	// section of the catalog, else ErrCategoryNotFound (→ 400), distinct from
	// a missing service's 404. Clearing (nil CategoryID) is free.
	if in.SetCategory && in.CategoryID != nil {
		if err := s.categoryExists(ctx, *in.CategoryID); err != nil {
			return Service{}, err
		}
	}

	var sv Service
	err := s.pool.QueryRow(ctx,
		`WITH updated AS (
		   UPDATE services SET
		     slug        = COALESCE($2, slug),
		     name        = COALESCE($3, name),
		     description = COALESCE($4, description),
		     url         = COALESCE($5, url),
		     icon        = COALESCE($6, icon),
		     gatus_key    = CASE WHEN $7 THEN $8 ELSE gatus_key END,
		     category_id  = CASE WHEN $9 THEN $10 ELSE category_id END,
		     click_action = COALESCE($11, click_action),
		     iframe_same_origin = COALESCE($12, iframe_same_origin),
		     updated_at   = now()
		   WHERE id = $1
		   RETURNING id, slug, name, description, url, icon, gatus_key, category_id, source_library_id, click_action,
		             iframe_same_origin
		 )
		 SELECT u.id, u.slug, u.name, u.description, u.url, u.icon, COALESCE(u.gatus_key, ''),
		        u.category_id, c.name, u.source_library_id, u.click_action, u.iframe_same_origin
		   FROM updated u
		   LEFT JOIN categories c ON c.id = u.category_id`,
		id, in.Slug, in.Name, in.Description, in.URL, in.Icon, setGatus, gatusKey, in.SetCategory, in.CategoryID, in.ClickAction,
		in.IframeSameOrigin,
	).Scan(&sv.ID, &sv.Slug, &sv.Name, &sv.Description, &sv.URL, &sv.Icon, &sv.GatusKey,
		&sv.CategoryID, &sv.CategoryName, &sv.SourceLibraryID, &sv.ClickAction, &sv.IframeSameOrigin)
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

// categoryExists returns nil when id names a section of the catalog,
// ErrCategoryNotFound otherwise (including a malformed UUID).
func (s *Store) categoryExists(ctx context.Context, id string) error {
	var one int
	err := s.pool.QueryRow(ctx, `SELECT 1 FROM categories WHERE id = $1`, id).Scan(&one)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22P02" { // malformed UUID
		return ErrCategoryNotFound
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCategoryNotFound
	}
	return err
}

// serviceExists returns nil when serviceID names a catalog app, ErrNotFound
// otherwise (malformed UUID or nonexistent → 404). The icon paths check it
// first.
func (s *Store) serviceExists(ctx context.Context, serviceID string) error {
	var one int
	err := s.pool.QueryRow(ctx, `SELECT 1 FROM services WHERE id = $1`, serviceID).Scan(&one)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22P02" { // malformed UUID
		return ErrNotFound
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// ServiceURL returns the registered url of a catalog app (v21 fetch-icon,
// widgets). ErrNotFound when id names no app (malformed UUID or nonexistent).
func (s *Store) ServiceURL(ctx context.Context, serviceID string) (string, error) {
	var url string
	err := s.pool.QueryRow(ctx, `SELECT url FROM services WHERE id = $1`, serviceID).Scan(&url)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22P02" { // malformed UUID
		return "", ErrNotFound
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return url, nil
}

// DeleteService removes a catalog app. Returns ErrNotFound when id names no
// app (malformed UUID or nonexistent → 404). Favorites, layout, icons, the
// widget and allow-list rows cascade.
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

// GetIcon returns the stored PNG bytes and ETag for a variant of a catalog
// app. Returns ErrNotFound when the app doesn't exist (404) or the variant has
// no upload.
func (s *Store) GetIcon(ctx context.Context, serviceID, variant string) (Icon, error) {
	if err := s.serviceExists(ctx, serviceID); err != nil {
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

// PutIcon upserts an icon variant on a catalog app. Returns ErrNotFound when
// the app doesn't exist (404). The upsert handles both first upload and
// replace (PK is (service_id, variant)).
func (s *Store) PutIcon(ctx context.Context, serviceID, variant string, bytes []byte, width, height int, etag string) error {
	if err := s.serviceExists(ctx, serviceID); err != nil {
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

// DeleteIcon removes a variant on a catalog app. Returns ErrNotFound when the
// app doesn't exist (404). Idempotent: deleting a variant that isn't set
// succeeds.
func (s *Store) DeleteIcon(ctx context.Context, serviceID, variant string) error {
	if err := s.serviceExists(ctx, serviceID); err != nil {
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
	// SPEC-245-224 — the catalog is shared, so any user may favorite any service
	// in it. Ownership is no longer required (the v9 per-user "own service only"
	// gate is retired); the FK below still rejects a nonexistent service (404).
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
		// SPEC-245-224 — the catalog is shared, so a user may order any service
		// that exists in it (the v9 per-user "own services only" gate is
		// retired). The EXISTS guard now only rejects a nonexistent id → 0 rows
		// → ErrNotFound (404).
		tag, err := tx.Exec(ctx,
			`INSERT INTO user_layout (user_id, service_id, sort_index)
			 SELECT $1, $2, $3 WHERE EXISTS (SELECT 1 FROM services WHERE id = $2)`,
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
	u, err := scanUser(s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users `+where, arg))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	return u, nil
}
