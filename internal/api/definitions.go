package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"if3110.local/db-platform/internal/auth"
	"if3110.local/db-platform/internal/contracts"
	"if3110.local/db-platform/internal/definition"
)

type DefinitionService interface {
	List(context.Context, contracts.UserID, contracts.DatabaseID) ([]definition.Definition, error)
	Get(context.Context, contracts.UserID, contracts.DatabaseID, string) (definition.Definition, error)
	Create(context.Context, contracts.UserID, contracts.DatabaseID, definition.Input) (definition.Definition, error)
	Revise(context.Context, contracts.UserID, contracts.DatabaseID, string, int, definition.Input) (definition.Definition, error)
	Validate(context.Context, contracts.UserID, contracts.DatabaseID, string) (definition.ValidationReport, error)
}

type definitionHandler struct{ service DefinitionService }

func NewDefinitionHandler(service DefinitionService, sessions Sessions) (http.Handler, error) {
	if service == nil || sessions == nil {
		return nil, errors.New("definition API requires service and sessions")
	}
	h := &definitionHandler{service: service}
	return sessions.RequireSessionWithError(http.HandlerFunc(h.serve), func(w http.ResponseWriter, _ *http.Request, err error) {
		if errors.Is(err, auth.ErrUnauthenticated) {
			WriteError(w, 401, "UNAUTHENTICATED", "Sesi Anda berakhir. Silakan masuk kembali.")
			return
		}
		WriteError(w, 503, "AUTH_UNAVAILABLE", "Layanan login sementara tidak tersedia.")
	}), nil
}

func (h *definitionHandler) serve(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.CurrentUser(r)
	if !ok || actor == "" {
		WriteError(w, 401, "UNAUTHENTICATED", "Sesi Anda berakhir. Silakan masuk kembali.")
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/"), "/")
	if len(parts) < 3 || len(parts) > 5 || parts[0] != "databases" || parts[2] != "migrations" {
		WriteError(w, 404, "NOT_FOUND", "Endpoint tidak ditemukan.")
		return
	}
	if !validID(parts[1]) || (len(parts) >= 4 && !validID(parts[3])) {
		WriteError(w, 400, "INVALID_REQUEST", "ID tidak valid.")
		return
	}
	if r.URL.RawQuery != "" {
		WriteError(w, 400, "INVALID_REQUEST", "Parameter query tidak didukung.")
		return
	}
	id := contracts.DatabaseID(parts[1])
	ctx := r.Context()
	if len(parts) == 5 {
		if parts[4] != "validate" {
			WriteError(w, 404, "NOT_FOUND", "Endpoint tidak ditemukan.")
			return
		}
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return
		}
		report, err := h.service.Validate(ctx, actor, id, parts[3])
		if err != nil {
			h.fail(w, err)
			return
		}
		writeData(w, struct {
			Validation definition.ValidationReport `json:"validation"`
		}{report})
		return
	}
	if len(parts) == 3 {
		switch r.Method {
		case http.MethodGet:
			items, err := h.service.List(ctx, actor, id)
			if err != nil {
				h.fail(w, err)
				return
			}
			if items == nil {
				items = []definition.Definition{}
			}
			writeData(w, struct {
				Migrations []definition.Definition `json:"migrations"`
			}{items})
		case http.MethodPost:
			var input definition.Input
			if !decodeJSON(w, r, &input, 2<<20+16<<10) {
				return
			}
			item, err := h.service.Create(ctx, actor, id, input)
			if err != nil {
				h.fail(w, err)
				return
			}
			writeJSON(w, 201, struct {
				Data any `json:"data"`
			}{struct {
				Migration definition.Definition `json:"migration"`
			}{item}})
		default:
			methodNotAllowed(w, "GET, POST")
		}
		return
	}
	switch r.Method {
	case http.MethodGet:
		item, err := h.service.Get(ctx, actor, id, parts[3])
		if err != nil {
			h.fail(w, err)
			return
		}
		writeData(w, struct {
			Migration definition.Definition `json:"migration"`
		}{item})
	case http.MethodPatch:
		var body struct {
			definition.Input
			ExpectedRevision int `json:"expected_revision"`
		}
		if !decodeJSON(w, r, &body, 2<<20+16<<10) {
			return
		}
		if body.ExpectedRevision < 1 {
			WriteError(w, 400, "INVALID_REQUEST", "Revisi yang diharapkan tidak valid.")
			return
		}
		item, err := h.service.Revise(ctx, actor, id, parts[3], body.ExpectedRevision, body.Input)
		if err != nil {
			h.fail(w, err)
			return
		}
		writeData(w, struct {
			Migration definition.Definition `json:"migration"`
		}{item})
	default:
		methodNotAllowed(w, "GET, PATCH")
	}
}

func (h *definitionHandler) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, contracts.ErrForbidden):
		WriteError(w, 403, "FORBIDDEN", "Akses ditolak.")
	case errors.Is(err, definition.ErrNotFound):
		WriteError(w, 404, "NOT_FOUND", "Data tidak ditemukan.")
	case errors.Is(err, definition.ErrConflict):
		WriteError(w, 409, "CONFLICT", "Revisi atau status migration telah berubah.")
	case errors.Is(err, definition.ErrInvalidInput):
		WriteError(w, 422, "VALIDATION_FAILED", "Isian migration tidak valid.")
	default:
		WriteError(w, 503, "SERVICE_UNAVAILABLE", "Layanan sementara tidak tersedia.")
	}
}
