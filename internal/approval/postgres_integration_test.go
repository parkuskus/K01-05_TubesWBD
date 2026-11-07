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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"if3110.local/db-platform/db/migrations"
	"if3110.local/db-platform/internal/approval"
	"if3110.local/db-platform/internal/contracts"
	"if3110.local/db-platform/internal/definition"
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
	if _, err := f.service.Approve(context.Background(), f.users[1], request.ID, d.Revision, request.Round, f.groups[1], request.Checksum); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("leader of another group voted = %v, want forbidden", err)
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
			if change == "leader" {
				if _, err := f.db.Exec(`INSERT INTO group_memberships(group_id,user_id) VALUES($1,$2)`, f.groups[0], f.users[2]); err != nil {
					t.Fatal(err)
				}
				if _, err := f.db.Exec(`UPDATE groups SET leader_user_id=$2 WHERE id=$1`, f.groups[0], f.users[2]); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := f.db.Exec(`UPDATE database_connections SET revision=revision+1 WHERE id=$1`, f.databaseID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.service.Approve(context.Background(), f.users[1], request.ID, d.Revision, request.Round, f.groups[0], request.Checksum); !errors.Is(err, approval.ErrStale) {
				t.Fatalf("vote after %s change = %v, want ErrStale", change, err)
			}
		})
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
