package migrations

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

//go:embed *.sql
var files embed.FS

const (
	advisoryLockKey1 int64 = 0x49463331 // "IF31"
	advisoryLockKey2 int64 = 0x6d696772 // "migr"

	m1BaselineMaxVersion = "0007"
)

var (
	ErrNilDatabase             = errors.New("platform database connection is required")
	ErrChecksumMismatch        = errors.New("platform migration checksum mismatch")
	ErrPendingMigrations       = errors.New("platform database has pending migrations")
	ErrUnknownAppliedMigration = errors.New("platform database contains unknown applied migration")
	ErrOutOfOrderMigration     = errors.New("platform migration history is out of order")
	ErrIncompleteBaseline      = errors.New("existing M1 platform schema is incomplete and cannot be baselined")
)

// MigrationFile represents a single embedded platform SQL migration.
type MigrationFile struct {
	Version  string
	Filename string
	SQL      string
	Checksum string
}

type appliedMigration struct {
	Version  string
	Filename string
	Checksum string
}

var m1BaselineObjects = []string{
	"sessions",
	"session_tokens",
	"users",
	"groups",
	"group_memberships",
	"database_connections",
	"group_database_access",
	"users_username_lower_idx",
}

// LoadEmbedded returns all embedded SQL migrations sorted by version.
func LoadEmbedded() ([]MigrationFile, error) {
	return loadFromFS(files)
}

func loadFromFS(fsys fs.FS) ([]MigrationFile, error) {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)

	out := make([]MigrationFile, 0, len(names))
	seenVersions := make(map[string]string, len(names))
	for _, name := range names {
		version := parseVersion(name)
		if version == "" {
			return nil, fmt.Errorf("invalid migration filename %q: missing version prefix", name)
		}
		if prev, exists := seenVersions[version]; exists {
			return nil, fmt.Errorf("duplicate migration version %q in %q and %q", version, prev, name)
		}
		seenVersions[version] = name

		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("read platform migration %s: %w", name, err)
		}
		normalized := normalizeLineEndings(string(raw))
		sum := sha256.Sum256([]byte(normalized))
		out = append(out, MigrationFile{
			Version:  version,
			Filename: name,
			SQL:      normalized,
			Checksum: hex.EncodeToString(sum[:]),
		})
	}
	return out, nil
}

func parseVersion(filename string) string {
	idx := strings.IndexByte(filename, '_')
	if idx <= 0 {
		return ""
	}
	return strings.TrimSpace(filename[:idx])
}

func normalizeLineEndings(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}

// Apply acquires a session-level PostgreSQL advisory lock, initializes the
// schema_migrations ledger, baselines existing M1 installations (0001-0007)
// without re-running them, verifies checksums of applied migrations, and
// applies any pending migrations in version order.
func Apply(ctx context.Context, db *sql.DB) error {
	return applyWithFS(ctx, db, files)
}

func applyWithFS(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	if db == nil {
		return ErrNilDatabase
	}
	migrations, err := loadFromFS(fsys)
	if err != nil {
		return err
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire platform migration connection: %w", err)
	}
	unlockedCleanly := false
	defer func() {
		if !unlockedCleanly {
			_ = conn.Raw(func(_ any) error {
				return driver.ErrBadConn
			})
		}
		_ = conn.Close()
	}()

	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1, $2)`, advisoryLockKey1, advisoryLockKey2); err != nil {
		return fmt.Errorf("acquire platform migration lock: %w", err)
	}
	defer func() {
		var unlocked bool
		if err := conn.QueryRowContext(context.Background(), `SELECT pg_advisory_unlock($1, $2)`, advisoryLockKey1, advisoryLockKey2).Scan(&unlocked); err == nil && unlocked {
			unlockedCleanly = true
		}
	}()

	if _, err := conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			filename TEXT NOT NULL UNIQUE,
			checksum TEXT NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`); err != nil {
		return fmt.Errorf("ensure schema_migrations table: %w", err)
	}

	applied, err := readAppliedConn(ctx, conn)
	if err != nil {
		return err
	}

	if len(applied) == 0 {
		baselined, err := maybeBaselineM1(ctx, conn, migrations)
		if err != nil {
			return err
		}
		if baselined {
			applied, err = readAppliedConn(ctx, conn)
			if err != nil {
				return err
			}
		}
	}

	embeddedByVersion := make(map[string]MigrationFile, len(migrations))
	for _, m := range migrations {
		embeddedByVersion[m.Version] = m
	}
	for version := range applied {
		if _, ok := embeddedByVersion[version]; !ok {
			return fmt.Errorf("%w: version %s", ErrUnknownAppliedMigration, version)
		}
	}

	seenPending := false
	for _, m := range migrations {
		if rec, isApplied := applied[m.Version]; isApplied {
			if seenPending {
				return fmt.Errorf("%w: applied version %s follows an unapplied migration", ErrOutOfOrderMigration, m.Version)
			}
			if rec.Checksum != m.Checksum {
				return fmt.Errorf("%w for %s: applied %s, embedded %s", ErrChecksumMismatch, m.Filename, rec.Checksum, m.Checksum)
			}
			continue
		}

		seenPending = true
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin platform migration %s: %w", m.Filename, err)
		}
		if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply platform migration %s: %w", m.Filename, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO schema_migrations (version, filename, checksum)
			VALUES ($1, $2, $3)`,
			m.Version, m.Filename, m.Checksum,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record platform migration %s: %w", m.Filename, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit platform migration %s: %w", m.Filename, err)
		}
	}

	return nil
}

// VerifyUpToDate checks that all embedded migrations have been applied to db
// and that their recorded SHA-256 checksums match. It does not execute DDL.
func VerifyUpToDate(ctx context.Context, db *sql.DB) error {
	return verifyUpToDateWithFS(ctx, db, files)
}

func verifyUpToDateWithFS(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	if db == nil {
		return ErrNilDatabase
	}
	migrations, err := loadFromFS(fsys)
	if err != nil {
		return err
	}

	rows, err := db.QueryContext(ctx, `SELECT version, filename, checksum FROM schema_migrations ORDER BY version ASC`)
	if err != nil {
		if isUndefinedTableError(err) {
			return ErrPendingMigrations
		}
		return fmt.Errorf("query schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[string]appliedMigration, len(migrations))
	for rows.Next() {
		var rec appliedMigration
		if err := rows.Scan(&rec.Version, &rec.Filename, &rec.Checksum); err != nil {
			return fmt.Errorf("scan schema_migrations: %w", err)
		}
		applied[rec.Version] = rec
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate schema_migrations: %w", err)
	}

	embeddedByVersion := make(map[string]MigrationFile, len(migrations))
	for _, m := range migrations {
		embeddedByVersion[m.Version] = m
	}
	for version := range applied {
		if _, ok := embeddedByVersion[version]; !ok {
			return fmt.Errorf("%w: version %s", ErrUnknownAppliedMigration, version)
		}
	}

	for _, m := range migrations {
		rec, ok := applied[m.Version]
		if !ok {
			return fmt.Errorf("%w: missing %s", ErrPendingMigrations, m.Filename)
		}
		if rec.Checksum != m.Checksum {
			return fmt.Errorf("%w for %s: applied %s, embedded %s", ErrChecksumMismatch, m.Filename, rec.Checksum, m.Checksum)
		}
	}
	return nil
}

func readAppliedConn(ctx context.Context, conn *sql.Conn) (map[string]appliedMigration, error) {
	rows, err := conn.QueryContext(ctx, `SELECT version, filename, checksum FROM schema_migrations ORDER BY version ASC`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[string]appliedMigration)
	for rows.Next() {
		var rec appliedMigration
		if err := rows.Scan(&rec.Version, &rec.Filename, &rec.Checksum); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		applied[rec.Version] = rec
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schema_migrations: %w", err)
	}
	return applied, nil
}

func maybeBaselineM1(ctx context.Context, conn *sql.Conn, migrations []MigrationFile) (bool, error) {
	existingCount := 0
	for _, obj := range m1BaselineObjects {
		var reg sql.NullString
		if err := conn.QueryRowContext(ctx, `SELECT to_regclass($1)::text`, obj).Scan(&reg); err != nil {
			return false, fmt.Errorf("inspect M1 baseline object %s: %w", obj, err)
		}
		if reg.Valid && reg.String != "" {
			existingCount++
		}
	}

	if existingCount == 0 {
		return false, nil
	}
	if existingCount != len(m1BaselineObjects) {
		return false, fmt.Errorf("%w: found %d of %d expected M1 schema objects", ErrIncompleteBaseline, existingCount, len(m1BaselineObjects))
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin M1 baseline transaction: %w", err)
	}
	for _, m := range migrations {
		if m.Version > m1BaselineMaxVersion {
			break
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO schema_migrations (version, filename, checksum)
			VALUES ($1, $2, $3)`,
			m.Version, m.Filename, m.Checksum,
		); err != nil {
			_ = tx.Rollback()
			return false, fmt.Errorf("record M1 baseline %s: %w", m.Filename, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit M1 baseline: %w", err)
	}
	return true, nil
}

func isUndefinedTableError(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "42P01" || pgErr.Code == "3F000")
}
