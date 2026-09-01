// Package migrations embeds the schema so a single binary can set up its own
// database. The files are plain SQL and can just as well be run by any other
// migration tool; this package exists so nobody has to.
package migrations

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed *.sql
var files embed.FS

// Apply runs every migration that has not been applied yet, in file order, and
// returns their names. Each file runs as a unit (they wrap themselves in
// BEGIN/COMMIT) and is recorded in sumzero_migrations only after it succeeds.
func Apply(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS sumzero_migrations (
			name       text PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return nil, err
	}

	names, err := fs.Glob(files, "*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)

	var applied []string
	for _, name := range names {
		var done bool
		if err := pool.QueryRow(ctx,
			`SELECT exists(SELECT 1 FROM sumzero_migrations WHERE name = $1)`, name).Scan(&done); err != nil {
			return applied, err
		}
		if done {
			continue
		}
		sql, err := files.ReadFile(name)
		if err != nil {
			return applied, err
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			return applied, fmt.Errorf("%s: %w", name, err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO sumzero_migrations (name) VALUES ($1)`, name); err != nil {
			return applied, err
		}
		applied = append(applied, name)
	}
	return applied, nil
}
