package storage

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ListCategories returns all categories in admin sort_index order (v4).
func (s *Store) ListCategories(ctx context.Context) ([]Category, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, sort_index FROM categories ORDER BY sort_index`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Category, 0)
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.ID, &c.Name, &c.SortIndex); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CreateCategory appends a new category at the end (sort_index = max+1, or 0
// when first) so creation never disturbs existing order. Returns ErrNameTaken
// when the name already exists (names are UNIQUE).
func (s *Store) CreateCategory(ctx context.Context, name string) (Category, error) {
	var c Category
	err := s.pool.QueryRow(ctx,
		`INSERT INTO categories (name, sort_index)
		 VALUES ($1, COALESCE((SELECT max(sort_index) + 1 FROM categories), 0))
		 RETURNING id, name, sort_index`,
		name,
	).Scan(&c.ID, &c.Name, &c.SortIndex)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Category{}, ErrNameTaken
	}
	if err != nil {
		return Category{}, err
	}
	return c, nil
}

// RenameCategory updates a category's name. Returns ErrNotFound when id names no
// category (including a malformed UUID) and ErrNameTaken on a name collision.
func (s *Store) RenameCategory(ctx context.Context, id, name string) (Category, error) {
	var c Category
	err := s.pool.QueryRow(ctx,
		`UPDATE categories SET name = $2 WHERE id = $1
		 RETURNING id, name, sort_index`,
		id, name,
	).Scan(&c.ID, &c.Name, &c.SortIndex)
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

// SetCategoryOrder rewrites every category's sort_index from orderedIDs by
// position (whole-array reindex, like SetLayout), in one transaction so a
// failure leaves the prior order intact. A malformed id → ErrNotFound.
func (s *Store) SetCategoryOrder(ctx context.Context, orderedIDs []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	for i, id := range orderedIDs {
		_, err := tx.Exec(ctx, `UPDATE categories SET sort_index = $2 WHERE id = $1`, id, i)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "22P02" { // malformed UUID
			return ErrNotFound
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// DeleteCategory removes a category. Idempotent: deleting an absent category (or
// a malformed id) succeeds with no effect. The services FK is ON DELETE SET
// NULL, so the category's apps fall back to Uncategorized — none are deleted.
func (s *Store) DeleteCategory(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM categories WHERE id = $1`, id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22P02" { // malformed UUID: nothing to delete
		return nil
	}
	return err
}
