package storage

import "context"

// CollapsedCategoryIDs returns the set of category ids userID has collapsed
// (v5). A fresh user has none — the empty result means everything is expanded
// (the default).
func (s *Store) CollapsedCategoryIDs(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT category_id FROM user_collapsed_categories WHERE user_id = $1
		 ORDER BY category_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SetCollapsedCategories replaces userID's collapsed set with categoryIDs (a
// whole-set replace, like SetLayout). Unknown, stale, or malformed ids are
// silently dropped — the INSERT only keeps ids that name a live category, so a
// category deleted between the client's read and write is simply ignored (never
// a 4xx). The swap runs in one transaction so a failure leaves the prior set
// intact.
func (s *Store) SetCollapsedCategories(ctx context.Context, userID string, categoryIDs []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`DELETE FROM user_collapsed_categories WHERE user_id = $1`, userID); err != nil {
		return err
	}
	// Compare ids as text against the live categories so unknown/stale ids match
	// nothing (dropped silently) and a malformed id never aborts the cast.
	if _, err := tx.Exec(ctx,
		`INSERT INTO user_collapsed_categories (user_id, category_id)
		 SELECT $1, c.id FROM categories c WHERE c.id::text = ANY($2::text[])
		 ON CONFLICT DO NOTHING`,
		userID, categoryIDs); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
