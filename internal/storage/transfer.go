package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// SPEC-backup-import §7 — the storage side of backup, restore and Homepage
// import. The planner (internal/transfer) is pure: it reads a TransferState and
// writes through a TransferTx. Every catalog row it reads or writes is the
// shared catalog owner's (AC-025, homepad-api#87), whichever admin runs it.

// importLockKey is the advisory lock that serializes restore and import
// applies (AC-009). It is distinct from migrateLockKey.
const importLockKey = 0x686f6d6570616432 // "homepad2"

// TransferState is everything the planner needs to know about the install.
type TransferState struct {
	// OwnerID is the shared catalog owner (the first admin).
	OwnerID string
	// CallerID is the admin running the export, restore or import.
	CallerID string
	Sections []TransferSection // in sort_index order
	Apps     []TransferApp     // the owner's services, by slug
	Offers   []TransferOffer   // in sort_index order
	Settings SystemSettings
	// People is every account, adults then kids. Loaded only when asked for.
	People []TransferPerson
	// CallerOrder is the caller's current view order of the catalog (the
	// ListServices order: saved layout first, then name). Import appends to it.
	CallerOrder []string
	// OtherAccounts counts accounts other than the caller.
	OtherAccounts int
	// AccountLimit is HOMEPAD_MAX_ACCOUNTS (0 = no limit) and Accounts what
	// it counts (every account plus pending invites), so a restore never
	// creates a kid past the cap (SPEC-household-basics AC-029).
	AccountLimit int
	Accounts     int
}

// TransferSection is one category of the shared catalog.
type TransferSection struct {
	ID        string
	Name      string
	SortIndex int
	GridWidth int
}

// TransferApp is one service of the shared catalog.
type TransferApp struct {
	ID              string
	Slug            string
	Name            string
	Description     string
	URL             string
	Icon            string
	GatusKey        string
	ClickAction     string
	SectionID       *string
	SourceLibraryID *string
	Icons           []TransferIcon // light before dark
}

// TransferIcon is one uploaded icon variant. Bytes is filled only when the
// state was loaded for an export.
type TransferIcon struct {
	Variant string
	ETag    string
	Bytes   []byte
}

// TransferOffer is one App Library offer.
type TransferOffer struct {
	ID                string
	Name              string
	URL               string
	Icon              string
	Description       string
	SuggestedCategory string
	SortIndex         int
}

// TransferPerson is one account with its per-person dashboard state. Only the
// fields a backup may carry are here: no password hash, no session.
type TransferPerson struct {
	ID                string
	Role              string // admin | user | kid
	Email             string // "" for a kid
	DisplayName       string // "" when unset (adults); the name (kids)
	AvatarColor       string // "" when unset
	ThemePref         string
	DensityPref       string
	ShowHealthBar     bool
	ShowUptimeDisplay bool
	Favorites         []string // service ids in the shared catalog
	Order             []string // service ids, saved layout order
	Collapsed         []string // category ids
	Allowed           []string // service ids (kids)
}

// TransferLoad says what LoadTransferState reads.
type TransferLoad struct {
	OwnerID   string
	CallerID  string
	People    bool // per-person state for every account
	IconBytes bool // icon bytes (export only)
}

// TransferAppRow is the full desired state of an app on create or update.
type TransferAppRow struct {
	Slug        string
	Name        string
	Description string
	URL         string
	Icon        string
	GatusKey    string // "" = unmonitored (NULL)
	ClickAction string
	SectionID   *string
}

// TransferOfferRow is the desired state of an App Library offer.
type TransferOfferRow struct {
	Name              string
	URL               string
	Icon              string
	Description       string
	SuggestedCategory string
	SortIndex         int
}

// TransferPersonRow is the per-person fields a restore sets.
type TransferPersonRow struct {
	DisplayName       *string // nil = NULL (adults only; a kid keeps its name)
	AvatarColor       *string // nil = NULL
	ThemePref         string
	DensityPref       string
	ShowHealthBar     bool
	ShowUptimeDisplay bool
}

// querier is what both the pool and a transaction offer.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// LoadTransferState reads the state for a preview or an export, in one
// read-only snapshot so the parts agree with each other.
func (s *Store) LoadTransferState(ctx context.Context, in TransferLoad) (*TransferState, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	return loadTransferState(ctx, tx, in)
}

func loadTransferState(ctx context.Context, q querier, in TransferLoad) (*TransferState, error) {
	st := &TransferState{OwnerID: in.OwnerID, CallerID: in.CallerID}
	err := collect(ctx, q, `SELECT id, name, sort_index, grid_width FROM categories
		WHERE user_id = $1 ORDER BY sort_index, name`, []any{in.OwnerID}, func(r pgx.Rows) error {
		var x TransferSection
		err := r.Scan(&x.ID, &x.Name, &x.SortIndex, &x.GridWidth)
		st.Sections = append(st.Sections, x)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("storage: transfer sections: %w", err)
	}
	appIdx := map[string]int{}
	err = collect(ctx, q, `SELECT id, slug, name, description, url, icon, COALESCE(gatus_key, ''), click_action,
		category_id, source_library_id FROM services WHERE user_id = $1 ORDER BY slug`, []any{in.OwnerID}, func(r pgx.Rows) error {
		var x TransferApp
		err := r.Scan(&x.ID, &x.Slug, &x.Name, &x.Description, &x.URL, &x.Icon, &x.GatusKey, &x.ClickAction,
			&x.SectionID, &x.SourceLibraryID)
		appIdx[x.ID] = len(st.Apps)
		st.Apps = append(st.Apps, x)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("storage: transfer apps: %w", err)
	}
	bytesCol := `NULL::bytea`
	if in.IconBytes {
		bytesCol = `si.bytes`
	}
	err = collect(ctx, q, `SELECT si.service_id, si.variant, si.etag, `+bytesCol+` FROM service_icons si
		JOIN services s ON s.id = si.service_id WHERE s.user_id = $1 ORDER BY si.service_id, si.variant DESC`,
		[]any{in.OwnerID}, func(r pgx.Rows) error {
			var id string
			var x TransferIcon
			if err := r.Scan(&id, &x.Variant, &x.ETag, &x.Bytes); err != nil {
				return err
			}
			if i, ok := appIdx[id]; ok {
				st.Apps[i].Icons = append(st.Apps[i].Icons, x)
			}
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("storage: transfer icons: %w", err)
	}
	err = collect(ctx, q, `SELECT id, name, url, icon, description, suggested_category, sort_index
		FROM library_apps ORDER BY sort_index, name`, nil, func(r pgx.Rows) error {
		var x TransferOffer
		err := r.Scan(&x.ID, &x.Name, &x.URL, &x.Icon, &x.Description, &x.SuggestedCategory, &x.SortIndex)
		st.Offers = append(st.Offers, x)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("storage: transfer offers: %w", err)
	}
	st.Settings = defaultSystemSettings()
	err = q.QueryRow(ctx, `SELECT show_uptime_display, status_degraded_ms FROM system_settings WHERE id = 1`).
		Scan(&st.Settings.ShowUptimeDisplay, &st.Settings.StatusDegradedMs)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("storage: transfer settings: %w", err)
	}
	// The caller's view order is ListServices' order.
	err = collect(ctx, q, `SELECT s.id FROM services s
		LEFT JOIN user_layout ul ON ul.service_id = s.id AND ul.user_id = $2
		WHERE s.user_id = $1 ORDER BY ul.sort_index NULLS LAST, s.name`, []any{in.OwnerID, in.CallerID}, func(r pgx.Rows) error {
		var id string
		err := r.Scan(&id)
		st.CallerOrder = append(st.CallerOrder, id)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("storage: transfer caller order: %w", err)
	}
	if err := q.QueryRow(ctx, `SELECT count(*) FROM users WHERE id::text <> $1`, in.CallerID).Scan(&st.OtherAccounts); err != nil {
		return nil, fmt.Errorf("storage: transfer accounts: %w", err)
	}
	if in.People {
		if err := loadTransferPeople(ctx, q, st, appIdx); err != nil {
			return nil, fmt.Errorf("storage: transfer people: %w", err)
		}
	}
	return st, nil
}

// loadTransferPeople reads every account and its per-person state. Only
// references into the shared catalog are kept.
func loadTransferPeople(ctx context.Context, q querier, st *TransferState, appIdx map[string]int) error {
	secs := map[string]bool{}
	for _, s := range st.Sections {
		secs[s.ID] = true
	}
	idx := map[string]int{}
	err := collect(ctx, q, `SELECT `+userColumns+` FROM users
		ORDER BY CASE role WHEN 'kid' THEN 1 ELSE 0 END, created_at, id`, nil, func(r pgx.Rows) error {
		u, err := scanUser(r)
		if err != nil {
			return err
		}
		idx[u.ID] = len(st.People)
		st.People = append(st.People, TransferPerson{ID: u.ID, Role: u.Role, Email: u.Email, DisplayName: u.DisplayName,
			AvatarColor: u.AvatarColor, ThemePref: u.ThemePref, DensityPref: u.DensityPref, ShowHealthBar: u.ShowHealthBar,
			ShowUptimeDisplay: u.ShowUptimeDisplay,
			Favorites:         []string{}, Order: []string{}, Collapsed: []string{}, Allowed: []string{}})
		return nil
	})
	if err != nil {
		return err
	}
	for _, rel := range []struct {
		sql   string
		into  func(p *TransferPerson, id string)
		valid map[string]bool
	}{
		{`SELECT user_id, service_id FROM favorites ORDER BY user_id, created_at, service_id`,
			func(p *TransferPerson, id string) { p.Favorites = append(p.Favorites, id) }, nil},
		{`SELECT user_id, service_id FROM user_layout ORDER BY user_id, sort_index`,
			func(p *TransferPerson, id string) { p.Order = append(p.Order, id) }, nil},
		{`SELECT user_id, category_id FROM user_collapsed_categories ORDER BY user_id, category_id`,
			func(p *TransferPerson, id string) { p.Collapsed = append(p.Collapsed, id) }, secs},
		{`SELECT user_id, service_id FROM kid_allowed_services ORDER BY user_id, service_id`,
			func(p *TransferPerson, id string) { p.Allowed = append(p.Allowed, id) }, nil},
	} {
		err := collect(ctx, q, rel.sql, nil, func(r pgx.Rows) error {
			var uid, id string
			if err := r.Scan(&uid, &id); err != nil {
				return err
			}
			i, ok := idx[uid]
			if !ok {
				return nil
			}
			if rel.valid != nil && !rel.valid[id] {
				return nil
			}
			if _, isApp := appIdx[id]; rel.valid == nil && !isApp {
				return nil
			}
			rel.into(&st.People[i], id)
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// collect runs a query and hands each row to fn.
func collect(ctx context.Context, q querier, sql string, args []any, fn func(pgx.Rows) error) error {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// TransferTx is one apply: a transaction holding importLockKey. It
// implements transfer.Writer.
type TransferTx struct {
	tx pgx.Tx
}

// InTransferTx runs fn in one transaction holding the import advisory lock,
// so two applies never interleave (AC-009). The lock is taken first, and the
// transaction is READ COMMITTED, so a waiting apply reads the first one's
// result once it gets the lock. fn's error rolls everything back and is
// returned as is.
func (s *Store) InTransferTx(ctx context.Context, fn func(tx *TransferTx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(importLockKey)); err != nil {
		return fmt.Errorf("storage: acquire import lock: %w", err)
	}
	if err := fn(&TransferTx{tx: tx}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// LoadState is LoadTransferState inside the apply's transaction.
func (t *TransferTx) LoadState(ctx context.Context, in TransferLoad) (*TransferState, error) {
	return loadTransferState(ctx, t.tx, in)
}

func (t *TransferTx) exec1(ctx context.Context, what, sql string, args ...any) error {
	tag, err := t.tx.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("storage: transfer %s: %w", what, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("storage: transfer %s: %w", what, ErrNotFound)
	}
	return nil
}

func (t *TransferTx) DeleteCatalog(ctx context.Context, owner string) error {
	for _, sql := range []string{
		`DELETE FROM services WHERE user_id = $1`,
		`DELETE FROM categories WHERE user_id = $1`,
	} {
		if _, err := t.tx.Exec(ctx, sql, owner); err != nil {
			return fmt.Errorf("storage: transfer delete catalog: %w", err)
		}
	}
	if _, err := t.tx.Exec(ctx, `DELETE FROM library_apps`); err != nil {
		return fmt.Errorf("storage: transfer delete library: %w", err)
	}
	return nil
}

func (t *TransferTx) CreateSection(ctx context.Context, owner, name string, sortIndex, gridWidth int) (string, error) {
	var id string
	err := t.tx.QueryRow(ctx, `INSERT INTO categories (user_id, name, sort_index, grid_width)
		VALUES ($1, $2, $3, $4) RETURNING id`, owner, name, sortIndex, gridWidth).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("storage: transfer create section: %w", err)
	}
	return id, nil
}

func (t *TransferTx) UpdateSection(ctx context.Context, owner, id string, sortIndex, gridWidth int) error {
	return t.exec1(ctx, "update section", `UPDATE categories SET sort_index = $3, grid_width = $4
		WHERE id = $1 AND user_id = $2`, id, owner, sortIndex, gridWidth)
}

func (t *TransferTx) CreateOffer(ctx context.Context, o TransferOfferRow) (string, error) {
	var id string
	err := t.tx.QueryRow(ctx, `INSERT INTO library_apps (name, url, icon, description, suggested_category, sort_index)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		o.Name, o.URL, o.Icon, o.Description, o.SuggestedCategory, o.SortIndex).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("storage: transfer create offer: %w", err)
	}
	return id, nil
}

func (t *TransferTx) UpdateOffer(ctx context.Context, id string, o TransferOfferRow) error {
	return t.exec1(ctx, "update offer", `UPDATE library_apps SET name = $2, url = $3, icon = $4, description = $5,
		suggested_category = $6, sort_index = $7, updated_at = now() WHERE id = $1`,
		id, o.Name, o.URL, o.Icon, o.Description, o.SuggestedCategory, o.SortIndex)
}

func (t *TransferTx) CreateApp(ctx context.Context, owner string, a TransferAppRow) (string, error) {
	var id string
	err := t.tx.QueryRow(ctx, `INSERT INTO services (user_id, slug, name, description, url, icon, gatus_key, click_action, category_id)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8, $9) RETURNING id`,
		owner, a.Slug, a.Name, a.Description, a.URL, a.Icon, a.GatusKey, a.ClickAction, a.SectionID).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("storage: transfer create app: %w", err)
	}
	return id, nil
}

func (t *TransferTx) UpdateApp(ctx context.Context, owner, id string, a TransferAppRow) error {
	return t.exec1(ctx, "update app", `UPDATE services SET slug = $3, name = $4, description = $5, url = $6, icon = $7,
		gatus_key = NULLIF($8, ''), click_action = $9, category_id = $10, updated_at = now()
		WHERE id = $1 AND user_id = $2`,
		id, owner, a.Slug, a.Name, a.Description, a.URL, a.Icon, a.GatusKey, a.ClickAction, a.SectionID)
}

// SetGatusKey fills an app's EMPTY status key, and nothing else (§4.3).
func (t *TransferTx) SetGatusKey(ctx context.Context, owner, id, key string) error {
	return t.exec1(ctx, "set gatus key", `UPDATE services SET gatus_key = $3, updated_at = now()
		WHERE id = $1 AND user_id = $2 AND gatus_key IS NULL`, id, owner, key)
}

// LinkLibrary re-links provenance (source_library_id) by an offer's name and
// URL (§3).
func (t *TransferTx) LinkLibrary(ctx context.Context, owner string, appIDs []string) error {
	_, err := t.tx.Exec(ctx, `UPDATE services s SET source_library_id = (
		  SELECT la.id FROM library_apps la WHERE la.name = s.name AND la.url = s.url ORDER BY la.sort_index, la.id LIMIT 1)
		WHERE s.user_id = $1 AND s.id::text = ANY($2::text[]) AND s.source_library_id IS NULL`, owner, appIDs)
	if err != nil {
		return fmt.Errorf("storage: transfer link library: %w", err)
	}
	return nil
}

func (t *TransferTx) PutIcon(ctx context.Context, appID, variant string, png []byte, width, height int, etag string) error {
	_, err := t.tx.Exec(ctx, `INSERT INTO service_icons (service_id, variant, bytes, byte_size, width, height, etag, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now())
		ON CONFLICT (service_id, variant) DO UPDATE SET bytes = EXCLUDED.bytes, byte_size = EXCLUDED.byte_size,
		  width = EXCLUDED.width, height = EXCLUDED.height, etag = EXCLUDED.etag, updated_at = now()`,
		appID, variant, png, len(png), width, height, etag)
	if err != nil {
		return fmt.Errorf("storage: transfer put icon: %w", err)
	}
	return nil
}

func (t *TransferTx) SetSettings(ctx context.Context, in SystemSettings) error {
	_, err := t.tx.Exec(ctx, `INSERT INTO system_settings (id, show_uptime_display, status_degraded_ms, updated_at)
		VALUES (1, $1, $2, now())
		ON CONFLICT (id) DO UPDATE SET show_uptime_display = EXCLUDED.show_uptime_display,
		  status_degraded_ms = EXCLUDED.status_degraded_ms, updated_at = now()`,
		in.ShowUptimeDisplay, in.StatusDegradedMs)
	if err != nil {
		return fmt.Errorf("storage: transfer settings: %w", err)
	}
	return nil
}

// CreateKid creates a kid the way the Accounts API does: no email, no
// password, no session.
func (t *TransferTx) CreateKid(ctx context.Context, name, avatarColor string) (string, error) {
	var id string
	err := t.tx.QueryRow(ctx, `INSERT INTO users (email, password_hash, role, display_name, avatar_color,
		                    show_health_bar, density_pref, show_uptime_display)
		VALUES (NULL, NULL, 'kid', $1, $2, FALSE, 'large',
		        COALESCE((SELECT show_uptime_display FROM system_settings WHERE id = 1), TRUE))
		RETURNING id`, name, avatarColor).Scan(&id)
	if isUniqueViolation(err, "users_kid_name_key") {
		return "", ErrKidNameTaken
	}
	if err != nil {
		return "", fmt.Errorf("storage: transfer create kid: %w", err)
	}
	return id, nil
}

// SetPerson sets a person's appearance and avatar; an adult's display name
// too (a kid keeps its name).
func (t *TransferTx) SetPerson(ctx context.Context, id string, p TransferPersonRow) error {
	return t.exec1(ctx, "set person", `UPDATE users SET theme_pref = $2, density_pref = $3, show_health_bar = $4,
		show_uptime_display = $5, avatar_color = $6,
		display_name = CASE WHEN role = 'kid' THEN display_name ELSE $7 END
		WHERE id = $1`, id, p.ThemePref, p.DensityPref, p.ShowHealthBar, p.ShowUptimeDisplay, p.AvatarColor, p.DisplayName)
}

func (t *TransferTx) AddFavorites(ctx context.Context, userID string, appIDs []string) error {
	_, err := t.tx.Exec(ctx, `INSERT INTO favorites (user_id, service_id)
		SELECT $1, s.id FROM services s WHERE s.id::text = ANY($2::text[]) ON CONFLICT DO NOTHING`, userID, appIDs)
	if err != nil {
		return fmt.Errorf("storage: transfer favorites: %w", err)
	}
	return nil
}

func (t *TransferTx) SetLayout(ctx context.Context, userID string, appIDs []string) error {
	if _, err := t.tx.Exec(ctx, `DELETE FROM user_layout WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("storage: transfer layout: %w", err)
	}
	_, err := t.tx.Exec(ctx, `INSERT INTO user_layout (user_id, service_id, sort_index)
		SELECT $1, s.id, (t.ord - 1)::int FROM unnest($2::text[]) WITH ORDINALITY AS t(id, ord)
		JOIN services s ON s.id::text = t.id ON CONFLICT DO NOTHING`, userID, appIDs)
	if err != nil {
		return fmt.Errorf("storage: transfer layout: %w", err)
	}
	return nil
}

func (t *TransferTx) SetCollapsed(ctx context.Context, userID string, sectionIDs []string) error {
	if _, err := t.tx.Exec(ctx, `DELETE FROM user_collapsed_categories WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("storage: transfer collapsed: %w", err)
	}
	_, err := t.tx.Exec(ctx, `INSERT INTO user_collapsed_categories (user_id, category_id)
		SELECT $1, c.id FROM categories c WHERE c.id::text = ANY($2::text[]) ON CONFLICT DO NOTHING`, userID, sectionIDs)
	if err != nil {
		return fmt.Errorf("storage: transfer collapsed: %w", err)
	}
	return nil
}

func (t *TransferTx) SetAllowed(ctx context.Context, kidID string, appIDs []string) error {
	if _, err := t.tx.Exec(ctx, `DELETE FROM kid_allowed_services WHERE user_id = $1`, kidID); err != nil {
		return fmt.Errorf("storage: transfer allow-list: %w", err)
	}
	_, err := t.tx.Exec(ctx, `INSERT INTO kid_allowed_services (user_id, service_id)
		SELECT $1, s.id FROM services s WHERE s.id::text = ANY($2::text[])
		  AND EXISTS (SELECT 1 FROM users WHERE id = $1 AND role = 'kid') ON CONFLICT DO NOTHING`, kidID, appIDs)
	if err != nil {
		return fmt.Errorf("storage: transfer allow-list: %w", err)
	}
	return nil
}
