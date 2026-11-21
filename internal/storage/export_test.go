package storage

import (
	"context"
	"io/fs"
)

// MigrateFS runs the migration runner over fsys instead of the embedded
// files, so a test can feed it a migration that fails half way.
func (s *Store) MigrateFS(ctx context.Context, fsys fs.FS) (MigrateReport, error) {
	return s.migrate(ctx, fsys)
}
