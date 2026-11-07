package approval

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"if3110.local/db-platform/internal/contracts"
	"if3110.local/db-platform/internal/definition"
)

type validator interface {
	Validate(context.Context, contracts.UserID, contracts.DatabaseID, string) (definition.ValidationReport, error)
}

type operationValidator interface {
	ValidateOperation(context.Context, contracts.UserID, contracts.DatabaseID, string, int) (definition.ValidationReport, error)
}

type Service struct {
	db         *sql.DB
	authorizer contracts.Authorizer
	validator  validator
}

type requirementSnapshot struct {
	group  int64
	leader sql.NullInt64
	level  int
}

func NewService(db *sql.DB, authorizer contracts.Authorizer, validator validator) (*Service, error) {
	if db == nil || authorizer == nil || validator == nil {
		return nil, ErrInvalidInput
	}
	return &Service{db: db, authorizer: authorizer, validator: validator}, nil
}

func idValue(raw string) (int64, error) {
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != raw {
		return 0, ErrInvalidInput
	}
	return n, nil
}

func approvalError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505", "23514":
			return ErrConflict
		case "23503":
			return ErrNotFound
		}
	}
	return errors.New("approval storage unavailable")
}

func (s *Service) Submit(ctx context.Context, actor contracts.UserID, databaseID contracts.DatabaseID, migrationID string, revision int, direction contracts.OperationDirection, expectedHead string) (Request, error) {
	return s.SubmitWithOrigin(ctx, actor, databaseID, migrationID, revision, direction, expectedHead, "")
}

func (s *Service) SubmitWithOrigin(ctx context.Context, actor contracts.UserID, databaseID contracts.DatabaseID, migrationID string, revision int, direction contracts.OperationDirection, expectedHead, originExecutionID string) (Request, error) {
	if _, err := idValue(string(actor)); err != nil {
		return Request{}, contracts.ErrForbidden
	}
	dbID, err := idValue(string(databaseID))
	if err != nil {
		return Request{}, contracts.ErrForbidden
	}
	migID, err := idValue(migrationID)
	if err != nil || revision < 1 || !direction.IsValid() || len(expectedHead) > 128 || len(originExecutionID) > 128 {
		return Request{}, ErrInvalidInput
	}
	needsOrigin := direction == contracts.DirectionForward || direction == contracts.DirectionResume || direction == contracts.DirectionCompensate
	if needsOrigin != (strings.TrimSpace(originExecutionID) != "") {
		return Request{}, ErrInvalidInput
	}
	if err := s.authorizer.RequireDatabaseAccess(ctx, actor, databaseID, contracts.ActionCreateMigration); err != nil {
		return Request{}, err
	}
	var report definition.ValidationReport
	if direction == contracts.DirectionApply {
		report, err = s.validator.Validate(ctx, actor, databaseID, migrationID)
	} else if validator, ok := s.validator.(operationValidator); ok {
		report, err = validator.ValidateOperation(ctx, actor, databaseID, migrationID, revision)
	} else {
		return Request{}, ErrInvalidInput
	}
	if err != nil {
		return Request{}, err
	}
	if !report.Valid || report.Revision != revision {
		return Request{}, ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Request{}, approvalError(err)
	}
	defer tx.Rollback()
	var policyVersion, connectionRevision int64
	if err := tx.QueryRowContext(ctx, `SELECT version FROM platform_policy_version WHERE id=true FOR SHARE`).Scan(&policyVersion); err != nil {
		return Request{}, approvalError(err)
	}
	var effectiveAccess int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(a.level),0) FROM group_memberships m JOIN group_database_access a ON a.group_id=m.group_id WHERE m.user_id=$1 AND a.database_id=$2`, actor, dbID).Scan(&effectiveAccess); err != nil {
		return Request{}, approvalError(err)
	}
	if effectiveAccess < int(contracts.AccessViewEdit) {
		return Request{}, contracts.ErrForbidden
	}
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM database_connections WHERE id=$1 FOR SHARE`, dbID).Scan(&connectionRevision); err != nil {
		return Request{}, approvalError(err)
	}
	var state string
	var currentRevision int
	var definitionConnectionRevision int64
	var checksum string
	if err := tx.QueryRowContext(ctx, `SELECT d.state,d.current_revision,d.connection_revision,r.checksum FROM migration_definitions d JOIN migration_revisions r ON r.migration_id=d.id AND r.revision=d.current_revision WHERE d.id=$1 AND d.database_id=$2 FOR UPDATE OF d`, migID, dbID).Scan(&state, &currentRevision, &definitionConnectionRevision, &checksum); err != nil {
		return Request{}, approvalError(err)
	}
	validState := state == "DRAFT" && direction == contracts.DirectionApply || state == "LOCKED" && direction != contracts.DirectionApply
	if !validState || currentRevision != revision || checksum != report.Checksum || definitionConnectionRevision != connectionRevision {
		return Request{}, ErrConflict
	}
	rows, err := tx.QueryContext(ctx, `SELECT g.id,g.leader_user_id,a.level FROM group_database_access a JOIN groups g ON g.id=a.group_id WHERE a.database_id=$1 AND a.level IN (1,2) ORDER BY g.id`, dbID)
	if err != nil {
		return Request{}, approvalError(err)
	}
	requirements := make([]requirementSnapshot, 0)
	for rows.Next() {
		var item requirementSnapshot
		if err := rows.Scan(&item.group, &item.leader, &item.level); err != nil {
			rows.Close()
			return Request{}, approvalError(err)
		}
		if !item.leader.Valid {
			rows.Close()
			return Request{}, ErrMissingLeader
		}
		requirements = append(requirements, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Request{}, approvalError(err)
	}
	rows.Close()
	if len(requirements) == 0 {
		return Request{}, contracts.ErrForbidden
	}
	var round int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(review_round),0)+1 FROM operation_requests WHERE migration_id=$1`, migID).Scan(&round); err != nil {
		return Request{}, approvalError(err)
	}
	var requestID int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO operation_requests(database_id,migration_id,revision,direction,expected_head,origin_execution_id,connection_revision,policy_version,revision_checksum,checksum,status,review_round,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'PENDING_REVIEW',$11,$12) RETURNING id`, dbID, migID, revision, direction, expectedHead, originExecutionID, connectionRevision, policyVersion, checksum, strings.Repeat("0", 64), round, actor).Scan(&requestID); err != nil {
		return Request{}, approvalError(err)
	}
	requestChecksum := checksumRequest(requestID, migID, revision, direction, expectedHead, originExecutionID, connectionRevision, policyVersion, checksum, round, requirements)
	if _, err := tx.ExecContext(ctx, `UPDATE operation_requests SET checksum=$2 WHERE id=$1`, requestID, requestChecksum); err != nil {
		return Request{}, approvalError(err)
	}
	for _, requirement := range requirements {
		if _, err := tx.ExecContext(ctx, `INSERT INTO approval_requirements(request_id,group_id,leader_user_id,grant_level) VALUES($1,$2,$3,$4)`, requestID, requirement.group, requirement.leader.Int64, requirement.level); err != nil {
			return Request{}, approvalError(err)
		}
	}
	if direction == contracts.DirectionApply {
		if _, err := tx.ExecContext(ctx, `UPDATE migration_definitions SET state='IN_REVIEW',updated_at=NOW() WHERE id=$1 AND state='DRAFT'`, migID); err != nil {
			return Request{}, approvalError(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Request{}, approvalError(err)
	}
	return s.Get(ctx, actor, databaseID, contracts.OperationRequestID(strconv.FormatInt(requestID, 10)))
}

func checksumRequest(requestID, migrationID int64, revision int, direction contracts.OperationDirection, expectedHead, originExecutionID string, connectionRevision, policyVersion int64, revisionChecksum string, round int, requirements []requirementSnapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d\x00%d\x00%d\x00%s\x00%s\x00%s\x00%d\x00%d\x00%s\x00%d", requestID, migrationID, revision, direction, expectedHead, originExecutionID, connectionRevision, policyVersion, revisionChecksum, round)
	for _, req := range requirements {
		fmt.Fprintf(&b, "\x00%d:%d:%d", req.group, req.leader.Int64, req.level)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

const requestFields = `id::text,database_id::text,migration_id::text,revision,direction,expected_head,origin_execution_id,connection_revision,policy_version,revision_checksum,checksum,status,review_round,created_by::text,created_at`

func (s *Service) load(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, requestID string) (Request, error) {
	var item Request
	err := q.QueryRowContext(ctx, `SELECT `+requestFields+` FROM operation_requests WHERE id=$1`, requestID).Scan(&item.ID, &item.DatabaseID, &item.MigrationID, &item.Revision, &item.Direction, &item.ExpectedHead, &item.OriginExecutionID, &item.ConnectionRevision, &item.PolicyVersion, &item.RevisionChecksum, &item.Checksum, &item.Status, &item.Round, &item.CreatedBy, &item.CreatedAt)
	if err != nil {
		return Request{}, approvalError(err)
	}
	err = q.QueryRowContext(ctx, `SELECT d.id::text,d.database_id::text,d.connection_revision,d.version,d.title,d.state,o.revision,d.created_by::text,d.attempted_at,d.created_at,d.updated_at,r.up_sql,r.down_sql,r.irreversible,r.reason,r.mode,r.batch_plan,r.notes,r.checksum FROM operation_requests o JOIN migration_definitions d ON d.id=o.migration_id JOIN migration_revisions r ON r.migration_id=o.migration_id AND r.revision=o.revision WHERE o.id=$1`, requestID).Scan(&item.Migration.ID, &item.Migration.DatabaseID, &item.Migration.ConnectionRevision, &item.Migration.Version, &item.Migration.Title, &item.Migration.State, &item.Migration.Revision, &item.Migration.CreatedBy, &item.Migration.AttemptedAt, &item.Migration.CreatedAt, &item.Migration.UpdatedAt, &item.Migration.UpSQL, &item.Migration.DownSQL, &item.Migration.Irreversible, &item.Migration.Reason, &item.Migration.Mode, &item.Migration.BatchPlan, &item.Migration.Notes, &item.Migration.Checksum)
	if err != nil {
		return Request{}, approvalError(err)
	}
	rows, err := q.QueryContext(ctx, `SELECT r.group_id::text,g.name,r.leader_user_id::text,u.username,r.grant_level,v.actor_id::text,v.decision,v.created_at FROM approval_requirements r JOIN groups g ON g.id=r.group_id JOIN users u ON u.id=r.leader_user_id LEFT JOIN approval_votes v ON v.request_id=r.request_id AND v.group_id=r.group_id WHERE r.request_id=$1 ORDER BY r.group_id`, requestID)
	if err != nil {
		return Request{}, approvalError(err)
	}
	defer rows.Close()
	item.Requirements = make([]Requirement, 0)
	for rows.Next() {
		var req Requirement
		var actor, decision sql.NullString
		var votedAt sql.NullTime
		if err := rows.Scan(&req.GroupID, &req.GroupName, &req.LeaderUserID, &req.LeaderUsername, &req.GrantLevel, &actor, &decision, &votedAt); err != nil {
			return Request{}, approvalError(err)
		}
		if actor.Valid {
			req.Vote = &Vote{ActorID: contracts.UserID(actor.String), Decision: decision.String, CreatedAt: votedAt.Time}
		}
		item.Requirements = append(item.Requirements, req)
	}
	return item, approvalError(rows.Err())
}

func (s *Service) List(ctx context.Context, actor contracts.UserID, databaseID contracts.DatabaseID) ([]Request, error) {
	if err := s.authorizer.RequireDatabaseAccess(ctx, actor, databaseID, contracts.ActionViewMigrationHistory); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id::text FROM operation_requests WHERE database_id=$1 ORDER BY created_at DESC,id DESC`, databaseID)
	if err != nil {
		return nil, approvalError(err)
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, approvalError(err)
		}
		item, err := s.load(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, approvalError(rows.Err())
}

func (s *Service) Get(ctx context.Context, actor contracts.UserID, databaseID contracts.DatabaseID, requestID contracts.OperationRequestID) (Request, error) {
	if err := s.authorizer.RequireDatabaseAccess(ctx, actor, databaseID, contracts.ActionViewMigrationHistory); err != nil {
		return Request{}, err
	}
	id, err := idValue(string(requestID))
	if err != nil {
		return Request{}, ErrNotFound
	}
	item, err := s.load(ctx, s.db, strconv.FormatInt(id, 10))
	if err != nil {
		return Request{}, err
	}
	if item.DatabaseID != databaseID {
		return Request{}, ErrNotFound
	}
	return item, nil
}

func (s *Service) Approve(ctx context.Context, actor contracts.UserID, requestID contracts.OperationRequestID, revision, round int, groupID contracts.GroupID, checksum string) (Request, error) {
	request, err := idValue(string(requestID))
	if err != nil {
		return Request{}, ErrNotFound
	}
	group, err := idValue(string(groupID))
	if err != nil || revision < 1 || round < 1 {
		return Request{}, ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Request{}, approvalError(err)
	}
	defer tx.Rollback()
	var item Request
	err = tx.QueryRowContext(ctx, `SELECT `+requestFields+` FROM operation_requests WHERE id=$1 FOR UPDATE`, request).Scan(&item.ID, &item.DatabaseID, &item.MigrationID, &item.Revision, &item.Direction, &item.ExpectedHead, &item.OriginExecutionID, &item.ConnectionRevision, &item.PolicyVersion, &item.RevisionChecksum, &item.Checksum, &item.Status, &item.Round, &item.CreatedBy, &item.CreatedAt)
	if err != nil {
		return Request{}, approvalError(err)
	}
	var liveLeader int64
	var isMember, hasGrant bool
	err = tx.QueryRowContext(ctx, `SELECT g.leader_user_id,EXISTS(SELECT 1 FROM group_memberships m WHERE m.group_id=g.id AND m.user_id=$3),EXISTS(SELECT 1 FROM group_database_access a WHERE a.group_id=g.id AND a.database_id=$4 AND a.level IN (1,2)) FROM approval_requirements r JOIN groups g ON g.id=r.group_id WHERE r.request_id=$1 AND r.group_id=$2`, request, group, actor, item.DatabaseID).Scan(&liveLeader, &isMember, &hasGrant)
	if errors.Is(err, sql.ErrNoRows) {
		return Request{}, ErrNotFound
	}
	if err != nil {
		return Request{}, approvalError(err)
	}
	if !isMember || !hasGrant || strconv.FormatInt(liveLeader, 10) != string(actor) {
		return Request{}, ErrNotFound
	}
	if len(checksum) != 64 {
		return Request{}, ErrInvalidInput
	}
	if revision != item.Revision || round != item.Round || checksum != item.Checksum {
		return Request{}, ErrStale
	}
	if item.Status != "PENDING_REVIEW" && item.Status != "APPROVED" {
		return Request{}, ErrConflict
	}
	var livePolicy, liveConnection int64
	if err := tx.QueryRowContext(ctx, `SELECT version FROM platform_policy_version WHERE id=true FOR SHARE`).Scan(&livePolicy); err != nil {
		return Request{}, approvalError(err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM database_connections WHERE id=$1 FOR SHARE`, item.DatabaseID).Scan(&liveConnection); err != nil {
		return Request{}, approvalError(err)
	}
	var state string
	var currentRevision int
	var liveChecksum string
	if err := tx.QueryRowContext(ctx, `SELECT d.state,d.current_revision,r.checksum FROM migration_definitions d JOIN migration_revisions r ON r.migration_id=d.id AND r.revision=d.current_revision WHERE d.id=$1 FOR UPDATE OF d`, item.MigrationID).Scan(&state, &currentRevision, &liveChecksum); err != nil {
		return Request{}, approvalError(err)
	}
	var requirements []requirementSnapshot
	rows, err := tx.QueryContext(ctx, `SELECT group_id,leader_user_id,grant_level FROM approval_requirements WHERE request_id=$1 ORDER BY group_id`, request)
	if err != nil {
		return Request{}, approvalError(err)
	}
	for rows.Next() {
		var req requirementSnapshot
		if err := rows.Scan(&req.group, &req.leader, &req.level); err != nil {
			rows.Close()
			return Request{}, approvalError(err)
		}
		requirements = append(requirements, req)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Request{}, approvalError(err)
	}
	rows.Close()
	liveRequirements, err := tx.QueryContext(ctx, `SELECT g.id,g.leader_user_id,a.level FROM group_database_access a JOIN groups g ON g.id=a.group_id WHERE a.database_id=$1 AND a.level IN (1,2) ORDER BY g.id`, item.DatabaseID)
	if err != nil {
		return Request{}, approvalError(err)
	}
	live := make([]requirementSnapshot, 0)
	for liveRequirements.Next() {
		var req requirementSnapshot
		if err := liveRequirements.Scan(&req.group, &req.leader, &req.level); err != nil {
			liveRequirements.Close()
			return Request{}, approvalError(err)
		}
		live = append(live, req)
	}
	if err := liveRequirements.Err(); err != nil {
		liveRequirements.Close()
		return Request{}, approvalError(err)
	}
	liveRequirements.Close()
	validState := state == "IN_REVIEW" && item.Direction == contracts.DirectionApply || state == "LOCKED" && item.Direction != contracts.DirectionApply
	stale := livePolicy != item.PolicyVersion || liveConnection != item.ConnectionRevision || !validState || currentRevision != item.Revision || liveChecksum != item.RevisionChecksum || len(live) != len(requirements)
	for i := range live {
		if i >= len(requirements) || live[i] != requirements[i] {
			stale = true
			break
		}
	}
	if checksumRequest(request, mustID(string(item.MigrationID)), item.Revision, item.Direction, string(item.ExpectedHead), string(item.OriginExecutionID), item.ConnectionRevision, item.PolicyVersion, item.RevisionChecksum, item.Round, requirements) != item.Checksum {
		stale = true
	}
	if stale {
		if _, err := tx.ExecContext(ctx, `UPDATE operation_requests SET status='STALE',updated_at=NOW() WHERE id=$1`, request); err != nil {
			return Request{}, approvalError(err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE migration_definitions SET state='DRAFT',updated_at=NOW() WHERE id=$1 AND state='IN_REVIEW'`, item.MigrationID); err != nil {
			return Request{}, approvalError(err)
		}
		if err := tx.Commit(); err != nil {
			return Request{}, approvalError(err)
		}
		return Request{}, ErrStale
	}
	var priorActor string
	err = tx.QueryRowContext(ctx, `SELECT actor_id::text FROM approval_votes WHERE request_id=$1 AND review_round=$2 AND group_id=$3`, request, round, group).Scan(&priorActor)
	if err == nil {
		if priorActor != string(actor) {
			return Request{}, ErrConflict
		}
		if err := tx.Commit(); err != nil {
			return Request{}, approvalError(err)
		}
		return s.Get(ctx, actor, item.DatabaseID, item.ID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Request{}, approvalError(err)
	}
	if item.Status != "PENDING_REVIEW" {
		return Request{}, ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO approval_votes(request_id,review_round,group_id,migration_revision,checksum,actor_id,decision) VALUES($1,$2,$3,$4,$5,$6,'APPROVE')`, request, round, group, revision, checksum, actor); err != nil {
		return Request{}, approvalError(err)
	}
	var voteCount, requirementCount int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM approval_votes WHERE request_id=$1 AND review_round=$2),(SELECT count(*) FROM approval_requirements WHERE request_id=$1)`, request, round).Scan(&voteCount, &requirementCount); err != nil {
		return Request{}, approvalError(err)
	}
	if voteCount == requirementCount {
		if _, err := tx.ExecContext(ctx, `UPDATE operation_requests SET status='APPROVED',updated_at=NOW() WHERE id=$1 AND status='PENDING_REVIEW'`, request); err != nil {
			return Request{}, approvalError(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Request{}, approvalError(err)
	}
	return s.Get(ctx, actor, item.DatabaseID, item.ID)
}

func mustID(raw string) int64 {
	n, _ := strconv.ParseInt(raw, 10, 64)
	return n
}

func (s *Service) GetByID(ctx context.Context, actor contracts.UserID, requestID contracts.OperationRequestID) (Request, error) {
	id, err := idValue(string(requestID))
	if err != nil {
		return Request{}, ErrNotFound
	}
	var databaseID string
	if err := s.db.QueryRowContext(ctx, `SELECT database_id::text FROM operation_requests WHERE id=$1`, id).Scan(&databaseID); err != nil {
		return Request{}, approvalError(err)
	}
	return s.Get(ctx, actor, contracts.DatabaseID(databaseID), requestID)
}

// Plan returns a checksum-sealed plan only for a currently approved request and
// the supplied read-only target evidence.
func (s *Service) Plan(ctx context.Context, actor contracts.UserID, requestID contracts.OperationRequestID, evidence contracts.CommitEvidenceReader, executionID contracts.ExecutionID) (contracts.ExecutionPlan, error) {
	if evidence == nil {
		return contracts.ExecutionPlan{}, ErrInvalidInput
	}
	request, err := s.GetByID(ctx, actor, requestID)
	if err != nil {
		return contracts.ExecutionPlan{}, err
	}
	if request.CreatedBy != actor {
		return contracts.ExecutionPlan{}, contracts.ErrForbidden
	}
	if err := s.authorizer.RequireDatabaseAccess(ctx, actor, request.DatabaseID, contracts.ActionCreateMigration); err != nil {
		return contracts.ExecutionPlan{}, err
	}
	if err := s.checkLiveSnapshot(ctx, request); err != nil {
		return contracts.ExecutionPlan{}, err
	}
	head, err := evidence.ActiveHead(ctx, request.DatabaseID)
	if err != nil {
		return contracts.ExecutionPlan{}, err
	}
	history, err := evidence.ListCommitHistory(ctx, request.DatabaseID)
	if err != nil {
		return contracts.ExecutionPlan{}, err
	}
	return BuildExecutionPlan(request, history, head, executionID)
}

// Revalidate checks the immutable request snapshot against live platform state
// immediately before execution while the worker holds the target lock.
func (s *Service) Revalidate(ctx context.Context, plan contracts.ExecutionPlan, currentHead contracts.ActiveHeadState) error {
	if err := plan.Validate(); err != nil {
		return err
	}
	request, err := s.load(ctx, s.db, string(plan.RequestID))
	if err != nil {
		return err
	}
	if request.Status != "APPROVED" && request.Status != "QUEUED" || request.ID != plan.RequestID || request.DatabaseID != plan.TargetDatabaseID || request.Migration.ID != string(plan.MigrationID) || plan.RevisionID != contracts.RevisionID(fmt.Sprintf("%s:%d", request.Migration.ID, request.Revision)) || request.Checksum == "" || request.CreatedBy != plan.ActorID || request.Direction != plan.Direction || request.OriginExecutionID != plan.OriginExecutionID || request.Migration.Version != plan.Version || contracts.ExecutionMode(request.Migration.Mode) != plan.Mode || request.Migration.UpSQL != plan.UpSQL || request.Migration.DownSQL != plan.DownSQL || request.Migration.Irreversible != plan.IsIrreversible || request.Migration.Reason != plan.IrreversibleReason {
		return contracts.ErrStaleApproval
	}
	if plan.PlanChecksum != plan.ComputeChecksum() || plan.ExpectedActiveHead != request.ExpectedHead || plan.PolicyVersion != request.PolicyVersion || plan.TargetConnectionRevision != request.ConnectionRevision {
		return contracts.ErrStaleApproval
	}
	if err := s.checkLiveSnapshot(ctx, request); err != nil {
		return err
	}
	if currentHead.ExecutionID != plan.ExpectedActiveHead {
		return contracts.ErrHeadMismatch
	}
	isRecovery := plan.Direction == contracts.DirectionResume || plan.Direction == contracts.DirectionCompensate
	if isRecovery {
		if currentHead.UnresolvedExecutionID == "" || currentHead.UnresolvedExecutionID != plan.OriginExecutionID || currentHead.UnresolvedReason != contracts.GateReasonPartial && currentHead.UnresolvedReason != contracts.GateReasonPartiallyReverted {
			return contracts.ErrExecutionGateBlocked
		}
	} else if currentHead.UnresolvedReason != "" {
		return contracts.ErrExecutionGateBlocked
	}
	return nil
}

func (s *Service) checkLiveSnapshot(ctx context.Context, request Request) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return approvalError(err)
	}
	defer tx.Rollback()
	var policy, connection int64
	var status string
	var revision int
	var checksum string
	err = tx.QueryRowContext(ctx, `SELECT version FROM platform_policy_version WHERE id=true FOR SHARE`).Scan(&policy)
	if err == nil {
		err = tx.QueryRowContext(ctx, `SELECT revision FROM database_connections WHERE id=$1 FOR SHARE`, request.DatabaseID).Scan(&connection)
	}
	if err == nil {
		err = tx.QueryRowContext(ctx, `SELECT d.state,d.current_revision,r.checksum FROM migration_definitions d JOIN migration_revisions r ON r.migration_id=d.id AND r.revision=d.current_revision WHERE d.database_id=$1 AND d.id=$2 FOR SHARE OF d`, request.DatabaseID, request.MigrationID).Scan(&status, &revision, &checksum)
	}
	if err != nil {
		return approvalError(err)
	}
	if policy != request.PolicyVersion || connection != request.ConnectionRevision || revision != request.Revision || checksum != request.RevisionChecksum || status != "IN_REVIEW" && status != "LOCKED" {
		return contracts.ErrStaleApproval
	}
	requirements := make([]requirementSnapshot, 0, len(request.Requirements))
	for _, item := range request.Requirements {
		groupID, groupErr := idValue(string(item.GroupID))
		leaderID, leaderErr := idValue(string(item.LeaderUserID))
		if groupErr != nil || leaderErr != nil {
			return contracts.ErrStaleApproval
		}
		requirements = append(requirements, requirementSnapshot{group: groupID, leader: sql.NullInt64{Int64: leaderID, Valid: true}, level: int(item.GrantLevel)})
	}
	if checksumRequest(mustID(string(request.ID)), mustID(string(request.MigrationID)), request.Revision, request.Direction, string(request.ExpectedHead), string(request.OriginExecutionID), request.ConnectionRevision, request.PolicyVersion, request.RevisionChecksum, request.Round, requirements) != request.Checksum {
		return contracts.ErrStaleApproval
	}
	var missing int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM approval_requirements r JOIN operation_requests o ON o.id=r.request_id LEFT JOIN approval_votes v ON v.request_id=r.request_id AND v.review_round=o.review_round AND v.group_id=r.group_id WHERE r.request_id=$1 AND (v.actor_id IS NULL OR v.actor_id<>r.leader_user_id OR v.decision<>'APPROVE' OR v.migration_revision<>o.revision OR v.checksum<>o.checksum)`, request.ID).Scan(&missing); err != nil {
		return approvalError(err)
	}
	if missing != 0 {
		return contracts.ErrStaleApproval
	}
	return approvalError(tx.Commit())
}
