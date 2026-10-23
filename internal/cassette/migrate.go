package cassette

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrations holds the forward-only schema migrations in order. Migration i
// (0-based) upgrades a file from schema version i to i+1, so the current
// schema version is len(migrations) (spec §7.6).
var migrations = mustLoadMigrations(migrationFiles)

func mustLoadMigrations(fsys fs.FS) []string {
	out, err := loadMigrations(fsys)
	if err != nil {
		panic(err) // a broken migration set is a build defect; TestEmbeddedMigrationsAreValid catches it
	}
	return out
}

// loadMigrations reads migrations/NNNN_*.sql in order. The number in each name
// must equal its position (0001, 0002, ...): a gap or duplicate would silently
// shift every later schema version, which is a compatibility contract (spec §7.6).
func loadMigrations(fsys fs.FS) ([]string, error) {
	names, err := fs.Glob(fsys, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	out := make([]string, len(names))
	for i, name := range names {
		if want := fmt.Sprintf("migrations/%04d_", i+1); !strings.HasPrefix(name, want) {
			return nil, fmt.Errorf("migration %s: want name prefix %s (numbers must be sequential from 0001)", name, want)
		}
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		out[i] = string(b)
	}
	return out, nil
}

// migrate applies the migrations the file has not seen yet, each in its own
// transaction together with the user_version bump that records it.
func (c *Cassette) migrate(ctx context.Context, all []string) error {
	for v := c.schema; v < len(all); v++ {
		tx, err := c.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("migrating %s to schema v%d: %w", c.path, v+1, err)
		}
		if _, err := tx.ExecContext(ctx, all[v]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migrating %s to schema v%d: %w", c.path, v+1, err)
		}
		// PRAGMA does not accept bound parameters; v+1 is an int we control.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", v+1)); err != nil { //nolint:gosec // G701: see above
			_ = tx.Rollback()
			return fmt.Errorf("recording schema v%d on %s: %w", v+1, c.path, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migrating %s to schema v%d: %w", c.path, v+1, err)
		}
		c.schema = v + 1
	}
	return nil
}
