package api

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"if3110.local/db-platform/internal/approval"
	"if3110.local/db-platform/internal/contracts"
)

type approvalServiceFixture struct {
	request approval.Request
	err     error
	call    string
}

func (f *approvalServiceFixture) Submit(_ context.Context, actor contracts.UserID, db contracts.DatabaseID, migration string, revision int, direction contracts.OperationDirection, expectedHead string) (approval.Request, error) {
	f.call = "submit:" + string(actor) + ":" + string(db) + ":" + migration + ":" + string(direction)
	return f.request, f.err
}
func (f *approvalServiceFixture) List(_ context.Context, _ contracts.UserID, db contracts.DatabaseID) ([]approval.Request, error) {
	f.call = "list:" + string(db)
	return []approval.Request{f.request}, f.err
}
func (f *approvalServiceFixture) GetByID(_ context.Context, actor contracts.UserID, id contracts.OperationRequestID) (approval.Request, error) {
	f.call = "get:" + string(actor) + ":" + string(id)
	return f.request, f.err
}
func (f *approvalServiceFixture) Approve(_ context.Context, actor contracts.UserID, id contracts.OperationRequestID, revision, round int, group contracts.GroupID, checksum string) (approval.Request, error) {
	f.call = "approve:" + string(actor) + ":" + string(id) + ":" + string(group)
	return f.request, f.err
}

func TestApprovalAPIRoutes(t *testing.T) {
	f := &approvalServiceFixture{request: approval.Request{ID: "11", Status: "PENDING_REVIEW", Checksum: strings.Repeat("a", 64)}}
	h, err := NewApprovalHandler(f, &testSessions{user: "7"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path, body, call, contains string
		status                             int
	}{
		{"POST", "/api/v1/databases/2/operation-requests", `{"migration_id":"3","revision":1,"direction":"APPLY"}`, "submit:7:2:3:APPLY", `"status":"PENDING_REVIEW"`, 201},
		{"GET", "/api/v1/databases/2/operation-requests", "", "list:2", `"requests"`, 200},
		{"GET", "/api/v1/operation-requests/11", "", "get:7:11", `"id":"11"`, 200},
		{"POST", "/api/v1/operation-requests/11/approvals", `{"migration_revision":1,"review_round":2,"group_id":"4","checksum":"` + strings.Repeat("a", 64) + `"}`, "approve:7:11:4", `"PENDING_REVIEW"`, 200},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			f.call = ""
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.contains) || f.call != tc.call {
				t.Fatalf("status=%d body=%s call=%q", w.Code, w.Body.String(), f.call)
			}
		})
	}
}

func TestApprovalAPIRejectsMalformedActorAndMapsErrors(t *testing.T) {
	for _, tc := range []struct {
		user   contracts.UserID
		method string
		path   string
		body   string
		status int
	}{
		{"", "GET", "/api/v1/databases/2/operation-requests", "", 401},
		{"7", "POST", "/api/v1/databases/02/operation-requests", `{"migration_id":"3","revision":1,"direction":"APPLY"}`, 400},
		{"7", "POST", "/api/v1/databases/2/operation-requests", `{"migration_id":"3","revision":1,"direction":"APPLY","actor_id":"99"}`, 400},
		{"7", "POST", "/api/v1/operation-requests/11/approvals", `{"migration_revision":1}`, 400},
	} {
		f := &approvalServiceFixture{}
		h, err := NewApprovalHandler(f, &testSessions{user: tc.user})
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status || f.call != "" {
			t.Fatalf("status=%d body=%s call=%q", w.Code, w.Body.String(), f.call)
		}
	}
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{contracts.ErrForbidden, 403, "FORBIDDEN"},
		{approval.ErrMissingLeader, 422, "MISSING_GROUP_LEADER"},
		{approval.ErrStale, 409, "APPROVAL_STALE"},
		{approval.ErrNotFound, 404, "NOT_FOUND"},
		{errors.New("secret DSN"), 503, "SERVICE_UNAVAILABLE"},
	} {
		f := &approvalServiceFixture{err: tc.err}
		h, _ := NewApprovalHandler(f, &testSessions{user: "7"})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/databases/2/operation-requests", nil))
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) || strings.Contains(w.Body.String(), "secret") {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
}
