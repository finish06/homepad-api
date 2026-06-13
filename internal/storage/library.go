package storage

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// LibraryApp is one admin-curated App Library offer (v9). It is pure catalog
// metadata with no owner — an offer, never an assignment. SuggestedCategory is
// a free-text hint (D5), not a category FK.
type LibraryApp struct {
	ID                string
	Name              string
	URL               string
	Icon              string
	Description       string
	SuggestedCategory string
	SortIndex         int
}

// LibraryOffer is a LibraryApp as seen by a browsing user: the offer plus the
// per-user `added` hint — whether the caller already holds a copy of it
// (source_library_id == offer id). The flag is a UI hint only; it never blocks
// a re-add (D6).
type LibraryOffer struct {
	LibraryApp
	Added bool
}

// LibraryAppUpdate is a partial patch of an offer. A nil field is left
// unchanged.
type LibraryAppUpdate struct {
	Name              *string
	URL               *string
	Icon              *string
	Description       *string
	SuggestedCategory *string
}

// ListLibrary returns every offer in sort_index order, each tagged with whether
// userID already holds a copy (the `added` hint, A9).
func (s *Store) ListLibrary(ctx context.Context, userID string) ([]LibraryOffer, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT la.id, la.name, la.url, la.icon, la.description, la.suggested_category, la.sort_index,
		        EXISTS (SELECT 1 FROM services s
		                 WHERE s.user_id = $1 AND s.source_library_id = la.id) AS added
		   FROM library_apps la
		  ORDER BY la.sort_index`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]LibraryOffer, 0)
	for rows.Next() {
		var o LibraryOffer
		if err := rows.Scan(&o.ID, &o.Name, &o.URL, &o.Icon, &o.Description, &o.SuggestedCategory, &o.SortIndex, &o.Added); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// CreateLibraryApp appends a new offer at the end of the browse order
// (sort_index = max+1, A8).
func (s *Store) CreateLibraryApp(ctx context.Context, in LibraryApp) (LibraryApp, error) {
	var la LibraryApp
	err := s.pool.QueryRow(ctx,
		`INSERT INTO library_apps (name, url, icon, description, suggested_category, sort_index)
		 VALUES ($1, $2, $3, $4, $5, COALESCE((SELECT max(sort_index) + 1 FROM library_apps), 0))
		 RETURNING id, name, url, icon, description, suggested_category, sort_index`,
		in.Name, in.URL, in.Icon, in.Description, in.SuggestedCategory,
	).Scan(&la.ID, &la.Name, &la.URL, &la.Icon, &la.Description, &la.SuggestedCategory, &la.SortIndex)
	if err != nil {
		return LibraryApp{}, err
	}
	return la, nil
}

// UpdateLibraryApp applies a partial patch to an offer and returns it. Editing
// an offer does NOT touch any user's copies (C1) — the copy is independent.
// Returns ErrNotFound when id names no offer (including a malformed UUID).
func (s *Store) UpdateLibraryApp(ctx context.Context, id string, in LibraryAppUpdate) (LibraryApp, error) {
	var la LibraryApp
	err := s.pool.QueryRow(ctx,
		`UPDATE library_apps SET
		   name               = COALESCE($2, name),
		   url                = COALESCE($3, url),
		   icon               = COALESCE($4, icon),
		   description        = COALESCE($5, description),
		   suggested_category = COALESCE($6, suggested_category),
		   updated_at         = now()
		 WHERE id = $1
		 RETURNING id, name, url, icon, description, suggested_category, sort_index`,
		id, in.Name, in.URL, in.Icon, in.Description, in.SuggestedCategory,
	).Scan(&la.ID, &la.Name, &la.URL, &la.Icon, &la.Description, &la.SuggestedCategory, &la.SortIndex)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22P02" { // malformed UUID
		return LibraryApp{}, ErrNotFound
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return LibraryApp{}, ErrNotFound
	}
	if err != nil {
		return LibraryApp{}, err
	}
	return la, nil
}

// SetLibraryOrder rewrites sort_index by position from orderedIDs in one
// transaction (the v4 reorder contract, A8). An id naming no offer → ErrNotFound;
// the prior order is left intact.
func (s *Store) SetLibraryOrder(ctx context.Context, orderedIDs []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	for i, id := range orderedIDs {
		tag, err := tx.Exec(ctx, `UPDATE library_apps SET sort_index = $2, updated_at = now() WHERE id = $1`, id, i)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "22P02" { // malformed UUID
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

// DeleteLibraryApp removes an offer. Idempotent: deleting an absent (or
// malformed) id succeeds with no effect. Existing copies are UNTOUCHED — the
// services.source_library_id FK is ON DELETE SET NULL, so their provenance
// breadcrumb just goes null (C1 / OQ5).
func (s *Store) DeleteLibraryApp(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM library_apps WHERE id = $1`, id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22P02" { // malformed UUID — nothing to delete
		return nil
	}
	return err
}
