package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"if3110.local/db-platform/internal/approval"
	"if3110.local/db-platform/internal/auth"
	"if3110.local/db-platform/internal/contracts"
)

type ApprovalService interface {
	Submit(context.Context, contracts.UserID, contracts.DatabaseID, string, int, contracts.OperationDirection, string) (approval.Request, error)
	List(context.Context, contracts.UserID, contracts.DatabaseID) ([]approval.Request, error)
	GetByID(context.Context, contracts.UserID, contracts.OperationRequestID) (approval.Request, error)
	Approve(context.Context, contracts.UserID, contracts.OperationRequestID, int, int, contracts.GroupID, string) (approval.Request, error)
}

type approvalHandler struct{ service ApprovalService }

func NewApprovalHandler(service ApprovalService, sessions Sessions) (http.Handler, error) {
	if service == nil || sessions == nil {
		return nil, errors.New("approval API requires service and sessions")
	}
	h := &approvalHandler{service: service}
	return sessions.RequireSessionWithError(http.HandlerFunc(h.serve), func(w http.ResponseWriter, _ *http.Request, err error) {
		if errors.Is(err, auth.ErrUnauthenticated) {
			WriteError(w, 401, "UNAUTHENTICATED", "Sesi Anda berakhir. Silakan masuk kembali.")
			return
		}
		WriteError(w, 503, "AUTH_UNAVAILABLE", "Layanan login sementara tidak tersedia.")
	}), nil
}

func (h *approvalHandler) serve(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.CurrentUser(r)
	if !ok || actor == "" {
		WriteError(w, 401, "UNAUTHENTICATED", "Sesi Anda berakhir. Silakan masuk kembali.")
		return
	}
	if r.URL.RawQuery != "" {
		WriteError(w, 400, "INVALID_REQUEST", "Parameter query tidak didukung.")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	parts := strings.Split(path, "/")
	ctx := r.Context()
	if len(parts) == 3 && parts[0] == "databases" && parts[2] == "operation-requests" {
		if !validID(parts[1]) {
			WriteError(w, 400, "INVALID_REQUEST", "ID database tidak valid.")
			return
		}
		databaseID := contracts.DatabaseID(parts[1])
		switch r.Method {
		case http.MethodGet:
			items, err := h.service.List(ctx, actor, databaseID)
			if err != nil {
				h.fail(w, err)
				return
			}
			if items == nil {
				items = []approval.Request{}
			}
			writeData(w, struct {
				Requests []approval.Request `json:"requests"`
			}{items})
		case http.MethodPost:
			var body struct {
				MigrationID  string                       `json:"migration_id"`
				Revision     int                          `json:"revision"`
				Direction    contracts.OperationDirection `json:"direction"`
				ExpectedHead string                       `json:"expected_head"`
			}
			if !decodeJSON(w, r, &body, 16<<10) {
				return
			}
			item, err := h.service.Submit(ctx, actor, databaseID, body.MigrationID, body.Revision, body.Direction, body.ExpectedHead)
			if err != nil {
				h.fail(w, err)
				return
			}
			writeJSON(w, http.StatusCreated, struct {
				Data struct {
					Request approval.Request `json:"request"`
				} `json:"data"`
			}{Data: struct {
				Request approval.Request `json:"request"`
			}{item}})
		default:
			methodNotAllowed(w, "GET, POST")
		}
		return
	}
	if len(parts) == 2 && parts[0] == "operation-requests" && validID(parts[1]) {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return
		}
		item, err := h.service.GetByID(ctx, actor, contracts.OperationRequestID(parts[1]))
		if err != nil {
			h.fail(w, err)
			return
		}
		writeData(w, struct {
			Request approval.Request `json:"request"`
		}{item})
		return
	}
	if len(parts) == 3 && parts[0] == "operation-requests" && validID(parts[1]) && parts[2] == "approvals" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return
		}
		var body struct {
			Revision int               `json:"migration_revision"`
			Round    int               `json:"review_round"`
			GroupID  contracts.GroupID `json:"group_id"`
			Checksum string            `json:"checksum"`
		}
		if !decodeJSON(w, r, &body, 16<<10) {
			return
		}
		if !validID(string(body.GroupID)) {
			WriteError(w, 400, "INVALID_REQUEST", "ID grup tidak valid.")
			return
		}
		item, err := h.service.Approve(ctx, actor, contracts.OperationRequestID(parts[1]), body.Revision, body.Round, body.GroupID, body.Checksum)
		if err != nil {
			h.fail(w, err)
			return
		}
		writeData(w, struct {
			Request approval.Request `json:"request"`
		}{item})
		return
	}
	WriteError(w, 404, "NOT_FOUND", "Endpoint tidak ditemukan.")
}

func (h *approvalHandler) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, contracts.ErrForbidden):
		WriteError(w, 403, "FORBIDDEN", "Akses ditolak.")
	case errors.Is(err, approval.ErrNotFound):
		WriteError(w, 404, "NOT_FOUND", "Data tidak ditemukan.")
	case errors.Is(err, approval.ErrMissingLeader):
		WriteError(w, 422, "MISSING_GROUP_LEADER", "Setiap grup dengan akses target harus memiliki leader sebelum pengajuan review.")
	case errors.Is(err, approval.ErrStale):
		WriteError(w, 409, "APPROVAL_STALE", "Akses, leader, target, atau revisi berubah. Ajukan review ulang.")
	case errors.Is(err, approval.ErrConflict):
		WriteError(w, 409, "CONFLICT", "Status operation request telah berubah.")
	case errors.Is(err, approval.ErrInvalidInput):
		WriteError(w, 422, "VALIDATION_FAILED", "Isian operation request tidak valid.")
	default:
		WriteError(w, 503, "SERVICE_UNAVAILABLE", "Layanan sementara tidak tersedia.")
	}
}
