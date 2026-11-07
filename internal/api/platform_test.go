package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"if3110.local/db-platform/internal/contracts"
	"if3110.local/db-platform/internal/platform"
)

type platformFixture struct {
	*platform.Service
	calls []string
	err   error
}

func (f *platformFixture) ListUsers(_ context.Context, actor contracts.UserID) ([]platform.User, error) {
	f.calls = append(f.calls, "users:"+string(actor))
	return []platform.User{{ID: "2", Username: "Rina", IsPlatformAdmin: false}}, f.err
}
func (f *platformFixture) ListGroups(context.Context, contracts.UserID) ([]platform.Group, error) {
	f.calls = append(f.calls, "groups")
	return []platform.Group{{ID: "3", Name: "Ops"}}, f.err
}
func (f *platformFixture) SetGroupLeader(_ context.Context, actor contracts.UserID, group contracts.GroupID, leader contracts.UserID) error {
	f.calls = append(f.calls, "leader:"+string(actor)+":"+string(group)+":"+string(leader))
	return f.err
}
func (f *platformFixture) ListMembers(context.Context, contracts.UserID, contracts.GroupID) ([]platform.User, error) {
	f.calls = append(f.calls, "members")
	return nil, f.err
}
func (f *platformFixture) ListGrants(context.Context, contracts.UserID, contracts.GroupID) ([]platform.Grant, error) {
	f.calls = append(f.calls, "grants")
	return []platform.Grant{{GroupID: "3", DatabaseID: "4", Level: contracts.AccessViewEdit}}, f.err
}
func (f *platformFixture) CreateUser(_ context.Context, _ contracts.UserID, name, password string) (contracts.UserID, error) {
	f.calls = append(f.calls, "create-user:"+name+":"+password)
	return "5", f.err
}
func (f *platformFixture) UpdateUsername(context.Context, contracts.UserID, contracts.UserID, string) error {
	f.calls = append(f.calls, "update-user")
	return f.err
}
func (f *platformFixture) SetPassword(context.Context, contracts.UserID, contracts.UserID, string) error {
	f.calls = append(f.calls, "password")
	return f.err
}
func (f *platformFixture) SetPlatformAdmin(context.Context, contracts.UserID, contracts.UserID, bool) error {
	f.calls = append(f.calls, "admin")
	return f.err
}
func (f *platformFixture) DeleteUser(context.Context, contracts.UserID, contracts.UserID) error {
	f.calls = append(f.calls, "delete-user")
	return f.err
}
func (f *platformFixture) CreateGroup(context.Context, contracts.UserID, string) (contracts.GroupID, error) {
	f.calls = append(f.calls, "create-group")
	return "3", f.err
}
func (f *platformFixture) RenameGroup(context.Context, contracts.UserID, contracts.GroupID, string) error {
	f.calls = append(f.calls, "rename-group")
	return f.err
}
func (f *platformFixture) DeleteGroup(context.Context, contracts.UserID, contracts.GroupID) error {
	f.calls = append(f.calls, "delete-group")
	return f.err
}
func (f *platformFixture) SetMembership(_ context.Context, _ contracts.UserID, _ contracts.UserID, _ contracts.GroupID, member bool) error {
	if member {
		f.calls = append(f.calls, "add-member")
	} else {
		f.calls = append(f.calls, "remove-member")
	}
	return f.err
}
func (f *platformFixture) SetGrant(_ context.Context, _ contracts.UserID, _ contracts.GroupID, _ contracts.DatabaseID, level contracts.AccessLevel) error {
	f.calls = append(f.calls, "grant:"+string(rune('0'+level)))
	return f.err
}

type platformAuthz struct{ err error }

func (a platformAuthz) RequirePlatformAdmin(context.Context, contracts.UserID) error { return a.err }
func (a platformAuthz) RequireDatabaseAccess(context.Context, contracts.UserID, contracts.DatabaseID, contracts.DatabaseAction) error {
	return a.err
}

func platformRequest(t *testing.T, f *platformFixture, authz platformAuthz, user contracts.UserID, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	h, err := NewPlatformHandler(f, authz, &testSessions{user: user})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestPlatformAPIReadRoutes(t *testing.T) {
	for _, tc := range []struct{ path, field, value string }{
		{"/api/v1/users", "users", `"username":"Rina"`},
		{"/api/v1/groups", "groups", `"name":"Ops"`},
		{"/api/v1/groups/3/members", "members", `"members":[]`},
		{"/api/v1/groups/3/grants", "grants", `"database_id":"4"`},
	} {
		t.Run(tc.field, func(t *testing.T) {
			f := &platformFixture{}
			w := platformRequest(t, f, platformAuthz{}, "1", "GET", tc.path, "")
			if w.Code != 200 || !strings.Contains(w.Body.String(), tc.value) || strings.Contains(w.Body.String(), "password") {
				t.Fatalf("response %d: %s", w.Code, w.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if _, ok := body["data"].(map[string]any)[tc.field]; !ok {
				t.Fatalf("missing %s", tc.field)
			}
		})
	}
	f := &platformFixture{}
	detail := platformRequest(t, f, platformAuthz{}, "1", "GET", "/api/v1/groups/3", "")
	if detail.Code != 200 || !strings.Contains(detail.Body.String(), `"group":{"id":"3","name":"Ops","leader_user_id":null}`) {
		t.Fatalf("group detail: %d %s", detail.Code, detail.Body.String())
	}
	f = &platformFixture{}
	missing := platformRequest(t, f, platformAuthz{}, "1", "GET", "/api/v1/groups/9", "")
	if missing.Code != 404 {
		t.Fatalf("missing group: %d %s", missing.Code, missing.Body.String())
	}
	f = &platformFixture{}
	grant := platformRequest(t, f, platformAuthz{}, "1", "GET", "/api/v1/groups/3/grants/4", "")
	if grant.Code != 200 || !strings.Contains(grant.Body.String(), `"grant":{"group_id":"3","database_id":"4","level":2}`) {
		t.Fatalf("grant detail: %d %s", grant.Code, grant.Body.String())
	}
	for _, tc := range []struct {
		user  contracts.UserID
		authz platformAuthz
		want  int
	}{{"", platformAuthz{}, 401}, {"2", platformAuthz{contracts.ErrForbidden}, 403}} {
		f := &platformFixture{}
		w := platformRequest(t, f, tc.authz, tc.user, "GET", "/api/v1/users", "")
		if w.Code != tc.want || len(f.calls) != 0 {
			t.Fatalf("denied %d calls=%v", w.Code, f.calls)
		}
	}
}

func TestPlatformAPIGroupLeader(t *testing.T) {
	f := &platformFixture{}
	w := platformRequest(t, f, platformAuthz{}, "1", "PUT", "/api/v1/groups/3/leader", `{"user_id":"5"}`)
	if w.Code != 204 || len(f.calls) != 1 || f.calls[0] != "leader:1:3:5" {
		t.Fatalf("set leader: %d %s calls=%v", w.Code, w.Body.String(), f.calls)
	}
	for _, tc := range []struct {
		path, body string
		want       int
	}{
		{"/api/v1/groups/0/leader", `{"user_id":"5"}`, 400},
		{"/api/v1/groups/3/leader", `{"user_id":"0"}`, 400},
		{"/api/v1/groups/3/leader", `{"user_id":"abc"}`, 400},
		{"/api/v1/groups/3/leader", `{"user_id":"5","actor_id":"1"}`, 400},
		{"/api/v1/groups/3/leader", `{"user_id":null}`, 400},
		{"/api/v1/groups/3/leader", `{}`, 400},
	} {
		f := &platformFixture{}
		w := platformRequest(t, f, platformAuthz{}, "1", "PUT", tc.path, tc.body)
		if w.Code != tc.want || len(f.calls) != 0 {
			t.Fatalf("unsafe leader: %d %s calls=%v", w.Code, w.Body.String(), f.calls)
		}
	}
	for _, tc := range []struct {
		err  error
		want int
	}{{platform.ErrNotFound, 404}, {platform.ErrConflict, 409}, {contracts.ErrForbidden, 403}} {
		f := &platformFixture{err: tc.err}
		w := platformRequest(t, f, platformAuthz{}, "1", "PUT", "/api/v1/groups/3/leader", `{"user_id":"5"}`)
		if w.Code != tc.want {
			t.Fatalf("service error %v: %d", tc.err, w.Code)
		}
	}
	f = &platformFixture{}
	w = platformRequest(t, f, platformAuthz{err: contracts.ErrForbidden}, "5", "PUT", "/api/v1/groups/3/leader", `{"user_id":"5"}`)
	if w.Code != 403 || len(f.calls) != 0 {
		t.Fatalf("non-admin leader: %d calls=%v", w.Code, f.calls)
	}
}

func TestPlatformAPIUserMutations(t *testing.T) {
	for _, tc := range []struct {
		method, path, body, call string
		want                     int
	}{
		{"POST", "/api/v1/users", `{"username":"Rina","password":"secret-pass"}`, "create-user:Rina:secret-pass", 201},
		{"PATCH", "/api/v1/users/5", `{"username":"Nina"}`, "update-user", 204},
		{"PATCH", "/api/v1/users/5", `{"is_platform_admin":false}`, "admin", 204},
		{"PUT", "/api/v1/users/5/password", `{"password":"new-pass"}`, "password", 204},
		{"DELETE", "/api/v1/users/5", "", "delete-user", 204},
	} {
		f := &platformFixture{}
		w := platformRequest(t, f, platformAuthz{}, "1", tc.method, tc.path, tc.body)
		if w.Code != tc.want || len(f.calls) != 1 || f.calls[0] != tc.call {
			t.Fatalf("%s %s: %d %s calls=%v", tc.method, tc.path, w.Code, w.Body.String(), f.calls)
		}
		if strings.Contains(w.Body.String(), "secret-pass") || strings.Contains(w.Body.String(), "new-pass") {
			t.Fatal("password echoed")
		}
	}
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"username":"x","password":"y","actor_id":"1"}`, 400},
		{`null`, 400}, {`{"username":"x","password":"y"}{}`, 400},
		{`{"username":"x","password":"y","username":"z"}`, 400},
		{`{"username":"x","password":"y"}` + strings.Repeat(" ", 17000), 413},
	} {
		f := &platformFixture{}
		w := platformRequest(t, f, platformAuthz{}, "1", "POST", "/api/v1/users", tc.body)
		if w.Code != tc.want || len(f.calls) != 0 {
			t.Fatalf("invalid: %d %s calls=%v", w.Code, w.Body.String(), f.calls)
		}
	}
	for _, tc := range []struct{ path, body string }{{"/api/v1/users/0", `{"username":"x"}`}, {"/api/v1/users/5", `{"username":"x","is_platform_admin":true}`}, {"/api/v1/users/5", `{}`}} {
		f := &platformFixture{}
		w := platformRequest(t, f, platformAuthz{}, "1", "PATCH", tc.path, tc.body)
		if w.Code != 400 || len(f.calls) != 0 {
			t.Fatalf("invalid patch: %d %s", w.Code, w.Body.String())
		}
	}
	for _, tc := range []struct {
		err  error
		want int
	}{{platform.ErrConflict, 409}, {platform.ErrNotFound, 404}, {contracts.ErrForbidden, 403}, {platform.ErrInvalidInput, 422}, {errors.New("db"), 503}} {
		f := &platformFixture{err: tc.err}
		w := platformRequest(t, f, platformAuthz{}, "1", "DELETE", "/api/v1/users/5", "")
		if w.Code != tc.want || strings.Contains(w.Body.String(), "db") {
			t.Fatalf("domain error %d %s", w.Code, w.Body.String())
		}
	}
}

func TestPlatformAPIGroupMembershipAndGrantMutations(t *testing.T) {
	for _, tc := range []struct {
		method, path, body, call string
		want                     int
	}{
		{"POST", "/api/v1/groups", `{"name":"Ops"}`, "create-group", 201},
		{"PATCH", "/api/v1/groups/3", `{"name":"Site"}`, "rename-group", 204},
		{"DELETE", "/api/v1/groups/3", "", "delete-group", 204},
		{"PUT", "/api/v1/groups/3/members/5", "", "add-member", 204},
		{"DELETE", "/api/v1/groups/3/members/5", "", "remove-member", 204},
		{"PUT", "/api/v1/groups/3/grants/4", `{"level":2}`, "grant:2", 204},
		{"DELETE", "/api/v1/groups/3/grants/4", "", "grant:0", 204},
	} {
		f := &platformFixture{}
		w := platformRequest(t, f, platformAuthz{}, "1", tc.method, tc.path, tc.body)
		if w.Code != tc.want || len(f.calls) != 1 || f.calls[0] != tc.call {
			t.Fatalf("%s %s: %d %s calls=%v", tc.method, tc.path, w.Code, w.Body.String(), f.calls)
		}
	}
	for _, body := range []string{`{"level":0}`, `{"level":3}`, `{"level":-1}`, `{"level":null}`, `{"level":2,"actor_id":"1"}`} {
		f := &platformFixture{}
		w := platformRequest(t, f, platformAuthz{}, "1", "PUT", "/api/v1/groups/3/grants/4", body)
		if w.Code < 400 || len(f.calls) != 0 {
			t.Fatalf("invalid grant: %d calls=%v", w.Code, f.calls)
		}
	}
}

func TestPlatformAPIRejectsUnsafeRequestsBeforeMutation(t *testing.T) {
	for _, tc := range []struct {
		method, path, body, contentType string
		want                            int
	}{
		{"POST", "/api/v1/users", `{"username":"X","password":"y"}`, "text/plain", 415},
		{"POST", "/api/v1/users?actor_id=1", `{"username":"X","password":"y"}`, "application/json", 400},
		{"PATCH", "/api/v1/users/5", `{"username":null}`, "application/json", 400},
		{"PUT", "/api/v1/users/5/password", `null`, "application/json", 400},
		{"PUT", "/api/v1/users/5/password", `{"password":null}`, "application/json", 400},
		{"PUT", "/api/v1/groups/3/grants/4", `{"level":2,"level":1}`, "application/json", 400},
		{"PUT", "/api/v1/groups/3/grants/4", `{"level":2}`, "application/json", 204},
		{"PATCH", "/api/v1/users/01", `{"username":"X"}`, "application/json", 400},
		{"POST", "/api/v1/users/5", `{"username":"X"}`, "application/json", 405},
	} {
		f := &platformFixture{}
		h, err := NewPlatformHandler(f, platformAuthz{}, &testSessions{user: "1"})
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		r.Header.Set("Content-Type", tc.contentType)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want || tc.want != 204 && len(f.calls) != 0 {
			t.Fatalf("%s %s: %d, calls=%v, body=%s", tc.method, tc.path, w.Code, f.calls, w.Body.String())
		}
		if tc.want == 405 && w.Header().Get("Allow") == "" {
			t.Fatal("missing Allow header")
		}
	}
	f := &platformFixture{}
	w := platformRequest(t, f, platformAuthz{contracts.ErrForbidden}, "2", "POST", "/api/v1/users", `{"username":"X","password":"y"}`)
	if w.Code != http.StatusForbidden || len(f.calls) != 0 {
		t.Fatalf("nonadmin: %d calls=%v", w.Code, f.calls)
	}
}
