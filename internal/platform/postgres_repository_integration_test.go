package platform_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"if3110.local/db-platform/db/migrations"
	"if3110.local/db-platform/internal/auth"
	"if3110.local/db-platform/internal/contracts"
	"if3110.local/db-platform/internal/platform"
)

func platformTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("PLATFORM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set PLATFORM_TEST_DATABASE_URL in the process to a dedicated platform_test database")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" || parsed.Path != "/platform_test" {
		t.Fatal("PLATFORM_TEST_DATABASE_URL must point to the dedicated platform_test database")
	}

	adminDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal("could not open test database")
	}
	t.Cleanup(func() { _ = adminDB.Close() })
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal("could not generate test schema name")
	}
	schema := "platform_test_" + hex.EncodeToString(suffix[:])
	if _, err := adminDB.ExecContext(context.Background(), `CREATE SCHEMA `+schema); err != nil {
		t.Fatal("could not create isolated test schema")
	}
	t.Cleanup(func() {
		if _, err := adminDB.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Error("could not remove isolated test schema")
		}
	})

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal("could not open test connection")
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(context.Background(), `SET search_path TO `+schema); err != nil {
		t.Fatal("could not set test schema")
	}
	if err := migrations.Apply(context.Background(), db); err != nil {
		t.Fatal("could not apply platform migrations")
	}
	return db
}

func TestRequirePlatformAdminReadsCurrentRole(t *testing.T) {
	db := platformTestDB(t)
	ctx := context.Background()
	login := auth.NewAuthenticator(db)
	if err := login.EnsureInitialAdmin(ctx, "root", "original-password"); err != nil {
		t.Fatal(err)
	}
	root, err := login.Authenticate(ctx, "root", "original-password")
	if err != nil {
		t.Fatal(err)
	}
	s := platform.NewService(db)
	member, err := s.CreateUser(ctx, root, "member", "original-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RequirePlatformAdmin(ctx, member); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("ordinary user: %v", err)
	}
	if err := s.RequirePlatformAdmin(ctx, "999999"); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("missing user: %v", err)
	}
	if err := s.RequirePlatformAdmin(ctx, root); err != nil {
		t.Fatalf("admin: %v", err)
	}
	if err := s.SetPlatformAdmin(ctx, root, member, true); err != nil {
		t.Fatal(err)
	}
	if err := s.RequirePlatformAdmin(ctx, member); err != nil {
		t.Fatalf("promoted user: %v", err)
	}
	if err := s.SetPlatformAdmin(ctx, root, member, false); err != nil {
		t.Fatal(err)
	}
	if err := s.RequirePlatformAdmin(ctx, member); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("demoted user: %v", err)
	}
}

func TestAdminUsersSessionAndLastAdmin(t *testing.T) {
	db := platformTestDB(t)
	ctx := context.Background()
	login := auth.NewAuthenticator(db)
	if err := login.EnsureInitialAdmin(ctx, "root", "original-password"); err != nil {
		t.Fatal(err)
	}
	root, err := login.Authenticate(ctx, "root", "original-password")
	if err != nil {
		t.Fatal(err)
	}
	s := platform.NewService(db)
	if _, err := s.CreateUser(ctx, "99999", "intruder", "password"); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("non-admin: %v", err)
	}
	user, err := s.CreateUser(ctx, root, "  MEMBER  ", "original-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, root, "member", "password"); !errors.Is(err, platform.ErrConflict) {
		t.Fatalf("duplicate: %v", err)
	}
	list, err := s.ListUsers(ctx, root)
	if err != nil || len(list) != 2 || list[1].Username != "member" {
		t.Fatalf("list users: %v (%v)", list, err)
	}
	if _, err := login.Authenticate(ctx, "member", "original-password"); err != nil {
		t.Fatal("new account cannot log in")
	}

	// Sessions deliberately contain only hashed tokens. Revocation must remove both current and grace tokens.
	var sessionID int64
	if err := db.QueryRow(`INSERT INTO sessions (user_id,created_at,last_seen_at,absolute_expires_at,rotated_at) VALUES ($1,now(),now(),now()+interval '12 hours',now()) RETURNING id`, user).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	for _, token := range []byte{1, 2} {
		if _, err := db.Exec(`INSERT INTO session_tokens (token_hash,session_id,valid_until) VALUES ($1,$2,$3)`, bytes.Repeat([]byte{token}, 32), sessionID, time.Now().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetPassword(ctx, root, user, "replacement-password"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM session_tokens WHERE session_id=$1`, sessionID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("tokens after password change: %d, %v", count, err)
	}
	if _, err := login.Authenticate(ctx, "member", "original-password"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("old password works: %v", err)
	}
	if _, err := login.Authenticate(ctx, "member", "replacement-password"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPlatformAdmin(ctx, root, root, false); !errors.Is(err, platform.ErrConflict) {
		t.Fatalf("demote last admin: %v", err)
	}
	if err := s.DeleteUser(ctx, root, root); !errors.Is(err, platform.ErrConflict) {
		t.Fatalf("delete last admin: %v", err)
	}
	if err := s.SetPlatformAdmin(ctx, root, user, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPlatformAdmin(ctx, root, root, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListUsers(ctx, root); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("demoted actor still admin: %v", err)
	}
	if err := s.DeleteUser(ctx, user, root); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(ctx, user, user); !errors.Is(err, platform.ErrConflict) {
		t.Fatalf("delete last remaining admin: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM users WHERE is_platform_admin`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("remaining admins: %d, %v", count, err)
	}
}

func TestConcurrentAdminDemotionKeepsOneAdmin(t *testing.T) {
	db := platformTestDB(t)
	ctx := context.Background()
	login := auth.NewAuthenticator(db)
	if err := login.EnsureInitialAdmin(ctx, "root", "initial-password"); err != nil {
		t.Fatal(err)
	}
	first, err := login.Authenticate(ctx, "root", "initial-password")
	if err != nil {
		t.Fatal(err)
	}
	s := platform.NewService(db)
	second, err := s.CreateUser(ctx, first, "second", "initial-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPlatformAdmin(ctx, first, second, true); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []contracts.UserID{first, second} {
		wg.Add(1)
		go func(id contracts.UserID) { defer wg.Done(); <-start; results <- s.SetPlatformAdmin(ctx, id, id, false) }(id)
	}
	close(start)
	wg.Wait()
	close(results)
	var success, denied int
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, platform.ErrConflict) || errors.Is(err, contracts.ErrForbidden) {
			denied++
		} else {
			t.Fatalf("unexpected demotion error: %v", err)
		}
	}
	var admins int
	if err := db.QueryRow(`SELECT count(*) FROM users WHERE is_platform_admin`).Scan(&admins); err != nil || admins != 1 || success != 1 || denied != 1 {
		t.Fatalf("admins=%d success=%d denied=%d error=%v", admins, success, denied, err)
	}
}

func TestPasswordChangeRollsBackWhenSessionRevocationFails(t *testing.T) {
	db := platformTestDB(t)
	ctx := context.Background()
	login := auth.NewAuthenticator(db)
	if err := login.EnsureInitialAdmin(ctx, "root", "initial-password"); err != nil {
		t.Fatal(err)
	}
	root, err := login.Authenticate(ctx, "root", "initial-password")
	if err != nil {
		t.Fatal(err)
	}
	s := platform.NewService(db)
	user, err := s.CreateUser(ctx, root, "member", "original-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions (user_id,created_at,last_seen_at,absolute_expires_at,rotated_at) VALUES ($1,now(),now(),now()+interval '12 hours',now())`, user); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE FUNCTION fail_revoke() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'test revocation failure'; END $$`); err != nil {
		t.Fatal("could not install test-only trigger function")
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_session_revoke BEFORE UPDATE ON sessions FOR EACH ROW EXECUTE FUNCTION fail_revoke()`); err != nil {
		t.Fatal("could not install test-only trigger")
	}
	if err := s.SetPassword(ctx, root, user, "new-password"); err == nil {
		t.Fatal("revocation failure allowed password change")
	}
	if _, err := login.Authenticate(ctx, "member", "original-password"); err != nil {
		t.Fatalf("failed transaction changed password: %v", err)
	}
	if _, err := login.Authenticate(ctx, "member", "new-password"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("new password accepted after rollback: %v", err)
	}
	var revoked bool
	if err := db.QueryRow(`SELECT revoked_at IS NOT NULL FROM sessions WHERE user_id=$1`, user).Scan(&revoked); err != nil || revoked {
		t.Fatalf("failed transaction changed session: revoked=%t error=%v", revoked, err)
	}
}

func TestLoginProofRejectsSessionAfterPasswordOrRoleChange(t *testing.T) {
	db := platformTestDB(t)
	ctx := context.Background()
	login := auth.NewAuthenticator(db)
	if err := login.EnsureInitialAdmin(ctx, "root", "root-password"); err != nil {
		t.Fatal(err)
	}
	root, err := login.Authenticate(ctx, "root", "root-password")
	if err != nil {
		t.Fatal(err)
	}
	svc := platform.NewService(db)
	user, err := svc.CreateUser(ctx, root, "member", "old-password")
	if err != nil {
		t.Fatal(err)
	}
	proof, err := login.AuthenticateForSession(ctx, "member", "old-password")
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := auth.NewSessionManager(db, auth.SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SetPassword(ctx, root, user, "new-password"); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/login", nil)
	if err := sessions.Create(response, request, proof); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("stale password proof created session: %v", err)
	}
	if len(response.Result().Cookies()) != 0 {
		t.Fatal("failed login issued session cookie")
	}
	proof, err = login.AuthenticateForSession(ctx, "member", "new-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SetPlatformAdmin(ctx, root, user, true); err != nil {
		t.Fatal(err)
	}
	if err := sessions.Create(httptest.NewRecorder(), request, proof); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("stale role proof created session: %v", err)
	}
	proof, err = login.AuthenticateForSession(ctx, "member", "new-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.Create(httptest.NewRecorder(), request, proof); err != nil {
		t.Fatalf("current proof rejected: %v", err)
	}
}

func TestLoginCannotKeepSessionForDeletedUser(t *testing.T) {
	db := platformTestDB(t)
	ctx := context.Background()
	login := auth.NewAuthenticator(db)
	if err := login.EnsureInitialAdmin(ctx, "root", "root-password"); err != nil {
		t.Fatal(err)
	}
	root, err := login.Authenticate(ctx, "root", "root-password")
	if err != nil {
		t.Fatal(err)
	}
	svc := platform.NewService(db)
	sessions, err := auth.NewSessionManager(db, auth.SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/login", nil)

	// Deletion wins between password verification and issuing a session.
	first, err := svc.CreateUser(ctx, root, "first", "user-password")
	if err != nil {
		t.Fatal(err)
	}
	staleProof, err := login.AuthenticateForSession(ctx, "first", "user-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteUser(ctx, root, first); err != nil {
		t.Fatal(err)
	}
	failed := httptest.NewRecorder()
	if err := sessions.Create(failed, request, staleProof); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("deleted user acquired a session: %v", err)
	}
	if len(failed.Result().Cookies()) != 0 {
		t.Fatal("deleted user received a session cookie")
	}

	// Login wins first; deletion must invalidate the issued cookie.
	second, err := svc.CreateUser(ctx, root, "second", "user-password")
	if err != nil {
		t.Fatal(err)
	}
	proof, err := login.AuthenticateForSession(ctx, "second", "user-password")
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	if err := sessions.Create(response, request, proof); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteUser(ctx, root, second); err != nil {
		t.Fatal(err)
	}
	protected := httptest.NewRequest(http.MethodGet, "/private", nil)
	protected.AddCookie(response.Result().Cookies()[0])
	result := httptest.NewRecorder()
	sessions.RequireSession(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("deleted user reached protected handler")
	})).ServeHTTP(result, protected)
	if result.Code != http.StatusUnauthorized {
		t.Fatalf("deleted user's session returned %d, want 401", result.Code)
	}
}

func TestGroupNameRejectsBlankAndCaseInsensitiveDuplicate(t *testing.T) {
	db := platformTestDB(t)
	if _, err := db.Exec(`INSERT INTO groups (name) VALUES ('Editors')`); err != nil {
		t.Fatal("could not insert group")
	}
	for _, name := range []string{"editors", "  "} {
		if _, err := db.Exec(`INSERT INTO groups (name) VALUES ($1)`, name); err == nil {
			t.Errorf("invalid group name was accepted: %q", name)
		}
	}
	if err := migrations.Apply(context.Background(), db); err != nil {
		t.Fatal("reapplying platform migrations failed")
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM groups WHERE name = 'Editors'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("reapplying migrations changed group data: count=%d, error=%v", count, err)
	}
}

func TestGroupMembershipRejectsMissingUserOrGroupAndDuplicatePair(t *testing.T) {
	db := platformTestDB(t)
	var userID, groupID int64
	if err := db.QueryRow(`INSERT INTO users (username, password_hash) VALUES ('member', 'test-hash') RETURNING id`).Scan(&userID); err != nil {
		t.Fatal("could not insert test user")
	}
	if err := db.QueryRow(`INSERT INTO groups (name) VALUES ('Members') RETURNING id`).Scan(&groupID); err != nil {
		t.Fatal("could not insert test group")
	}
	if _, err := db.Exec(`INSERT INTO group_memberships (group_id, user_id) VALUES ($1, $2)`, groupID, userID); err != nil {
		t.Fatal("valid membership was rejected")
	}
	for _, pair := range [][2]int64{{groupID, userID}, {groupID + 1000000, userID}, {groupID, userID + 1000000}} {
		if _, err := db.Exec(`INSERT INTO group_memberships (group_id, user_id) VALUES ($1, $2)`, pair[0], pair[1]); err == nil {
			t.Errorf("duplicate or invalid membership was accepted")
		}
	}
	if _, err := db.Exec(`DELETE FROM users WHERE id = $1`, userID); err != nil {
		t.Fatal("could not delete test user")
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM group_memberships`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("user deletion left memberships: count=%d, error=%v", count, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM groups WHERE id = $1`, groupID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("user deletion removed group: count=%d, error=%v", count, err)
	}
	if err := db.QueryRow(`INSERT INTO users (username, password_hash) VALUES ('another-member', 'test-hash') RETURNING id`).Scan(&userID); err != nil {
		t.Fatal("could not insert second test user")
	}
	if _, err := db.Exec(`INSERT INTO group_memberships (group_id, user_id) VALUES ($1, $2)`, groupID, userID); err != nil {
		t.Fatal("could not insert second membership")
	}
	if _, err := db.Exec(`DELETE FROM groups WHERE id = $1`, groupID); err != nil {
		t.Fatal("could not delete test group")
	}
	if err := db.QueryRow(`SELECT count(*) FROM group_memberships`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("group deletion left memberships: count=%d, error=%v", count, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM users WHERE id = $1`, userID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("group deletion removed user: count=%d, error=%v", count, err)
	}
}

func TestGroupMembershipForeignKeysAndCascade(t *testing.T) {
	db := platformTestDB(t)
	ctx := context.Background()
	login := auth.NewAuthenticator(db)
	if err := login.EnsureInitialAdmin(ctx, "root", "initial-password"); err != nil {
		t.Fatal(err)
	}
	admin, err := login.Authenticate(ctx, "root", "initial-password")
	if err != nil {
		t.Fatal(err)
	}
	s := platform.NewService(db)
	member, err := s.CreateUser(ctx, admin, "member", "member-password")
	if err != nil {
		t.Fatal(err)
	}
	group, err := s.CreateGroup(ctx, admin, "  Editors  ")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateGroup(ctx, admin, "editors"); !errors.Is(err, platform.ErrConflict) {
		t.Fatalf("duplicate group: %v", err)
	}
	if err := s.SetMembership(ctx, member, member, group, true); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("non-admin membership: %v", err)
	}
	if err := s.SetMembership(ctx, admin, "999999", group, true); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("missing user: %v", err)
	}
	if err := s.SetMembership(ctx, admin, member, "999999", false); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("missing group on removal: %v", err)
	}
	if _, err := s.ListMembers(ctx, admin, "999999"); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("missing group on listing: %v", err)
	}
	if err := s.SetMembership(ctx, admin, member, group, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMembership(ctx, admin, member, group, true); err != nil {
		t.Fatalf("duplicate membership should be idempotent: %v", err)
	}
	members, err := s.ListMembers(ctx, admin, group)
	if err != nil || len(members) != 1 || members[0].Username != "member" || members[0].IsPlatformAdmin {
		t.Fatalf("unexpected members: %v, %v", members, err)
	}
	groups, err := s.ListGroups(ctx, admin)
	if err != nil || len(groups) != 1 || groups[0].Name != "Editors" {
		t.Fatalf("unexpected groups: %v, %v", groups, err)
	}
	if err := s.RenameGroup(ctx, admin, group, " Writers "); err != nil {
		t.Fatal(err)
	}
	groups, err = s.ListGroups(ctx, admin)
	if err != nil || len(groups) != 1 || groups[0].Name != "Writers" {
		t.Fatalf("rename did not persist: %v, %v", groups, err)
	}
	if err := s.SetMembership(ctx, admin, member, group, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMembership(ctx, admin, member, group, false); err != nil {
		t.Fatalf("repeated removal: %v", err)
	}
	members, err = s.ListMembers(ctx, admin, group)
	if err != nil || len(members) != 0 {
		t.Fatalf("membership remained: %v, %v", members, err)
	}
}

func TestConcurrentMembershipInsertionUnique(t *testing.T) {
	db := platformTestDB(t)
	ctx := context.Background()
	login := auth.NewAuthenticator(db)
	if err := login.EnsureInitialAdmin(ctx, "root", "initial-password"); err != nil {
		t.Fatal(err)
	}
	admin, err := login.Authenticate(ctx, "root", "initial-password")
	if err != nil {
		t.Fatal(err)
	}
	s := platform.NewService(db)
	user, err := s.CreateUser(ctx, admin, "member", "member-password")
	if err != nil {
		t.Fatal(err)
	}
	group, err := s.CreateGroup(ctx, admin, "Editors")
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() { <-start; results <- s.SetMembership(ctx, admin, user, group, true) }()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("simultaneous membership: %v", err)
		}
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM group_memberships`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("membership count=%d, error=%v", count, err)
	}
}

func TestGroupDeleteRemovesMembershipAndRevokesSession(t *testing.T) {
	db := platformTestDB(t)
	ctx := context.Background()
	login := auth.NewAuthenticator(db)
	if err := login.EnsureInitialAdmin(ctx, "root", "initial-password"); err != nil {
		t.Fatal(err)
	}
	admin, err := login.Authenticate(ctx, "root", "initial-password")
	if err != nil {
		t.Fatal(err)
	}
	s := platform.NewService(db)
	user, err := s.CreateUser(ctx, admin, "member", "member-password")
	if err != nil {
		t.Fatal(err)
	}
	group, err := s.CreateGroup(ctx, admin, "Editors")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMembership(ctx, admin, user, group, true); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions (user_id, created_at, last_seen_at, absolute_expires_at, rotated_at) VALUES ($1, now(), now(), now() + interval '12 hours', now())`, user); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteGroup(ctx, admin, group); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteGroup(ctx, admin, group); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("deleted group was still present: %v", err)
	}
	var members, users, sessions int
	if err := db.QueryRow(`SELECT count(*) FROM group_memberships`).Scan(&members); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM users WHERE id::text = $1`, user).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM sessions WHERE user_id = $1 AND revoked_at IS NULL`, user).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if members != 0 || users != 1 || sessions != 0 {
		t.Fatalf("group deletion left access or deleted user: members=%d users=%d active sessions=%d", members, users, sessions)
	}
}

func TestGrantForeignKeysEffectiveAccessAndRevocation(t *testing.T) {
	db := platformTestDB(t)
	ctx := context.Background()
	login := auth.NewAuthenticator(db)
	if err := login.EnsureInitialAdmin(ctx, "root", "initial-password"); err != nil {
		t.Fatal(err)
	}
	admin, err := login.Authenticate(ctx, "root", "initial-password")
	if err != nil {
		t.Fatal(err)
	}
	s := platform.NewService(db)
	member, err := s.CreateUser(ctx, admin, "member", "member-password")
	if err != nil {
		t.Fatal(err)
	}
	readers, err := s.CreateGroup(ctx, admin, "Readers")
	if err != nil {
		t.Fatal(err)
	}
	editors, err := s.CreateGroup(ctx, admin, "Editors")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMembership(ctx, admin, member, readers, true); err != nil {
		t.Fatal(err)
	}

	var connID int64
	if err := db.QueryRowContext(ctx, `INSERT INTO database_connections (name, host, port, database_name, username, ssl_mode, password_ciphertext) VALUES ('Target 1', 'localhost', 5432, 'target_db', 'postgres', 'disable', $1) RETURNING id`, bytes.Repeat([]byte{1}, 32)).Scan(&connID); err != nil {
		t.Fatal(err)
	}
	dbID := contracts.DatabaseID("1")
	if connID != 1 {
		t.Fatalf("unexpected connection id %d", connID)
	}

	// Non-admin cannot manage grants; missing group or missing database connection returns ErrNotFound.
	if err := s.SetGrant(ctx, member, readers, dbID, contracts.AccessViewOnly); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("non-admin SetGrant: %v", err)
	}
	if err := s.SetGrant(ctx, admin, "999999", dbID, contracts.AccessViewOnly); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("missing group SetGrant: %v", err)
	}
	if err := s.SetGrant(ctx, admin, readers, "999999", contracts.AccessViewOnly); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("missing database SetGrant: %v", err)
	}

	// Set View Only grant on Readers group.
	if err := s.SetGrant(ctx, admin, readers, dbID, contracts.AccessViewOnly); err != nil {
		t.Fatal(err)
	}
	grants, err := s.ListGrants(ctx, admin, readers)
	if err != nil || len(grants) != 1 || grants[0].DatabaseID != dbID || grants[0].Level != contracts.AccessViewOnly {
		t.Fatalf("unexpected grants: %+v, %v", grants, err)
	}

	// Platform Admin without group membership does not receive implicit database access.
	if err := s.RequireDatabaseAccess(ctx, admin, dbID, contracts.ActionViewSchema); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("admin without grant got access: %v", err)
	}

	// Member in Readers has View Only, not View + Edit.
	if err := s.RequireDatabaseAccess(ctx, member, dbID, contracts.ActionViewSchema); err != nil {
		t.Fatalf("member view schema: %v", err)
	}
	if err := s.RequireDatabaseAccess(ctx, member, dbID, contracts.ActionRunMigration); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("view-only member allowed to run migration: %v", err)
	}

	// Adding member to Editors with View + Edit elevates effective access (strongest grant wins).
	if err := s.SetMembership(ctx, admin, member, editors, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetGrant(ctx, admin, editors, dbID, contracts.AccessViewEdit); err != nil {
		t.Fatal(err)
	}
	if err := s.RequireDatabaseAccess(ctx, member, dbID, contracts.ActionRunMigration); err != nil {
		t.Fatalf("view-edit member denied run migration: %v", err)
	}

	// Revoking Editors grant via AccessNone immediately downgrades access on next check.
	if err := s.SetGrant(ctx, admin, editors, dbID, contracts.AccessNone); err != nil {
		t.Fatal(err)
	}
	if err := s.RequireDatabaseAccess(ctx, member, dbID, contracts.ActionRunMigration); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("revoked edit grant still allowed run migration: %v", err)
	}
	if err := s.RequireDatabaseAccess(ctx, member, dbID, contracts.ActionViewData); err != nil {
		t.Fatalf("remaining view grant denied view data: %v", err)
	}

	// Deleting connection cascades grant rows cleanly.
	if _, err := db.ExecContext(ctx, `DELETE FROM database_connections WHERE id = $1`, connID); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM group_database_access`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("orphan grants after connection delete: %d, %v", remaining, err)
	}
}

func TestAccessRevocationInvalidatesSessions(t *testing.T) {
	db := platformTestDB(t)
	ctx := context.Background()
	login := auth.NewAuthenticator(db)
	if err := login.EnsureInitialAdmin(ctx, "root", "initial-password"); err != nil {
		t.Fatal(err)
	}
	admin, err := login.Authenticate(ctx, "root", "initial-password")
	if err != nil {
		t.Fatal(err)
	}
	s := platform.NewService(db)
	member, err := s.CreateUser(ctx, admin, "member", "member-password")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateUser(ctx, admin, "other", "other-password")
	if err != nil {
		t.Fatal(err)
	}
	group, err := s.CreateGroup(ctx, admin, "Editors")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMembership(ctx, admin, member, group, true); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO database_connections (name, host, port, database_name, username, ssl_mode, password_ciphertext) VALUES ('Target', 'localhost', 5432, 'target_db', 'postgres', 'disable', $1)`, bytes.Repeat([]byte{1}, 32)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetGrant(ctx, admin, group, "1", contracts.AccessViewEdit); err != nil {
		t.Fatal(err)
	}
	newSession := func(user contracts.UserID) int64 {
		t.Helper()
		var id int64
		if err := db.QueryRowContext(ctx, `INSERT INTO sessions (user_id,created_at,last_seen_at,absolute_expires_at,rotated_at) VALUES ($1,now(),now(),now()+interval '12 hours',now()) RETURNING id`, user).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO session_tokens (token_hash,session_id) VALUES ($1,$2)`, bytes.Repeat([]byte{byte(id)}, 32), id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	active := func(id int64) bool {
		t.Helper()
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM session_tokens WHERE session_id=$1`, id).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count > 0
	}

	first, unaffected := newSession(member), newSession(other)
	if err := s.SetGrant(ctx, admin, group, "1", contracts.AccessViewOnly); err != nil {
		t.Fatal(err)
	}
	if active(first) || !active(unaffected) {
		t.Fatal("grant downgrade did not revoke only affected member")
	}
	second := newSession(member)
	if err := s.SetGrant(ctx, admin, group, "1", contracts.AccessViewOnly); err != nil || !active(second) {
		t.Fatalf("unchanged grant revoked session: %v", err)
	}
	if err := s.SetGrant(ctx, admin, group, "999999", contracts.AccessViewEdit); !errors.Is(err, platform.ErrNotFound) || !active(second) {
		t.Fatalf("failed grant mutation revoked session: %v", err)
	}
	if err := s.SetMembership(ctx, admin, member, group, false); err != nil {
		t.Fatal(err)
	}
	if active(second) || !active(unaffected) {
		t.Fatal("membership removal did not revoke only affected member")
	}
	if err := s.SetMembership(ctx, admin, member, group, true); err != nil {
		t.Fatal(err)
	}
	third := newSession(member)
	if err := s.SetGrant(ctx, admin, group, "1", contracts.AccessNone); err != nil {
		t.Fatal(err)
	}
	if active(third) || !active(unaffected) {
		t.Fatal("grant deletion did not revoke only affected member")
	}
	if err := s.SetGrant(ctx, admin, group, "1", contracts.AccessViewOnly); err != nil {
		t.Fatal(err)
	}
	elevated := newSession(member)
	if err := s.SetGrant(ctx, admin, group, "1", contracts.AccessViewEdit); err != nil {
		t.Fatal(err)
	}
	if active(elevated) || !active(unaffected) {
		t.Fatal("grant elevation did not rotate by revoking affected session")
	}
	fourth := newSession(member)
	if err := s.DeleteGroup(ctx, admin, group); err != nil {
		t.Fatal(err)
	}
	if active(fourth) || !active(unaffected) {
		t.Fatal("group deletion did not revoke only affected member")
	}
}
