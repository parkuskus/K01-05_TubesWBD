package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"if3110.local/db-platform/internal/approval"
	"if3110.local/db-platform/internal/auth"
	"if3110.local/db-platform/internal/contracts"
)

type ApprovalService interface {
	Submit(context.Context, contracts.UserID, contracts.DatabaseID, string, int, contracts.OperationDirection, string) (approval.Request, error)
	SubmitWithOrigin(context.Context, contracts.UserID, contracts.DatabaseID, string, int, contracts.OperationDirection, string, string) (approval.Request, error)
	List(context.Context, contracts.UserID, contracts.DatabaseID) ([]approval.Request, error)
	GetByID(context.Context, contracts.UserID, contracts.OperationRequestID) (approval.Request, error)
	Approve(context.Context, contracts.UserID, contracts.OperationRequestID, int, int, contracts.GroupID, string) (approval.Request, error)
	Plan(context.Context, contracts.UserID, contracts.OperationRequestID, contracts.CommitEvidenceReader, contracts.ExecutionID) (contracts.ExecutionPlan, error)
}

type approvalHandler struct {
	service   ApprovalService
	evidence  contracts.CommitEvidenceReader
	publisher contracts.JobPublisher
}

type publishedJobFinder interface {
	FindPublished(context.Context, contracts.UserID, contracts.DatabaseID, string) (contracts.JobSummary, bool, error)
}

type executionPlanResponse struct {
	RequestID                contracts.OperationRequestID `json:"request_id"`
	ExecutionID              contracts.ExecutionID        `json:"execution_id"`
	MigrationID              contracts.MigrationID        `json:"migration_id"`
	RevisionID               contracts.RevisionID         `json:"revision_id"`
	Version                  int64                        `json:"version"`
	Direction                contracts.OperationDirection `json:"direction"`
	Mode                     contracts.ExecutionMode      `json:"mode"`
	ActorID                  contracts.UserID             `json:"actor_id"`
	TargetDatabaseID         contracts.DatabaseID         `json:"target_database_id"`
	TargetConnectionRevision int64                        `json:"target_connection_revision"`
	ExpectedActiveHead       contracts.ExecutionID        `json:"expected_active_head"`
	PolicyVersion            int64                        `json:"policy_version"`
	UpSQL                    string                       `json:"up_sql"`
	DownSQL                  string                       `json:"down_sql"`
	IsIrreversible           bool                         `json:"is_irreversible"`
	IrreversibleReason       string                       `json:"irreversible_reason"`
	OriginExecutionID        contracts.ExecutionID        `json:"origin_execution_id"`
	Batch                    contracts.BatchConfig        `json:"batch"`
	PlanChecksum             string                       `json:"plan_checksum"`
}

func planResponse(plan contracts.ExecutionPlan) executionPlanResponse {
	return executionPlanResponse{
		RequestID: plan.RequestID, ExecutionID: plan.ExecutionID, MigrationID: plan.MigrationID,
		RevisionID: plan.RevisionID, Version: plan.Version, Direction: plan.Direction, Mode: plan.Mode,
		ActorID: plan.ActorID, TargetDatabaseID: plan.TargetDatabaseID,
		TargetConnectionRevision: plan.TargetConnectionRevision, ExpectedActiveHead: plan.ExpectedActiveHead,
		PolicyVersion: plan.PolicyVersion, UpSQL: plan.UpSQL, DownSQL: plan.DownSQL,
		IsIrreversible: plan.IsIrreversible, IrreversibleReason: plan.IrreversibleReason,
		OriginExecutionID: plan.OriginExecutionID, Batch: plan.Batch, PlanChecksum: plan.PlanChecksum,
	}
}

// NewApprovalHandler wires an optional read-only target evidence reader for plan creation.
func NewApprovalHandler(service ApprovalService, sessions Sessions, evidence contracts.CommitEvidenceReader, publisher contracts.JobPublisher) (http.Handler, error) {
	if service == nil || sessions == nil {
		return nil, errors.New("approval API requires service and sessions")
	}
	h := &approvalHandler{service: service, evidence: evidence, publisher: publisher}
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
				MigrationID       string                       `json:"migration_id"`
				Revision          int                          `json:"revision"`
				Direction         contracts.OperationDirection `json:"direction"`
				ExpectedHead      string                       `json:"expected_head"`
				OriginExecutionID string                       `json:"origin_execution_id"`
			}
			if !decodeJSON(w, r, &body, 16<<10) {
				return
			}
			item, err := h.service.SubmitWithOrigin(ctx, actor, databaseID, body.MigrationID, body.Revision, body.Direction, body.ExpectedHead, body.OriginExecutionID)
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
	if len(parts) == 3 && parts[0] == "operation-requests" && validID(parts[1]) && parts[2] == "plan" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return
		}
		plan, err := h.plan(ctx, actor, contracts.OperationRequestID(parts[1]), uuid.NewString())
		if err != nil {
			h.fail(w, err)
			return
		}
		writeData(w, struct {
			Plan executionPlanResponse `json:"plan"`
		}{planResponse(plan)})
		return
	}
	if len(parts) == 3 && parts[0] == "operation-requests" && validID(parts[1]) && parts[2] == "enqueue" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return
		}
		if h.publisher == nil {
			WriteError(w, 503, "SERVICE_UNAVAILABLE", "Layanan antrean sementara tidak tersedia.")
			return
		}
		idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if idempotencyKey == "" || len(idempotencyKey) > 255 || strings.IndexFunc(idempotencyKey, unicode.IsControl) >= 0 {
			WriteError(w, 400, "INVALID_REQUEST", "Header Idempotency-Key wajib dan harus valid.")
			return
		}
		var body struct{}
		if !decodeJSON(w, r, &body, 1024) {
			return
		}
		requestID := contracts.OperationRequestID(parts[1])
		request, err := h.service.GetByID(ctx, actor, requestID)
		if err != nil {
			h.fail(w, err)
			return
		}
		if finder, ok := h.publisher.(publishedJobFinder); ok {
			job, found, err := finder.FindPublished(ctx, actor, request.DatabaseID, idempotencyKey)
			if err != nil {
				h.fail(w, err)
				return
			}
			if found {
				if job.RequestID != requestID {
					h.fail(w, contracts.ErrIdempotencyConflict)
					return
				}
				h.writeAcceptedJob(w, job)
				return
			}
		}
		executionID := uuid.NewSHA1(uuid.NameSpaceURL, []byte(string(requestID)+"\x00"+idempotencyKey)).String()
		plan, err := h.plan(ctx, actor, requestID, executionID)
		if err != nil {
			h.fail(w, err)
			return
		}
		job, err := h.publisher.Publish(ctx, contracts.EnqueueJobRequest{Plan: plan, IdempotencyKey: idempotencyKey})
		if err != nil {
			h.fail(w, err)
			return
		}
		h.writeAcceptedJob(w, job)
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

func (h *approvalHandler) writeAcceptedJob(w http.ResponseWriter, job contracts.JobSummary) {
	writeJSON(w, http.StatusAccepted, struct {
		Data struct {
			Job contracts.JobSummary `json:"job"`
		} `json:"data"`
	}{Data: struct {
		Job contracts.JobSummary `json:"job"`
	}{job}})
}

func (h *approvalHandler) plan(ctx context.Context, actor contracts.UserID, requestID contracts.OperationRequestID, executionID string) (contracts.ExecutionPlan, error) {
	if h.evidence == nil {
		return contracts.ExecutionPlan{}, errors.New("target evidence reader unavailable")
	}
	return h.service.Plan(ctx, actor, requestID, h.evidence, contracts.ExecutionID(executionID))
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
	case errors.Is(err, contracts.ErrHeadMismatch), errors.Is(err, contracts.ErrExecutionGateBlocked), errors.Is(err, contracts.ErrIrreversibleMigration):
		WriteError(w, 409, "PLAN_REJECTED", "Kondisi target tidak mengizinkan operasi ini.")
	case errors.Is(err, contracts.ErrIdempotencyConflict):
		WriteError(w, 409, "IDEMPOTENCY_CONFLICT", "Idempotency-Key sudah digunakan untuk operasi berbeda.")
	case errors.Is(err, approval.ErrConflict):
		WriteError(w, 409, "CONFLICT", "Status operation request telah berubah.")
	case errors.Is(err, approval.ErrInvalidInput):
		WriteError(w, 422, "VALIDATION_FAILED", "Isian operation request tidak valid.")
	default:
		WriteError(w, 503, "SERVICE_UNAVAILABLE", "Layanan sementara tidak tersedia.")
	}
}
