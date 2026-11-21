package storage

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ListCategories returns the catalog's sections in their sort_index order.
// Every user reads the same set (SPEC-household-catalog-owner).
func (s *Store) ListCategories(ctx context.Context) ([]Category, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, sort_index, grid_width FROM categories ORDER BY sort_index, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Category, 0)
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.ID, &c.Name, &c.SortIndex, &c.GridWidth); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CreateCategory appends a new section at the end (sort_index = max+1, or 0
// when first). Returns ErrNameTaken when a section already has that name
// (names are unique across the catalog).
func (s *Store) CreateCategory(ctx context.Context, name string) (Category, error) {
	var c Category
	err := s.pool.QueryRow(ctx,
		`INSERT INTO categories (name, sort_index)
		 VALUES ($1, COALESCE((SELECT max(sort_index) + 1 FROM categories), 0))
		 RETURNING id, name, sort_index, grid_width`,
		name,
	).Scan(&c.ID, &c.Name, &c.SortIndex, &c.GridWidth)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Category{}, ErrNameTaken
	}
	if err != nil {
		return Category{}, err
	}
	return c, nil
}

// RenameCategory renames a section. Returns ErrNotFound when id names no
// section (malformed UUID or nonexistent → 404) and ErrNameTaken on a name
// collision.
func (s *Store) RenameCategory(ctx context.Context, id, name string) (Category, error) {
	var c Category
	err := s.pool.QueryRow(ctx,
		`UPDATE categories SET name = $2 WHERE id = $1
		 RETURNING id, name, sort_index, grid_width`,
		id, name,
	).Scan(&c.ID, &c.Name, &c.SortIndex, &c.GridWidth)
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

// SetCategoryOrder rewrites the sort_index of the sections from orderedIDs by
// position, in one transaction. An id naming no section → ErrNotFound; the
// prior order is left intact.
func (s *Store) SetCategoryOrder(ctx context.Context, orderedIDs []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	for i, id := range orderedIDs {
		tag, err := tx.Exec(ctx, `UPDATE categories SET sort_index = $2 WHERE id = $1`, id, i)
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

// DeleteCategory removes a section. Idempotent: deleting an absent (or
// malformed) id succeeds with no effect. The services FK is ON DELETE SET
// NULL, so the section's apps fall to Uncategorized — none deleted.
func (s *Store) DeleteCategory(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM categories WHERE id = $1`, id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22P02" { // malformed UUID: nothing to delete
		return nil
	}
	return err
}

// SetCategoryWidth sets the App Grid box width (grid_width) of a section
// (SPEC-app-grid §3B). The caller validates the span; the DB CHECK is the
// floor. Returns ErrNotFound when id names no section (malformed UUID or
// nonexistent → 404).
func (s *Store) SetCategoryWidth(ctx context.Context, id string, width int) (Category, error) {
	var c Category
	err := s.pool.QueryRow(ctx,
		`UPDATE categories SET grid_width = $2 WHERE id = $1
		 RETURNING id, name, sort_index, grid_width`,
		id, width,
	).Scan(&c.ID, &c.Name, &c.SortIndex, &c.GridWidth)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22P02" { // malformed UUID: no such category
		return Category{}, ErrNotFound
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Category{}, ErrNotFound
	}
	if err != nil {
		return Category{}, err
	}
	return c, nil
}
