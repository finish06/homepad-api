package storage

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

//go:embed seed/homepad_demo_board.json
var demoBoardJSON []byte

type demoBoardEntry struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	Icon        string `json:"icon"`
	Description string `json:"description"`
	Category    string `json:"category"`
}

// demoCategoryOrder fixes the curated category ordering for the public demo
// board — the App Library sorts alphabetically by name, which is not the order
// we want the demo's tiles grouped in.
var demoCategoryOrder = []string{
	"Monitoring", "Dashboards", "Photos", "Media", "Documents", "Files",
	"Home Automation", "Networking", "Identity", "Productivity", "Development", "Backup",
}

var demoSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

// SeedDemoIfNoUsers bootstraps the public demo instance: a single shared login
// (homepad@gethomepad.dev / homepad1, admin) whose board is pre-populated from
// the embedded curated catalog, grouped into demoCategoryOrder. It runs only
// when the users table is empty, so on the ephemeral demo DB it self-heals
// after a pod restart without ever clobbering a live user's board. Idempotent:
// returns (false, nil) once any user exists. Demo-image only (this file lives
// on the demo-seed branch, never merged to main).
func (s *Store) SeedDemoIfNoUsers(ctx context.Context) (bool, error) {
	var entries []demoBoardEntry
	if err := json.Unmarshal(demoBoardJSON, &entries); err != nil {
		return false, fmt.Errorf("storage.SeedDemoIfNoUsers: parse board seed: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	// Same advisory lock the migrator/library seed use, so two replicas booting
	// at once cannot both observe an empty users table and each seed it.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(migrateLockKey)); err != nil {
		return false, fmt.Errorf("storage.SeedDemoIfNoUsers: acquire lock: %w", err)
	}

	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		return false, fmt.Errorf("storage.SeedDemoIfNoUsers: count users: %w", err)
	}
	if n > 0 {
		return false, nil // already bootstrapped or a real user exists — never clobber
	}

	hash, err := bcrypt.GenerateFromPassword([]byte("homepad1"), bcrypt.DefaultCost)
	if err != nil {
		return false, fmt.Errorf("storage.SeedDemoIfNoUsers: hash: %w", err)
	}

	var uid string
	if err := tx.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, role, display_name)
		 VALUES ('homepad@gethomepad.dev', $1, 'admin', 'homepad') RETURNING id`,
		string(hash)).Scan(&uid); err != nil {
		return false, fmt.Errorf("storage.SeedDemoIfNoUsers: user: %w", err)
	}

	catID := make(map[string]string, len(demoCategoryOrder))
	for i, name := range demoCategoryOrder {
		var id string
		if err := tx.QueryRow(ctx,
			`INSERT INTO categories (name, sort_index, user_id) VALUES ($1, $2, $3::uuid) RETURNING id`,
			name, i, uid).Scan(&id); err != nil {
			return false, fmt.Errorf("storage.SeedDemoIfNoUsers: category %q: %w", name, err)
		}
		catID[name] = id
	}

	for _, e := range entries {
		slug := strings.Trim(demoSlugRe.ReplaceAllString(strings.ToLower(e.Name), "-"), "-")
		var cid interface{}
		if id, ok := catID[e.Category]; ok {
			cid = id
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO services (slug, name, description, url, icon, category_id, user_id)
			 VALUES ($1, $2, $3, $4, $5, $6::uuid, $7::uuid)`,
			slug, e.Name, e.Description, e.URL, e.Icon, cid, uid); err != nil {
			return false, fmt.Errorf("storage.SeedDemoIfNoUsers: service %q: %w", e.Name, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
