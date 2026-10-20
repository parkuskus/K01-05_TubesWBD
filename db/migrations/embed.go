package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
)

//go:embed *.sql
var files embed.FS

func Apply(ctx context.Context, db *sql.DB) error {
	names, err := fs.Glob(files, "*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		migration, err := files.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read platform migration %s: %w", name, err)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin platform migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, string(migration)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply platform migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit platform migration %s: %w", name, err)
		}
	}
	return nil
}
