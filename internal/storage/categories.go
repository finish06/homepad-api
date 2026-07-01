package storage

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ListCategories returns userID's OWN categories in their sort_index order
// (v9 — per-user, Invariant 2).
func (s *Store) ListCategories(ctx context.Context, userID string) ([]Category, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, sort_index, layout_row, layout_col_order, layout_width_pct
		 FROM categories WHERE user_id = $1 ORDER BY sort_index`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Category, 0)
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.ID, &c.Name, &c.SortIndex, &c.LayoutRow, &c.LayoutColOrder, &c.LayoutWidthPct); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CreateCategory appends a new category owned by userID at the end (sort_index =
// max+1 among the user's categories, or 0 when first). Returns ErrNameTaken when
// the user already has a category with that name (names are unique per user, D3).
func (s *Store) CreateCategory(ctx context.Context, userID, name string) (Category, error) {
	var c Category
	err := s.pool.QueryRow(ctx,
		`INSERT INTO categories (user_id, name, sort_index, layout_row)
		 VALUES ($1, $2,
		   COALESCE((SELECT max(sort_index) + 1 FROM categories WHERE user_id = $1), 0),
		   COALESCE((SELECT max(sort_index) + 1 FROM categories WHERE user_id = $1), 0))
		 RETURNING id, name, sort_index, layout_row, layout_col_order, layout_width_pct`,
		userID, name,
	).Scan(&c.ID, &c.Name, &c.SortIndex, &c.LayoutRow, &c.LayoutColOrder, &c.LayoutWidthPct)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Category{}, ErrNameTaken
	}
	if err != nil {
		return Category{}, err
	}
	return c, nil
}

// RenameCategory renames one of userID's OWN categories (v9 — owner-scoped).
// Returns ErrNotFound when id names no category owned by userID (malformed UUID,
// nonexistent, or another user's row → 404, D2) and ErrNameTaken on a per-user
// name collision.
func (s *Store) RenameCategory(ctx context.Context, id, userID, name string) (Category, error) {
	var c Category
	err := s.pool.QueryRow(ctx,
		`UPDATE categories SET name = $3 WHERE id = $1 AND user_id = $2
		 RETURNING id, name, sort_index, layout_row, layout_col_order, layout_width_pct`,
		id, userID, name,
	).Scan(&c.ID, &c.Name, &c.SortIndex, &c.LayoutRow, &c.LayoutColOrder, &c.LayoutWidthPct)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if pgErr.Code == "23505" {
			return Category{}, ErrNameTaken
		}
		if pgErr.Code == "22P02" { // malformed UUID: no such category
			return Category{}, ErrNotFound
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Category{}, ErrNotFound
	}
	if err != nil {
		return Category{}, err
	}
	return c, nil
}

// SetCategoryOrder rewrites the sort_index of userID's OWN categories from
// orderedIDs by position (v9 — owner-scoped), in one transaction. An id naming
// no category owned by userID (foreign or nonexistent) → ErrNotFound; the prior
// order is left intact (A14).
func (s *Store) SetCategoryOrder(ctx context.Context, userID string, orderedIDs []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	for i, id := range orderedIDs {
		tag, err := tx.Exec(ctx, `UPDATE categories SET sort_index = $3 WHERE id = $1 AND user_id = $2`, id, userID, i)
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

// DeleteCategory removes one of userID's OWN categories (v9 — owner-scoped).
// Idempotent for the owner: deleting an absent (or malformed) id succeeds with
// no effect. Another user's category is never touched (Invariant 2). The
// services FK is ON DELETE SET NULL, so the category's apps fall to
// Uncategorized — none deleted.
func (s *Store) DeleteCategory(ctx context.Context, id, userID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM categories WHERE id = $1 AND user_id = $2`, id, userID)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22P02" { // malformed UUID: nothing to delete
		return nil
	}
	return err
}

// SetCategoryLayout applies a batch of layout assignments to userID's OWN
// categories atomically — all rows update or none do (SPEC AC10). One
// transaction; an id naming no category owned by userID (foreign, nonexistent,
// or malformed) rolls the whole batch back with ErrNotFound so no partial layout
// is ever persisted.
func (s *Store) SetCategoryLayout(ctx context.Context, userID string, updates []CategoryLayout) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	for _, u := range updates {
		tag, err := tx.Exec(ctx,
			`UPDATE categories SET layout_row = $3, layout_col_order = $4, layout_width_pct = $5
			 WHERE id = $1 AND user_id = $2`,
			u.ID, userID, u.LayoutRow, u.LayoutColOrder, u.LayoutWidthPct)
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
