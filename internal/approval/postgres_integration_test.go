package approval_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"if3110.local/db-platform/db/migrations"
	"if3110.local/db-platform/internal/approval"
	"if3110.local/db-platform/internal/contracts"
	"if3110.local/db-platform/internal/definition"
	"if3110.local/db-platform/internal/jobs"
	"if3110.local/db-platform/internal/platform"
)

type approvalFixture struct {
	db          *sql.DB
	service     *approval.Service
	definitions *definition.Service
	databaseID  contracts.DatabaseID
	users       []contracts.UserID
	groups      []contracts.GroupID
}

type approvalValidatorFunc func(context.Context, contracts.UserID, contracts.DatabaseID, string) (definition.ValidationReport, error)

type approvalEvidence struct {
	head    contracts.ActiveHeadState
	history []contracts.CommitHistoryEvent
}

func (e approvalEvidence) ActiveHead(context.Context, contracts.DatabaseID) (contracts.ActiveHeadState, error) {
	return e.head, nil
}

func (e approvalEvidence) ListCommitHistory(context.Context, contracts.DatabaseID) ([]contracts.CommitHistoryEvent, error) {
	return e.history, nil
}

func (approvalEvidence) BatchProgress(context.Context, contracts.DatabaseID, contracts.ExecutionID) (contracts.BatchCheckpointState, error) {
	return contracts.BatchCheckpointState{}, nil
}

func (f approvalValidatorFunc) Validate(ctx context.Context, actor contracts.UserID, databaseID contracts.DatabaseID, migrationID string) (definition.ValidationReport, error) {
	return f(ctx, actor, databaseID, migrationID)
}

type revokingAuthorizer struct {
	contracts.Authorizer
	db       *sql.DB
	groupID  contracts.GroupID
	database contracts.DatabaseID
	done     bool
}

func (a *revokingAuthorizer) RequireDatabaseAccess(ctx context.Context, actor contracts.UserID, databaseID contracts.DatabaseID, action contracts.DatabaseAction) error {
	if err := a.Authorizer.RequireDatabaseAccess(ctx, actor, databaseID, action); err != nil {
		return err
	}
	if action == contracts.ActionCreateMigration && !a.done {
		a.done = true
		_, err := a.db.ExecContext(ctx, `UPDATE group_database_access SET level=1 WHERE group_id=$1 AND database_id=$2`, a.groupID, a.database)
		return err
	}
	return nil
}

func newApprovalFixture(t *testing.T) *approvalFixture {
	t.Helper()
	dsn := os.Getenv("PLATFORM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set PLATFORM_TEST_DATABASE_URL to a dedicated platform_test database")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Path != "/platform_test" {
		t.Fatal("use dedicated platform_test database")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test database URL")
	}
	admin := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = admin.Close() })
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	schema := "approval_test_" + hex.EncodeToString(suffix[:])
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.ExecContext(context.Background(), "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal("cannot create isolated test schema")
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error("cannot drop isolated test schema")
		}
	})
	cfg.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = db.Close() })
	if err := migrations.Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var users []contracts.UserID
	for _, name := range []string{"submitter", "leader-a", "leader-b", "leader-c", "outsider"} {
		var id int64
		if err := db.QueryRow(`INSERT INTO users(username,password_hash) VALUES($1,'hash') RETURNING id`, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		users = append(users, contracts.UserID(strconv.FormatInt(id, 10)))
	}
	var databaseID int64
	if err := db.QueryRow(`INSERT INTO database_connections(name,host,port,database_name,username,ssl_mode,password_ciphertext) VALUES('target','localhost',5432,'target','tester','disable',$1) RETURNING id`, make([]byte, 29)).Scan(&databaseID); err != nil {
		t.Fatal(err)
	}
	groups := make([]contracts.GroupID, 3)
	for i, name := range []string{"group-a", "group-b", "group-c"} {
		var groupID int64
		if err := db.QueryRow(`INSERT INTO groups(name) VALUES($1) RETURNING id`, name).Scan(&groupID); err != nil {
			t.Fatal(err)
		}
		groups[i] = contracts.GroupID(strconv.FormatInt(groupID, 10))
	}
	p := platform.NewService(db)
	definitions := mustDefinitionService(t, db)
	service, err := approval.NewService(db, p, definitions)
	if err != nil {
		t.Fatal(err)
	}
	return &approvalFixture{db: db, service: service, definitions: definitions, databaseID: contracts.DatabaseID(strconv.FormatInt(databaseID, 10)), users: users, groups: groups}
}

func mustDefinitionService(t *testing.T, db *sql.DB) *definition.Service {
	t.Helper()
	s, err := definition.NewService(db, platform.NewService(db))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (f *approvalFixture) grant(t *testing.T, group contracts.GroupID, member, leader contracts.UserID, level contracts.AccessLevel) {
	t.Helper()
	if _, err := f.db.Exec(`INSERT INTO group_memberships(group_id,user_id) VALUES($1,$2)`, group, member); err != nil {
		t.Fatal(err)
	}
	if leader != "" && leader != member {
		if _, err := f.db.Exec(`INSERT INTO group_memberships(group_id,user_id) VALUES($1,$2)`, group, leader); err != nil {
			t.Fatal(err)
		}
	}
	if leader != "" {
		if _, err := f.db.Exec(`UPDATE groups SET leader_user_id=$2 WHERE id=$1`, group, leader); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.Exec(`INSERT INTO group_database_access(group_id,database_id,level) VALUES($1,$2,$3)`, group, f.databaseID, level); err != nil {
		t.Fatal(err)
	}
}

func (f *approvalFixture) createDefinition(t *testing.T) definition.Definition {
	t.Helper()
	return createDefinitionWithActor(t, f, f.users[0])
}

func createDefinitionWithActor(t *testing.T, f *approvalFixture, actor contracts.UserID) definition.Definition {
	t.Helper()
	d, err := f.definitions.Create(context.Background(), actor, f.databaseID, definition.Input{Title: "Initial", UpSQL: "SELECT 1", DownSQL: "SELECT 2", Mode: "NORMAL"})
	if err != nil {
		t.Fatalf("create definition: %v", err)
	}
	return d
}

func TestPostgresApprovalRequiresEveryGrantedGroup(t *testing.T) {
	f := newApprovalFixture(t)
	f.grant(t, f.groups[0], f.users[0], f.users[1], contracts.AccessViewEdit)
	f.grant(t, f.groups[1], f.users[0], f.users[2], contracts.AccessViewOnly)
	f.grant(t, f.groups[2], f.users[0], f.users[3], contracts.AccessViewEdit)
	d := f.createDefinition(t)
	request, err := f.service.Submit(context.Background(), f.users[0], f.databaseID, d.ID, d.Revision, contracts.DirectionApply, "")
	if err != nil {
		t.Fatal(err)
	}
	if request.Status != "PENDING_REVIEW" || request.Revision != d.Revision || request.Checksum == "" || len(request.Requirements) != 3 || request.Requirements[0].GroupName != "group-a" || request.Requirements[0].LeaderUsername != "leader-a" || request.Migration.UpSQL != d.UpSQL || request.Migration.Notes != d.Notes {
		t.Fatalf("request did not snapshot all grants: %+v", request)
	}
	if _, err := f.db.Exec(`UPDATE operation_requests SET revision=revision+1 WHERE id=$1`, request.ID); err == nil {
		t.Fatal("operation request snapshot was mutable")
	}
	if _, err := f.db.Exec(`UPDATE approval_requirements SET grant_level=1 WHERE request_id=$1 AND group_id=$2`, request.ID, f.groups[0]); err == nil {
		t.Fatal("approval requirement snapshot was mutable")
	}
	for i, user := range []contracts.UserID{f.users[1], f.users[2]} {
		request, err = f.service.Approve(context.Background(), user, request.ID, d.Revision, request.Round, f.groups[i], request.Checksum)
		if err != nil || request.Status != "PENDING_REVIEW" {
			t.Fatalf("request became approved before every group: %+v %v", request, err)
		}
	}
	request, err = f.service.Approve(context.Background(), f.users[3], request.ID, d.Revision, request.Round, f.groups[2], request.Checksum)
	if err != nil || request.Status != "APPROVED" {
		t.Fatalf("last group vote did not approve request: %+v %v", request, err)
	}
	if _, err := f.db.Exec(`DELETE FROM approval_votes WHERE request_id=$1 AND group_id=$2`, request.ID, f.groups[2]); err == nil {
		t.Fatal("approval vote history was mutable")
	}
	request, err = f.service.Approve(context.Background(), f.users[3], request.ID, d.Revision, request.Round, f.groups[2], request.Checksum)
	if err != nil || request.Status != "APPROVED" {
		t.Fatalf("identical duplicate vote was not idempotent: %+v %v", request, err)
	}
	if _, err := f.db.Exec(`UPDATE group_database_access SET level=1 WHERE group_id=$1 AND database_id=$2`, f.groups[0], f.databaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Approve(context.Background(), f.users[3], request.ID, d.Revision, request.Round, f.groups[2], request.Checksum); !errors.Is(err, approval.ErrStale) {
		t.Fatalf("approved request after policy change = %v, want ErrStale", err)
	}
	if got, err := f.service.Get(context.Background(), f.users[0], f.databaseID, request.ID); err != nil || got.Status != "STALE" {
		t.Fatalf("approved request did not become stale: %+v %v", got, err)
	}
}

func TestPostgresRollbackCanEnterReviewFromLockedRevision(t *testing.T) {
	f := newApprovalFixture(t)
	f.grant(t, f.groups[0], f.users[0], f.users[1], contracts.AccessViewEdit)
	d := f.createDefinition(t)
	if err := f.definitions.MarkAttempted(context.Background(), d.ID, d.Revision); err != nil {
		t.Fatal(err)
	}
	request, err := f.service.Submit(context.Background(), f.users[0], f.databaseID, d.ID, d.Revision, contracts.DirectionRollback, "active-execution")
	if err != nil {
		t.Fatalf("submit rollback review: %v", err)
	}
	if request.Status != "PENDING_REVIEW" || request.Direction != contracts.DirectionRollback {
		t.Fatalf("rollback request not submitted for review: %+v", request)
	}
}

func TestPostgresResumeReviewBindsOriginAndRevalidationRequiresGate(t *testing.T) {
	f := newApprovalFixture(t)
	f.grant(t, f.groups[0], f.users[0], f.users[1], contracts.AccessViewEdit)
	d := f.createDefinition(t)
	if err := f.definitions.MarkAttempted(context.Background(), d.ID, d.Revision); err != nil {
		t.Fatal(err)
	}
	request, err := f.service.SubmitWithOrigin(context.Background(), f.users[0], f.databaseID, d.ID, d.Revision, contracts.DirectionResume, "", "exec-partial")
	if err != nil {
		t.Fatalf("submit resume review: %v", err)
	}
	if request.OriginExecutionID != "exec-partial" {
		t.Fatalf("resume origin was not snapshotted: %+v", request)
	}
	request, err = f.service.Approve(context.Background(), f.users[1], request.ID, d.Revision, request.Round, f.groups[0], request.Checksum)
	if err != nil {
		t.Fatal(err)
	}
	head := contracts.ActiveHeadState{UnresolvedExecutionID: "exec-partial", UnresolvedReason: contracts.GateReasonPartial}
	plan, err := f.service.Plan(context.Background(), f.users[0], request.ID, approvalEvidence{head: head}, "exec-resume")
	if err != nil {
		t.Fatalf("plan resume: %v", err)
	}
	if plan.OriginExecutionID != "exec-partial" {
		t.Fatalf("plan origin mismatch: %+v", plan)
	}
	if err := f.service.Revalidate(context.Background(), plan, contracts.ActiveHeadState{}); !errors.Is(err, contracts.ErrExecutionGateBlocked) {
		t.Fatalf("resume without unresolved gate = %v, want blocked", err)
	}
	if err := f.service.Revalidate(context.Background(), plan, head); err != nil {
		t.Fatalf("revalidate resume with matching gate: %v", err)
	}
}

func TestPostgresPlannerSealsApprovedApplyAndRevalidatesLiveSnapshot(t *testing.T) {
	f := newApprovalFixture(t)
	f.grant(t, f.groups[0], f.users[0], f.users[1], contracts.AccessViewEdit)
	d := f.createDefinition(t)
	request, err := f.service.Submit(context.Background(), f.users[0], f.databaseID, d.ID, d.Revision, contracts.DirectionApply, "")
	if err != nil {
		t.Fatal(err)
	}
	request, err = f.service.Approve(context.Background(), f.users[1], request.ID, d.Revision, request.Round, f.groups[0], request.Checksum)
	if err != nil || request.Status != "APPROVED" {
		t.Fatalf("approval: %+v %v", request, err)
	}
	evidence := approvalEvidence{}
	plan, err := f.service.Plan(context.Background(), f.users[0], request.ID, evidence, "exec-1")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Direction != contracts.DirectionApply || plan.MigrationID != contracts.MigrationID(d.ID) || plan.UpSQL != d.UpSQL || plan.DownSQL != d.DownSQL || plan.PlanChecksum != plan.ComputeChecksum() {
		t.Fatalf("plan did not seal approved definition: %+v", plan)
	}
	if err := f.service.Revalidate(context.Background(), plan, contracts.ActiveHeadState{}); err != nil {
		t.Fatalf("revalidate current snapshot: %v", err)
	}
	tampered := plan
	tampered.UpSQL = "DROP TABLE users"
	tampered.PlanChecksum = tampered.ComputeChecksum()
	if err := f.service.Revalidate(context.Background(), tampered, contracts.ActiveHeadState{}); !errors.Is(err, contracts.ErrStaleApproval) {
		t.Fatalf("revalidate modified SQL = %v, want stale approval", err)
	}
	if _, err := f.db.Exec(`UPDATE database_connections SET revision=revision+1 WHERE id=$1`, f.databaseID); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Revalidate(context.Background(), plan, contracts.ActiveHeadState{}); !errors.Is(err, contracts.ErrStaleApproval) {
		t.Fatalf("revalidate after target change = %v, want stale approval", err)
	}
}

func TestPostgresJobPublishRequiresApprovedRequestAndQueuesAtomically(t *testing.T) {
	f := newApprovalFixture(t)
	f.grant(t, f.groups[0], f.users[0], f.users[1], contracts.AccessViewEdit)
	d := f.createDefinition(t)
	request, err := f.service.Submit(context.Background(), f.users[0], f.databaseID, d.ID, d.Revision, contracts.DirectionApply, "")
	if err != nil {
		t.Fatal(err)
	}
	request.Status = "APPROVED"
	request.Requirements[0].Vote = &approval.Vote{ActorID: f.users[1], Decision: "APPROVE"}
	plan, err := approval.BuildExecutionPlan(request, nil, contracts.ActiveHeadState{}, "exec-enqueue")
	if err != nil {
		t.Fatal(err)
	}
	store := jobs.NewPostgresJobStore(f.db)
	if _, err := store.Publish(context.Background(), jobs.PublishInput{Plan: plan, IdempotencyKey: "publish-before-approval"}); !errors.Is(err, contracts.ErrStaleApproval) {
		t.Fatalf("publish pending request = %v, want stale approval", err)
	}
	request, err = f.service.Approve(context.Background(), f.users[1], request.ID, d.Revision, request.Round, f.groups[0], request.Checksum)
	if err != nil || request.Status != "APPROVED" {
		t.Fatalf("approve request: %+v %v", request, err)
	}
	job, err := store.Publish(context.Background(), jobs.PublishInput{Plan: plan, IdempotencyKey: "publish-after-approval"})
	if err != nil || job.Status != contracts.JobStatusPending {
		t.Fatalf("publish approved request: %+v %v", job, err)
	}
	var requestStatus, definitionState string
	if err := f.db.QueryRow(`SELECT o.status,d.state FROM operation_requests o JOIN migration_definitions d ON d.id=o.migration_id WHERE o.id=$1`, request.ID).Scan(&requestStatus, &definitionState); err != nil {
		t.Fatal(err)
	}
	if requestStatus != "QUEUED" || definitionState != "LOCKED" {
		t.Fatalf("publish not atomic: request=%s definition=%s", requestStatus, definitionState)
	}
	replay, err := store.Publish(context.Background(), jobs.PublishInput{Plan: plan, IdempotencyKey: "publish-after-approval"})
	if err != nil || replay.ID != job.ID {
		t.Fatalf("idempotent publish replay: %+v %v", replay, err)
	}
}

func TestPostgresApprovalRejectsMissingLeaderAndSpoofedGroup(t *testing.T) {
	f := newApprovalFixture(t)
	f.grant(t, f.groups[0], f.users[0], f.users[1], contracts.AccessViewEdit)
	f.grant(t, f.groups[1], f.users[0], "", contracts.AccessViewOnly)
	d := f.createDefinition(t)
	if _, err := f.service.Submit(context.Background(), f.users[0], f.databaseID, d.ID, d.Revision, contracts.DirectionApply, ""); !errors.Is(err, approval.ErrMissingLeader) {
		t.Fatalf("submit without a leader = %v, want ErrMissingLeader", err)
	}
	var count int
	if err := f.db.QueryRow(`SELECT count(*) FROM operation_requests`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed submit left a partial request: count=%d err=%v", count, err)
	}
	if _, err := f.db.Exec(`INSERT INTO group_memberships(group_id,user_id) VALUES($1,$2)`, f.groups[1], f.users[2]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE groups SET leader_user_id=$2 WHERE id=$1`, f.groups[1], f.users[2]); err != nil {
		t.Fatal(err)
	}
	request, err := f.service.Submit(context.Background(), f.users[0], f.databaseID, d.ID, d.Revision, contracts.DirectionApply, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Get(context.Background(), f.users[4], f.databaseID, request.ID); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("cross-target request read = %v, want forbidden", err)
	}
	if _, err := f.service.Submit(context.Background(), f.users[4], f.databaseID, d.ID, d.Revision, contracts.DirectionApply, ""); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("submit without grant = %v, want forbidden", err)
	}
	if _, err := f.service.Approve(context.Background(), f.users[1], request.ID, d.Revision, request.Round, f.groups[1], request.Checksum); !errors.Is(err, approval.ErrNotFound) {
		t.Fatalf("leader of another group vote = %v, want hidden request", err)
	}
	if _, err := f.service.Approve(context.Background(), f.users[2], request.ID, d.Revision, request.Round, f.groups[1], strings.Repeat("b", 64)); !errors.Is(err, approval.ErrStale) {
		t.Fatalf("stale checksum vote = %v, want ErrStale", err)
	}
}

func TestPostgresApprovalBecomesStaleAfterPolicyChange(t *testing.T) {
	f := newApprovalFixture(t)
	f.grant(t, f.groups[0], f.users[0], f.users[1], contracts.AccessViewEdit)
	d := f.createDefinition(t)
	request, err := f.service.Submit(context.Background(), f.users[0], f.databaseID, d.ID, d.Revision, contracts.DirectionApply, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE group_database_access SET level=$3 WHERE group_id=$1 AND database_id=$2`, f.groups[0], f.databaseID, contracts.AccessViewOnly); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Approve(context.Background(), f.users[4], request.ID, d.Revision, request.Round, f.groups[0], request.Checksum); !errors.Is(err, approval.ErrNotFound) {
		t.Fatalf("outsider vote on stale request = %v, want hidden request", err)
	}
	stillPending, err := f.service.Get(context.Background(), f.users[0], f.databaseID, request.ID)
	if err != nil || stillPending.Status != "PENDING_REVIEW" {
		t.Fatalf("outsider changed stale request state: %+v %v", stillPending, err)
	}
	if _, err := f.service.Approve(context.Background(), f.users[1], request.ID, d.Revision, request.Round, f.groups[0], request.Checksum); !errors.Is(err, approval.ErrStale) {
		t.Fatalf("vote after policy change = %v, want ErrStale", err)
	}
	got, err := f.service.Get(context.Background(), f.users[0], f.databaseID, request.ID)
	if err != nil || got.Status != "STALE" {
		t.Fatalf("stale request status = %+v, %v", got, err)
	}
	if _, err := f.db.Exec(`UPDATE group_database_access SET level=$3 WHERE group_id=$1 AND database_id=$2`, f.groups[0], f.databaseID, contracts.AccessViewEdit); err != nil {
		t.Fatal(err)
	}
	retry, err := f.service.Submit(context.Background(), f.users[0], f.databaseID, d.ID, d.Revision, contracts.DirectionApply, "")
	if err != nil || retry.Round != 2 || retry.Status != "PENDING_REVIEW" {
		t.Fatalf("fresh request did not preserve and advance review round: %+v %v", retry, err)
	}
}

func TestPostgresOneLeaderMustApproveEachGroup(t *testing.T) {
	f := newApprovalFixture(t)
	f.grant(t, f.groups[0], f.users[0], f.users[1], contracts.AccessViewEdit)
	f.grant(t, f.groups[1], f.users[0], f.users[1], contracts.AccessViewOnly)
	d := f.createDefinition(t)
	request, err := f.service.Submit(context.Background(), f.users[0], f.databaseID, d.ID, d.Revision, contracts.DirectionApply, "")
	if err != nil {
		t.Fatal(err)
	}
	request, err = f.service.Approve(context.Background(), f.users[1], request.ID, d.Revision, request.Round, f.groups[0], request.Checksum)
	if err != nil || request.Status != "PENDING_REVIEW" {
		t.Fatalf("one leader vote auto-approved both groups: %+v %v", request, err)
	}
	request, err = f.service.Approve(context.Background(), f.users[1], request.ID, d.Revision, request.Round, f.groups[1], request.Checksum)
	if err != nil || request.Status != "APPROVED" {
		t.Fatalf("same leader could not satisfy second group explicitly: %+v %v", request, err)
	}
}

func TestPostgresLeaderAndConnectionChangesStaleRequests(t *testing.T) {
	for _, change := range []string{"leader", "connection"} {
		t.Run(change, func(t *testing.T) {
			f := newApprovalFixture(t)
			f.grant(t, f.groups[0], f.users[0], f.users[1], contracts.AccessViewEdit)
			d := f.createDefinition(t)
			request, err := f.service.Submit(context.Background(), f.users[0], f.databaseID, d.ID, d.Revision, contracts.DirectionApply, "")
			if err != nil {
				t.Fatal(err)
			}
			voteLeader := f.users[1]
			if change == "leader" {
				if _, err := f.db.Exec(`INSERT INTO group_memberships(group_id,user_id) VALUES($1,$2)`, f.groups[0], f.users[2]); err != nil {
					t.Fatal(err)
				}
				if _, err := f.db.Exec(`UPDATE groups SET leader_user_id=$2 WHERE id=$1`, f.groups[0], f.users[2]); err != nil {
					t.Fatal(err)
				}
				voteLeader = f.users[2]
			} else {
				if _, err := f.db.Exec(`UPDATE database_connections SET revision=revision+1 WHERE id=$1`, f.databaseID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.service.Approve(context.Background(), voteLeader, request.ID, d.Revision, request.Round, f.groups[0], request.Checksum); !errors.Is(err, approval.ErrStale) {
				t.Fatalf("vote after %s change = %v, want ErrStale", change, err)
			}
		})
	}
}

func TestPostgresSubmitRechecksSubmitterGrantInLockedSnapshot(t *testing.T) {
	f := newApprovalFixture(t)
	f.grant(t, f.groups[0], f.users[0], f.users[1], contracts.AccessViewEdit)
	d := f.createDefinition(t)
	p := platform.NewService(f.db)
	authorizer := &revokingAuthorizer{Authorizer: p, db: f.db, groupID: f.groups[0], database: f.databaseID}
	service, err := approval.NewService(f.db, authorizer, f.definitions)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Submit(context.Background(), f.users[0], f.databaseID, d.ID, d.Revision, contracts.DirectionApply, ""); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("submission after edit grant revocation = %v, want forbidden", err)
	}
	var count int
	if err := f.db.QueryRow(`SELECT count(*) FROM operation_requests`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("revoked submitter created a request: count=%d err=%v", count, err)
	}
}

func TestPostgresSubmitRejectsConnectionChangeAfterValidation(t *testing.T) {
	f := newApprovalFixture(t)
	f.grant(t, f.groups[0], f.users[0], f.users[1], contracts.AccessViewEdit)
	d := f.createDefinition(t)
	validator := approvalValidatorFunc(func(ctx context.Context, actor contracts.UserID, databaseID contracts.DatabaseID, migrationID string) (definition.ValidationReport, error) {
		report, err := f.definitions.Validate(ctx, actor, databaseID, migrationID)
		if err != nil {
			return report, err
		}
		if _, err := f.db.ExecContext(ctx, `UPDATE database_connections SET revision=revision+1 WHERE id=$1`, databaseID); err != nil {
			return definition.ValidationReport{}, err
		}
		return report, nil
	})
	service, err := approval.NewService(f.db, platform.NewService(f.db), validator)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Submit(context.Background(), f.users[0], f.databaseID, d.ID, d.Revision, contracts.DirectionApply, ""); !errors.Is(err, approval.ErrConflict) {
		t.Fatalf("submission after connection revision changed = %v, want conflict", err)
	}
	var count int
	if err := f.db.QueryRow(`SELECT count(*) FROM operation_requests`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("stale validation created a request: count=%d err=%v", count, err)
	}
}

func TestPostgresConcurrentGroupVotesApproveOnceAllPresent(t *testing.T) {
	f := newApprovalFixture(t)
	f.grant(t, f.groups[0], f.users[0], f.users[1], contracts.AccessViewEdit)
	f.grant(t, f.groups[1], f.users[0], f.users[2], contracts.AccessViewOnly)
	d := f.createDefinition(t)
	request, err := f.service.Submit(context.Background(), f.users[0], f.databaseID, d.ID, d.Revision, contracts.DirectionApply, "")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i, actor := range []contracts.UserID{f.users[1], f.users[2]} {
		wg.Add(1)
		go func(group contracts.GroupID, actor contracts.UserID) {
			defer wg.Done()
			_, err := f.service.Approve(context.Background(), actor, request.ID, d.Revision, request.Round, group, request.Checksum)
			errs <- err
		}(f.groups[i], actor)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := f.service.Get(context.Background(), f.users[0], f.databaseID, request.ID)
	if err != nil || got.Status != "APPROVED" || len(got.Requirements) != 2 || got.Requirements[0].Vote == nil || got.Requirements[1].Vote == nil {
		t.Fatalf("concurrent votes lost: %+v %v", got, err)
	}
}

func TestPostgresPolicyMutationSerializesBeforeVote(t *testing.T) {
	f := newApprovalFixture(t)
	f.grant(t, f.groups[0], f.users[0], f.users[1], contracts.AccessViewEdit)
	d := f.createDefinition(t)
	request, err := f.service.Submit(context.Background(), f.users[0], f.databaseID, d.ID, d.Revision, contracts.DirectionApply, "")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var locked int64
	if err := tx.QueryRow(`SELECT version FROM platform_policy_version WHERE id=true FOR UPDATE`).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	voteResult := make(chan error, 1)
	go func() {
		close(started)
		_, err := f.service.Approve(context.Background(), f.users[1], request.ID, d.Revision, request.Round, f.groups[0], request.Checksum)
		voteResult <- err
	}()
	<-started
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		if err := f.db.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE '%platform_policy_version%' AND pid<>pg_backend_pid()`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("vote never waited for the policy snapshot lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := tx.Exec(`UPDATE group_database_access SET level=1 WHERE group_id=$1 AND database_id=$2`, f.groups[0], f.databaseID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-voteResult; !errors.Is(err, approval.ErrStale) {
		t.Fatalf("vote concurrent with policy mutation = %v, want ErrStale", err)
	}
}
