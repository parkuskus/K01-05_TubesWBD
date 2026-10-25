package platformui

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"if3110.local/db-platform/internal/auth"
	"if3110.local/db-platform/internal/contracts"
	"if3110.local/db-platform/internal/platform"
	"if3110.local/db-platform/internal/ui"
)

type adminFake struct {
	users   []platform.User
	groups  []platform.Group
	members map[contracts.GroupID][]platform.User
	grants  map[contracts.GroupID][]platform.Grant
	calls   int
	err     error
	actor   contracts.UserID
}

func (f *adminFake) changed() error { f.calls++; return f.err }
func (f *adminFake) ListUsers(context.Context, contracts.UserID) ([]platform.User, error) {
	return f.users, f.err
}
func (f *adminFake) CreateUser(_ context.Context, actor contracts.UserID, name, password string) (contracts.UserID, error) {
	f.actor = actor
	if err := f.changed(); err != nil {
		return "", err
	}
	f.users = append(f.users, platform.User{ID: "2", Username: name})
	return "2", nil
}
func (f *adminFake) UpdateUsername(_ context.Context, _, id contracts.UserID, name string) error {
	if err := f.changed(); err != nil {
		return err
	}
	for i := range f.users {
		if f.users[i].ID == id {
			f.users[i].Username = name
		}
	}
	return nil
}
func (f *adminFake) SetPassword(context.Context, contracts.UserID, contracts.UserID, string) error {
	return f.changed()
}
func (f *adminFake) SetPlatformAdmin(_ context.Context, _, id contracts.UserID, admin bool) error {
	if err := f.changed(); err != nil {
		return err
	}
	for i := range f.users {
		if f.users[i].ID == id {
			f.users[i].IsPlatformAdmin = admin
		}
	}
	return nil
}
func (f *adminFake) DeleteUser(_ context.Context, _, id contracts.UserID) error {
	if err := f.changed(); err != nil {
		return err
	}
	for i, u := range f.users {
		if u.ID == id {
			f.users = append(f.users[:i], f.users[i+1:]...)
			break
		}
	}
	return nil
}
func (f *adminFake) ListGroups(context.Context, contracts.UserID) ([]platform.Group, error) {
	return f.groups, f.err
}
func (f *adminFake) CreateGroup(_ context.Context, _ contracts.UserID, name string) (contracts.GroupID, error) {
	if err := f.changed(); err != nil {
		return "", err
	}
	f.groups = append(f.groups, platform.Group{ID: "4", Name: name})
	return "4", nil
}
func (f *adminFake) RenameGroup(_ context.Context, _ contracts.UserID, id contracts.GroupID, name string) error {
	if err := f.changed(); err != nil {
		return err
	}
	for i := range f.groups {
		if f.groups[i].ID == id {
			f.groups[i].Name = name
		}
	}
	return nil
}
func (f *adminFake) DeleteGroup(_ context.Context, _ contracts.UserID, id contracts.GroupID) error {
	if err := f.changed(); err != nil {
		return err
	}
	for i, g := range f.groups {
		if g.ID == id {
			f.groups = append(f.groups[:i], f.groups[i+1:]...)
			break
		}
	}
	return nil
}
func (f *adminFake) ListMembers(_ context.Context, _ contracts.UserID, group contracts.GroupID) ([]platform.User, error) {
	return f.members[group], f.err
}
func (f *adminFake) SetMembership(_ context.Context, actor, user contracts.UserID, group contracts.GroupID, member bool) error {
	f.actor = actor
	if err := f.changed(); err != nil {
		return err
	}
	if member {
		f.members[group] = append(f.members[group], platform.User{ID: user, Username: "added"})
	} else {
		f.members[group] = nil
	}
	return nil
}
func (f *adminFake) ListGrants(_ context.Context, _ contracts.UserID, group contracts.GroupID) ([]platform.Grant, error) {
	return f.grants[group], f.err
}
func (f *adminFake) SetGrant(_ context.Context, actor contracts.UserID, group contracts.GroupID, db contracts.DatabaseID, level contracts.AccessLevel) error {
	f.actor = actor
	if err := f.changed(); err != nil {
		return err
	}
	if level == contracts.AccessNone {
		f.grants[group] = nil
	} else {
		f.grants[group] = []platform.Grant{{GroupID: group, DatabaseID: db, Level: level}}
	}
	return nil
}

type adminAuthz struct{}

func (adminAuthz) RequirePlatformAdmin(_ context.Context, actor contracts.UserID) error {
	if actor != "1" {
		return contracts.ErrForbidden
	}
	return nil
}
func (adminAuthz) RequireDatabaseAccess(context.Context, contracts.UserID, contracts.DatabaseID, contracts.DatabaseAction) error {
	return contracts.ErrForbidden
}

func testAdmin(t *testing.T) (*adminFake, http.Handler) {
	t.Helper()
	f := &adminFake{users: []platform.User{{ID: "1", Username: "owner", IsPlatformAdmin: true}}, groups: []platform.Group{{ID: "3", Name: "operators"}}, members: make(map[contracts.GroupID][]platform.User), grants: make(map[contracts.GroupID][]platform.Grant)}
	h, err := NewHandler(f, adminAuthz{}, ui.Layout)
	if err != nil {
		t.Fatal(err)
	}
	h.currentUser = func(r *http.Request) (contracts.UserID, bool) {
		id := contracts.UserID(r.Header.Get("X-User"))
		return id, id != ""
	}
	protected, err := auth.CSRFProtection(bytes.Repeat([]byte{2}, 32), false, h)
	if err != nil {
		t.Fatal(err)
	}
	return f, protected
}
func adminGet(h http.Handler, path, actor string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "http://example.com"+path, nil)
	r.Header.Set("X-User", actor)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func adminPost(h http.Handler, path, actor string, form url.Values, cookies []*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "http://example.com"+path, strings.NewReader(form.Encode()))
	r.Header.Set("X-User", actor)
	r.Header.Set("Origin", "http://example.com")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func adminToken(t *testing.T, h http.Handler, path string) ([]*http.Cookie, string) {
	t.Helper()
	w := adminGet(h, path, "1")
	m := regexp.MustCompile(`name="gorilla.csrf.Token" value="([^"]+)"`).FindStringSubmatch(w.Body.String())
	if w.Code != 200 || len(m) != 2 {
		t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
	}
	return w.Result().Cookies(), m[1]
}

func TestAdminUserAndGroupForms(t *testing.T) {
	f, h := testAdmin(t)
	cookies, token := adminToken(t, h, "/platform/users")
	form := url.Values{"gorilla.csrf.Token": {token}, "username": {"alice"}, "password": {"secret-not-rendered"}}
	if w := adminPost(h, "/platform/users", "1", form, cookies); w.Code != 303 {
		t.Fatalf("create user %d", w.Code)
	}
	if f.actor != "1" {
		t.Fatalf("create actor = %q", f.actor)
	}
	if w := adminGet(h, "/platform/users", "1"); w.Code != 200 || !strings.Contains(w.Body.String(), "alice") || strings.Contains(w.Body.String(), "secret-not-rendered") || strings.Count(w.Body.String(), "<main") != 1 {
		t.Fatalf("users: %d %s", w.Code, w.Body.String())
	}
	if w := adminGet(h, "/platform/users", "1"); !strings.Contains(w.Body.String(), `href="/static/platform.css"`) || !strings.Contains(w.Body.String(), `href="/databases/connections/"`) || !strings.Contains(w.Body.String(), `class="btn-danger"`) {
		t.Fatal("admin page lacks module stylesheet, navigation, or destructive action styling")
	}
	for _, tc := range []struct {
		path string
		form url.Values
	}{
		{"/platform/users/2/username", url.Values{"username": {"renamed"}}},
		{"/platform/users/2/password", url.Values{"password": {"different-secret"}}},
		{"/platform/users/2/admin", url.Values{"admin": {"true"}}},
		{"/platform/users/2/delete", url.Values{"confirm": {"true"}}},
		{"/platform/groups", url.Values{"name": {"reviewers"}}},
		{"/platform/groups/3/name", url.Values{"name": {"renamed-group"}}},
		{"/platform/groups/3/delete", url.Values{"confirm": {"true"}}},
	} {
		tc.form.Set("gorilla.csrf.Token", token)
		if w := adminPost(h, tc.path, "1", tc.form, cookies); w.Code != 303 {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
	if f.calls != 8 || len(f.users) != 1 || len(f.groups) != 1 || f.groups[0].Name != "reviewers" {
		t.Fatalf("mutations: %+v", f)
	}
}

func TestAdminMembershipAndGrantForms(t *testing.T) {
	f, h := testAdmin(t)
	cookies, token := adminToken(t, h, "/platform/groups/3")
	if w := adminGet(h, "/platform/groups/3", "1"); w.Code != 200 || !strings.Contains(w.Body.String(), "operators") || !strings.Contains(w.Body.String(), `name="database_id"`) {
		t.Fatalf("group detail did not provide grant form: %d %s", w.Code, w.Body.String())
	}
	for _, tc := range []struct {
		path string
		form url.Values
	}{
		{"/platform/groups/3/members", url.Values{"user_id": {"2"}, "member": {"true"}}},
		{"/platform/groups/3/grants", url.Values{"database_id": {"7"}, "level": {"2"}}},
		{"/platform/groups/3/members", url.Values{"user_id": {"2"}, "member": {"false"}, "confirm": {"true"}}},
		{"/platform/groups/3/grants", url.Values{"database_id": {"7"}, "level": {"0"}, "confirm": {"true"}}},
	} {
		tc.form.Set("gorilla.csrf.Token", token)
		f.actor = ""
		if w := adminPost(h, tc.path, "1", tc.form, cookies); w.Code != 303 {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
		if f.actor != "1" {
			t.Fatalf("%s actor = %q", tc.path, f.actor)
		}
	}
	if f.calls != 4 || len(f.members["3"]) != 0 || len(f.grants["3"]) != 0 {
		t.Fatalf("membership/grants: %+v", f)
	}
}

func TestAdminDestructiveConfirmation(t *testing.T) {
	f, h := testAdmin(t)
	cookies, token := adminToken(t, h, "/platform/groups/3")
	for _, tc := range []struct {
		path string
		form url.Values
	}{
		{"/platform/users/1/delete", url.Values{}},
		{"/platform/users/1/admin", url.Values{"admin": {"false"}}},
		{"/platform/groups/3/delete", url.Values{}},
		{"/platform/groups/3/members", url.Values{"user_id": {"2"}, "member": {"false"}}},
		{"/platform/groups/3/grants", url.Values{"database_id": {"7"}, "level": {"0"}}},
	} {
		tc.form.Set("gorilla.csrf.Token", token)
		w := adminPost(h, tc.path, "1", tc.form, cookies)
		if w.Code != http.StatusBadRequest || f.calls != 0 {
			t.Fatalf("unconfirmed %s: status=%d, service calls=%d", tc.path, w.Code, f.calls)
		}
	}
	f.members["3"] = []platform.User{{ID: "2", Username: "anggota"}}
	f.grants["3"] = []platform.Grant{{GroupID: "3", DatabaseID: "7", Level: contracts.AccessViewOnly}}
	f.users[0].Username = `<owner&>`
	f.groups[0].Name = `<operators&>`
	for _, path := range []string{"/platform/users", "/platform/groups", "/platform/groups/3"} {
		w := adminGet(h, path, "1")
		if w.Code != 200 || !strings.Contains(w.Body.String(), `name="confirm" value="true" required`) {
			t.Fatalf("GET %s missing confirmation checkbox: %d", path, w.Code)
		}
		if strings.Contains(w.Body.String(), `<owner&>`) || strings.Contains(w.Body.String(), `<operators&>`) {
			t.Fatalf("GET %s rendered unescaped target name", path)
		}
	}
	if w := adminGet(h, "/platform/groups/3", "1"); !strings.Contains(w.Body.String(), "anggota akan kehilangan akses") || !strings.Contains(w.Body.String(), "database ID 7") {
		t.Fatal("group confirmations omit target or consequence")
	}
}

func TestAdminErrorsAndMissingGroup(t *testing.T) {
	f, h := testAdmin(t)
	cookies, token := adminToken(t, h, "/platform/groups/3")
	if w := adminGet(h, "/platform/groups/77", "1"); w.Code != 404 {
		t.Fatalf("missing group: %d", w.Code)
	}
	for _, tc := range []struct {
		err    error
		status int
	}{{platform.ErrInvalidInput, 400}, {platform.ErrNotFound, 404}, {platform.ErrConflict, 409}} {
		f.err = tc.err
		w := adminPost(h, "/platform/groups/3/grants", "1", url.Values{"gorilla.csrf.Token": {token}, "database_id": {"7"}, "level": {"2"}}, cookies)
		if w.Code != tc.status || strings.Contains(w.Body.String(), tc.err.Error()) {
			t.Fatalf("service error response: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestAdminRejectsUnauthorizedCSRFAndInvalidInput(t *testing.T) {
	f, h := testAdmin(t)
	cookies, token := adminToken(t, h, "/platform/users")
	for _, path := range []string{"/platform/users", "/platform/groups", "/platform/groups/3"} {
		for _, tc := range []struct {
			actor  string
			status int
		}{{"", 401}, {"2", 403}} {
			if w := adminGet(h, path, tc.actor); w.Code != tc.status || strings.Contains(w.Body.String(), "owner") {
				t.Fatalf("GET %s as %q: %d", path, tc.actor, w.Code)
			}
		}
	}
	for _, tc := range []struct {
		path   string
		form   url.Values
		status int
	}{
		{"/platform/users", url.Values{"username": {"alice"}, "password": {"secret"}}, 403},
		{"/platform/groups/3/grants", url.Values{"database_id": {"7"}, "level": {"99"}, "gorilla.csrf.Token": {token}}, 400},
		{"/platform/groups/3/members", url.Values{"user_id": {"bad"}, "member": {"true"}, "gorilla.csrf.Token": {token}}, 400},
		{"/platform/users/invalid/delete", url.Values{"gorilla.csrf.Token": {token}}, 400},
	} {
		if w := adminPost(h, tc.path, "1", tc.form, cookies); w.Code != tc.status {
			t.Fatalf("POST %s: %d want %d", tc.path, w.Code, tc.status)
		}
	}
	if f.calls != 0 {
		t.Fatalf("invalid request mutated data: %d", f.calls)
	}
	if w := adminPost(h, "/platform/groups", "2", url.Values{"name": {"bad"}, "gorilla.csrf.Token": {token}}, cookies); w.Code != 403 || f.calls != 0 {
		t.Fatalf("non-admin POST: %d", w.Code)
	}
}

func TestAdminMutationRejectsInvalidSessionOrCSRF(t *testing.T) {
	f, h := testAdmin(t)
	cookies, token := adminToken(t, h, "/platform/groups/3")
	for _, path := range []string{"/platform/users", "/platform/groups/3/members", "/platform/groups/3/grants"} {
		form := url.Values{"username": {"x"}, "password": {"private"}, "user_id": {"2"}, "member": {"true"}, "database_id": {"7"}, "level": {"2"}}
		for _, tc := range []struct {
			actor, token string
			status       int
		}{{"1", "", 403}, {"1", "wrong-token", 403}, {"", token, 401}, {"2", token, 403}} {
			form.Set("gorilla.csrf.Token", tc.token)
			w := adminPost(h, path, tc.actor, form, cookies)
			if w.Code != tc.status || strings.Contains(w.Body.String(), "private") || f.calls != 0 {
				t.Fatalf("%s as %q token %q: %d calls %d", path, tc.actor, tc.token, w.Code, f.calls)
			}
		}
	}
}

func TestAdminPasswordAndLastAdminErrorsAreSafe(t *testing.T) {
	f, h := testAdmin(t)
	cookies, token := adminToken(t, h, "/platform/users")
	f.err = platform.ErrInvalidInput
	w := adminPost(h, "/platform/users", "1", url.Values{"username": {"alice"}, "password": {"private-password"}, "gorilla.csrf.Token": {token}}, cookies)
	if w.Code != 400 || strings.Contains(w.Body.String(), "private-password") {
		t.Fatalf("invalid password: %d %s", w.Code, w.Body.String())
	}
	f.err = platform.ErrConflict
	w = adminPost(h, "/platform/users/1/admin", "1", url.Values{"admin": {"false"}, "confirm": {"true"}, "gorilla.csrf.Token": {token}}, cookies)
	if w.Code != 409 || !f.users[0].IsPlatformAdmin {
		t.Fatalf("last admin: %d", w.Code)
	}
}

func TestAdminAcceptsFormMediaTypeWithCharset(t *testing.T) {
	f, h := testAdmin(t)
	cookies, token := adminToken(t, h, "/platform/groups")
	r := httptest.NewRequest("POST", "http://example.com/platform/groups", strings.NewReader(url.Values{"name": {"new-group"}, "gorilla.csrf.Token": {token}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	r.Header.Set("Origin", "http://example.com")
	r.Header.Set("X-User", "1")
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 303 || f.calls != 1 {
		t.Fatalf("valid form with charset: %d calls %d", w.Code, f.calls)
	}
}

func TestAdminErrorsDoNotLeakSecrets(t *testing.T) {
	f, h := testAdmin(t)
	cookies, token := adminToken(t, h, "/platform/users")
	f.err = platform.ErrConflict
	w := adminPost(h, "/platform/users", "1", url.Values{"username": {"duplicate"}, "password": {"private-password"}, "gorilla.csrf.Token": {token}}, cookies)
	if w.Code != 409 || strings.Contains(w.Body.String(), "private-password") {
		t.Fatalf("conflict: %d %s", w.Code, w.Body.String())
	}
	f.err = errors.New("private-sql-connection-string")
	w = adminPost(h, "/platform/groups", "1", url.Values{"name": {"x"}, "gorilla.csrf.Token": {token}}, cookies)
	if w.Code != 503 || strings.Contains(w.Body.String(), "private-sql-connection-string") {
		t.Fatalf("database error: %d %s", w.Code, w.Body.String())
	}
}
