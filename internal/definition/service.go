package definition

import (
	"context"
	"database/sql"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	"if3110.local/db-platform/internal/contracts"
)

type Service struct {
	db         *sql.DB
	authorizer contracts.Authorizer
}

func NewService(db *sql.DB, authorizer contracts.Authorizer) (*Service, error) {
	if db == nil || authorizer == nil {
		return nil, ErrInvalidInput
	}
	return &Service{db: db, authorizer: authorizer}, nil
}

func (s *Service) require(ctx context.Context, actor contracts.UserID, id contracts.DatabaseID, action contracts.DatabaseAction) error {
	if !validID(string(actor)) || !validID(string(id)) {
		return contracts.ErrForbidden
	}
	return s.authorizer.RequireDatabaseAccess(ctx, actor, id, action)
}

func storeError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505", "23514":
			return ErrConflict
		case "23503":
			return ErrNotFound
		}
	}
	return ErrUnavailable
}

const fields = `d.id::text,d.database_id::text,d.connection_revision,d.version,d.title,d.state,d.current_revision,d.created_by::text,d.attempted_at,d.created_at,d.updated_at,r.up_sql,r.down_sql,r.irreversible,r.reason,r.mode,r.batch_plan,r.notes,r.checksum`
const joined = ` FROM migration_definitions d JOIN migration_revisions r ON r.migration_id=d.id AND r.revision=d.current_revision`

type rowScanner interface{ Scan(...any) error }

func scan(row rowScanner) (Definition, error) {
	var d Definition
	err := row.Scan(&d.ID, &d.DatabaseID, &d.ConnectionRevision, &d.Version, &d.Title, &d.State, &d.Revision, &d.CreatedBy, &d.AttemptedAt, &d.CreatedAt, &d.UpdatedAt, &d.UpSQL, &d.DownSQL, &d.Irreversible, &d.Reason, &d.Mode, &d.BatchPlan, &d.Notes, &d.Checksum)
	return d, storeError(err)
}

func (s *Service) Create(ctx context.Context, actor contracts.UserID, id contracts.DatabaseID, in Input) (Definition, error) {
	if err := s.require(ctx, actor, id, contracts.ActionCreateMigration); err != nil {
		return Definition{}, err
	}
	if err := in.Validate(); err != nil {
		return Definition{}, err
	}
	if len(in.BatchPlan) == 0 {
		in.BatchPlan = []byte(`{}`)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Definition{}, storeError(err)
	}
	defer tx.Rollback()
	var connectionRevision, version int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM database_connections WHERE id=$1 FOR UPDATE`, id).Scan(&connectionRevision); err != nil {
		return Definition{}, storeError(err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM migration_definitions WHERE database_id=$1`, id).Scan(&version); err != nil {
		return Definition{}, storeError(err)
	}
	var definitionID string
	if err := tx.QueryRowContext(ctx, `INSERT INTO migration_definitions(database_id,connection_revision,version,title,created_by) VALUES($1,$2,$3,$4,$5) RETURNING id::text`, id, connectionRevision, version, in.Title, actor).Scan(&definitionID); err != nil {
		return Definition{}, storeError(err)
	}
	if err := insertRevision(ctx, tx, definitionID, 1, in, checksum(id, connectionRevision, in)); err != nil {
		return Definition{}, err
	}
	if err := tx.Commit(); err != nil {
		return Definition{}, storeError(err)
	}
	return s.Get(ctx, actor, id, definitionID)
}

func insertRevision(ctx context.Context, tx *sql.Tx, id string, revision int, in Input, hash string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO migration_revisions(migration_id,revision,up_sql,down_sql,irreversible,reason,mode,batch_plan,notes,checksum) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, id, revision, in.UpSQL, in.DownSQL, in.Irreversible, in.Reason, in.Mode, []byte(in.BatchPlan), in.Notes, hash)
	return storeError(err)
}

func (s *Service) Revise(ctx context.Context, actor contracts.UserID, id contracts.DatabaseID, definitionID string, expectedRevision int, in Input) (Definition, error) {
	if err := s.require(ctx, actor, id, contracts.ActionEditMigration); err != nil {
		return Definition{}, err
	}
	if !validID(definitionID) || expectedRevision < 1 || expectedRevision >= 2147483647 {
		return Definition{}, ErrInvalidInput
	}
	if err := in.Validate(); err != nil {
		return Definition{}, err
	}
	if len(in.BatchPlan) == 0 {
		in.BatchPlan = []byte(`{}`)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Definition{}, storeError(err)
	}
	defer tx.Rollback()
	var revision int
	var state string
	var attempted sql.NullTime
	var savedRevision int64
	if err := tx.QueryRowContext(ctx, `SELECT current_revision,state,attempted_at,connection_revision FROM migration_definitions WHERE id=$1 AND database_id=$2 FOR UPDATE`, definitionID, id).Scan(&revision, &state, &attempted, &savedRevision); err != nil {
		return Definition{}, storeError(err)
	}
	if revision != expectedRevision || state != "DRAFT" || attempted.Valid {
		return Definition{}, ErrConflict
	}
	var currentRevision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM database_connections WHERE id=$1`, id).Scan(&currentRevision); err != nil {
		return Definition{}, storeError(err)
	}
	if currentRevision != savedRevision {
		return Definition{}, ErrConflict
	}
	if err := insertRevision(ctx, tx, definitionID, revision+1, in, checksum(id, savedRevision, in)); err != nil {
		return Definition{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE migration_definitions SET title=$2,current_revision=$3,updated_at=NOW() WHERE id=$1`, definitionID, in.Title, revision+1); err != nil {
		return Definition{}, storeError(err)
	}
	if err := tx.Commit(); err != nil {
		return Definition{}, storeError(err)
	}
	return s.Get(ctx, actor, id, definitionID)
}

func (s *Service) List(ctx context.Context, actor contracts.UserID, id contracts.DatabaseID) ([]Definition, error) {
	if err := s.require(ctx, actor, id, contracts.ActionViewMigrationHistory); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+fields+joined+` WHERE d.database_id=$1 ORDER BY d.version`, id)
	if err != nil {
		return nil, storeError(err)
	}
	defer rows.Close()
	out := make([]Definition, 0)
	for rows.Next() {
		d, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, storeError(rows.Err())
}

func (s *Service) Get(ctx context.Context, actor contracts.UserID, id contracts.DatabaseID, definitionID string) (Definition, error) {
	if err := s.require(ctx, actor, id, contracts.ActionViewMigrationHistory); err != nil {
		return Definition{}, err
	}
	if !validID(definitionID) {
		return Definition{}, ErrInvalidInput
	}
	return scan(s.db.QueryRowContext(ctx, `SELECT `+fields+joined+` WHERE d.database_id=$1 AND d.id=$2`, id, definitionID))
}

// MarkAttempted must commit before sending any plan to an executor, including attempts that fail.
func (s *Service) MarkAttempted(ctx context.Context, definitionID string, revision int) error {
	if !validID(definitionID) || revision < 1 {
		return ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return storeError(err)
	}
	defer tx.Rollback()
	var current int
	var attempted sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT current_revision,attempted_at FROM migration_definitions WHERE id=$1 FOR UPDATE`, definitionID).Scan(&current, &attempted); err != nil {
		return storeError(err)
	}
	if current != revision {
		return ErrConflict
	}
	if attempted.Valid {
		return storeError(tx.Commit())
	}
	if _, err := tx.ExecContext(ctx, `UPDATE migration_definitions SET state='LOCKED',attempted_at=NOW(),updated_at=NOW() WHERE id=$1`, definitionID); err != nil {
		return storeError(err)
	}
	return storeError(tx.Commit())
}
