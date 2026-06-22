package storage

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// seedLibraryJSON is the committed catalog seed (the 39 homelab services,
// mirrored from homelab homepad/seed/homepad_seed.json). homepad-db runs on an
// ephemeral emptyDir in prod, so a pod restart wipes the catalog; embedding the
// seed lets a fresh DB self-heal on boot instead of presenting an empty App
// Library.
//
//go:embed seed/homepad_seed.json
var seedLibraryJSON []byte

// seedEntry is one row of the committed catalog seed. Description holds the
// gatus group ("kube"/"media"/"external"); slug/gatus_key are ignored here
// because library offers carry neither.
type seedEntry struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	Icon        string `json:"icon"`
	Description string `json:"description"`
}

// SeedLibraryIfEmpty populates the admin App Library from the committed catalog
// seed when — and only when — library_apps holds no rows. It is idempotent:
// once any offer exists (auto-seeded or admin-created) it is a no-op, so it
// never clobbers curation. The whole check-and-insert runs under the same
// advisory lock the migrator uses, so two replicas booting at once cannot
// double-seed. Returns the number of offers inserted (0 when already seeded).
func (s *Store) SeedLibraryIfEmpty(ctx context.Context) (int, error) {
	entries, err := parseSeed()
	if err != nil {
		return 0, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	// Same advisory lock the migrator uses: serialize the check-and-insert so
	// two replicas booting at once cannot both observe an empty library and
	// each seed it.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(migrateLockKey)); err != nil {
		return 0, fmt.Errorf("storage.SeedLibraryIfEmpty: acquire lock: %w", err)
	}

	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM library_apps`).Scan(&n); err != nil {
		return 0, fmt.Errorf("storage.SeedLibraryIfEmpty: count: %w", err)
	}
	if n > 0 {
		return 0, nil // already seeded or admin-curated — never clobber
	}

	for i, e := range entries {
		if _, err := tx.Exec(ctx,
			`INSERT INTO library_apps (name, url, icon, description, suggested_category, sort_index)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			e.Name, e.URL, e.Icon, e.Description, suggestedCategory(e.Description), i,
		); err != nil {
			return 0, fmt.Errorf("storage.SeedLibraryIfEmpty: insert %q: %w", e.Name, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(entries), nil
}

// suggestedCategory maps a seed entry's gatus-group description ("kube",
// "media", "external") to the curated category name the v4 head-start used
// ("Kube"/"Media"/"External"). Empty stays empty.
func suggestedCategory(group string) string {
	if group == "" {
		return ""
	}
	return strings.ToUpper(group[:1]) + group[1:]
}

// parseSeed unmarshals the embedded catalog seed into name-sorted entries
// (matching the 0007 cutover's ROW_NUMBER OVER ORDER BY name ordering).
func parseSeed() ([]seedEntry, error) {
	var entries []seedEntry
	if err := json.Unmarshal(seedLibraryJSON, &entries); err != nil {
		return nil, fmt.Errorf("storage: parse catalog seed: %w", err)
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}
