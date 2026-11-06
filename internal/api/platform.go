package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"if3110.local/db-platform/internal/auth"
	"if3110.local/db-platform/internal/contracts"
	"if3110.local/db-platform/internal/platform"
)

// PlatformService is the M1 administration surface exposed by the JSON adapter.
type PlatformService interface {
	ListUsers(context.Context, contracts.UserID) ([]platform.User, error)
	CreateUser(context.Context, contracts.UserID, string, string) (contracts.UserID, error)
	UpdateUsername(context.Context, contracts.UserID, contracts.UserID, string) error
	SetPassword(context.Context, contracts.UserID, contracts.UserID, string) error
	SetPlatformAdmin(context.Context, contracts.UserID, contracts.UserID, bool) error
	DeleteUser(context.Context, contracts.UserID, contracts.UserID) error
	ListGroups(context.Context, contracts.UserID) ([]platform.Group, error)
	SetGroupLeader(context.Context, contracts.UserID, contracts.GroupID, contracts.UserID) error
	CreateGroup(context.Context, contracts.UserID, string) (contracts.GroupID, error)
	RenameGroup(context.Context, contracts.UserID, contracts.GroupID, string) error
	DeleteGroup(context.Context, contracts.UserID, contracts.GroupID) error
	ListMembers(context.Context, contracts.UserID, contracts.GroupID) ([]platform.User, error)
	SetMembership(context.Context, contracts.UserID, contracts.UserID, contracts.GroupID, bool) error
	ListGrants(context.Context, contracts.UserID, contracts.GroupID) ([]platform.Grant, error)
	SetGrant(context.Context, contracts.UserID, contracts.GroupID, contracts.DatabaseID, contracts.AccessLevel) error
}

type platformHandler struct {
	service    PlatformService
	authorizer contracts.Authorizer
}

func NewPlatformHandler(service PlatformService, authorizer contracts.Authorizer, sessions Sessions) (http.Handler, error) {
	if service == nil || authorizer == nil || sessions == nil {
		return nil, errors.New("platform API requires service, authorizer, and sessions")
	}
	h := &platformHandler{service: service, authorizer: authorizer}
	return sessions.RequireSessionWithError(http.HandlerFunc(h.serve), func(w http.ResponseWriter, _ *http.Request, err error) {
		if errors.Is(err, auth.ErrUnauthenticated) {
			WriteError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "Sesi Anda berakhir. Silakan masuk kembali.")
			return
		}
		WriteError(w, http.StatusServiceUnavailable, "AUTH_UNAVAILABLE", "Layanan login sementara tidak tersedia.")
	}), nil
}

type apiUser struct {
	ID              contracts.UserID `json:"id"`
	Username        string           `json:"username"`
	IsPlatformAdmin bool             `json:"is_platform_admin"`
}
type apiGroup struct {
	ID           contracts.GroupID `json:"id"`
	Name         string            `json:"name"`
	LeaderUserID *contracts.UserID `json:"leader_user_id"`
}
type apiGrant struct {
	GroupID    contracts.GroupID     `json:"group_id"`
	DatabaseID contracts.DatabaseID  `json:"database_id"`
	Level      contracts.AccessLevel `json:"level"`
}

func (h *platformHandler) serve(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.CurrentUser(r)
	if !ok || actor == "" {
		WriteError(w, 401, "UNAUTHENTICATED", "Sesi Anda berakhir. Silakan masuk kembali.")
		return
	}
	if err := h.authorizer.RequirePlatformAdmin(r.Context(), actor); err != nil {
		h.fail(w, err)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/"), "/")
	if len(parts) < 1 || (parts[0] != "users" && parts[0] != "groups") {
		WriteError(w, 404, "NOT_FOUND", "Endpoint tidak ditemukan.")
		return
	}
	if r.URL.RawQuery != "" {
		WriteError(w, 400, "INVALID_REQUEST", "Parameter query tidak didukung.")
		return
	}
	if parts[0] == "users" {
		h.users(w, r, actor, parts)
	} else {
		h.groups(w, r, actor, parts)
	}
}

func (h *platformHandler) users(w http.ResponseWriter, r *http.Request, actor contracts.UserID, parts []string) {
	ctx := r.Context()
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			users, err := h.service.ListUsers(ctx, actor)
			if err != nil {
				h.fail(w, err)
				return
			}
			out := make([]apiUser, 0, len(users))
			for _, u := range users {
				out = append(out, apiUser{u.ID, u.Username, u.IsPlatformAdmin})
			}
			writeData(w, struct {
				Users []apiUser `json:"users"`
			}{out})
		case http.MethodPost:
			var input struct {
				Username string `json:"username"`
				Password string `json:"password"`
			}
			if !decodePlatform(w, r, &input) {
				return
			}
			id, err := h.service.CreateUser(ctx, actor, input.Username, input.Password)
			if err != nil {
				h.fail(w, err)
				return
			}
			writeJSON(w, 201, struct {
				Data any `json:"data"`
			}{struct {
				ID contracts.UserID `json:"id"`
			}{id}})
		default:
			methodNotAllowed(w, "GET, POST")
		}
		return
	}
	if len(parts) > 3 || len(parts) == 3 && parts[2] != "password" {
		WriteError(w, 404, "NOT_FOUND", "Endpoint tidak ditemukan.")
		return
	}
	if !validID(parts[1]) {
		WriteError(w, 400, "INVALID_REQUEST", "ID tidak valid.")
		return
	}
	id := contracts.UserID(parts[1])
	var err error
	if len(parts) == 3 {
		if r.Method != http.MethodPut {
			methodNotAllowed(w, "PUT")
			return
		}
		var input struct {
			Password string `json:"password"`
		}
		if !decodePlatform(w, r, &input) {
			return
		}
		err = h.service.SetPassword(ctx, actor, id, input.Password)
	} else {
		switch r.Method {
		case http.MethodPatch:
			var input struct {
				Username *string `json:"username"`
				Admin    *bool   `json:"is_platform_admin"`
			}
			if !decodePlatform(w, r, &input) {
				return
			}
			if (input.Username == nil) == (input.Admin == nil) {
				WriteError(w, 400, "INVALID_REQUEST", "Ubah tepat satu field.")
				return
			}
			if input.Username != nil {
				err = h.service.UpdateUsername(ctx, actor, id, *input.Username)
			} else {
				err = h.service.SetPlatformAdmin(ctx, actor, id, *input.Admin)
			}
		case http.MethodDelete:
			err = h.service.DeleteUser(ctx, actor, id)
		default:
			methodNotAllowed(w, "PATCH, DELETE")
			return
		}
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *platformHandler) groups(w http.ResponseWriter, r *http.Request, actor contracts.UserID, parts []string) {
	ctx := r.Context()
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			groups, err := h.service.ListGroups(ctx, actor)
			if err != nil {
				h.fail(w, err)
				return
			}
			out := make([]apiGroup, 0, len(groups))
			for _, g := range groups {
				out = append(out, apiGroup{g.ID, g.Name, g.LeaderUserID})
			}
			writeData(w, struct {
				Groups []apiGroup `json:"groups"`
			}{out})
		case http.MethodPost:
			var input struct {
				Name string `json:"name"`
			}
			if !decodePlatform(w, r, &input) {
				return
			}
			id, err := h.service.CreateGroup(ctx, actor, input.Name)
			if err != nil {
				h.fail(w, err)
				return
			}
			writeJSON(w, 201, struct {
				Data any `json:"data"`
			}{struct {
				ID contracts.GroupID `json:"id"`
			}{id}})
		default:
			methodNotAllowed(w, "GET, POST")
		}
		return
	}
	if len(parts) != 2 && len(parts) != 3 && len(parts) != 4 {
		WriteError(w, 404, "NOT_FOUND", "Endpoint tidak ditemukan.")
		return
	}
	if !validID(parts[1]) {
		WriteError(w, 400, "INVALID_REQUEST", "ID tidak valid.")
		return
	}
	group := contracts.GroupID(parts[1])
	if len(parts) == 3 && parts[2] == "leader" {
		if r.Method != http.MethodPut {
			methodNotAllowed(w, "PUT")
			return
		}
		var input struct {
			UserID string `json:"user_id"`
		}
		if !decodePlatform(w, r, &input) {
			return
		}
		if !validID(input.UserID) {
			WriteError(w, 400, "INVALID_REQUEST", "ID leader tidak valid.")
			return
		}
		if err := h.service.SetGroupLeader(ctx, actor, group, contracts.UserID(input.UserID)); err != nil {
			h.fail(w, err)
			return
		}
		w.WriteHeader(204)
		return
	}
	if len(parts) == 2 {
		var err error
		switch r.Method {
		case http.MethodGet:
			groups, listErr := h.service.ListGroups(ctx, actor)
			if listErr != nil {
				h.fail(w, listErr)
				return
			}
			for _, item := range groups {
				if item.ID == group {
					writeData(w, struct {
						Group apiGroup `json:"group"`
					}{apiGroup{item.ID, item.Name, item.LeaderUserID}})
					return
				}
			}
			h.fail(w, platform.ErrNotFound)
			return
		case http.MethodPatch:
			var input struct {
				Name string `json:"name"`
			}
			if !decodePlatform(w, r, &input) {
				return
			}
			err = h.service.RenameGroup(ctx, actor, group, input.Name)
		case http.MethodDelete:
			err = h.service.DeleteGroup(ctx, actor, group)
		default:
			methodNotAllowed(w, "GET, PATCH, DELETE")
			return
		}
		if err != nil {
			h.fail(w, err)
			return
		}
		w.WriteHeader(204)
		return
	}
	if parts[2] != "members" && parts[2] != "grants" {
		WriteError(w, 404, "NOT_FOUND", "Endpoint tidak ditemukan.")
		return
	}
	if len(parts) == 3 {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return
		}
		if parts[2] == "members" {
			members, err := h.service.ListMembers(ctx, actor, group)
			if err != nil {
				h.fail(w, err)
				return
			}
			out := make([]apiUser, 0, len(members))
			for _, u := range members {
				out = append(out, apiUser{u.ID, u.Username, u.IsPlatformAdmin})
			}
			writeData(w, struct {
				Members []apiUser `json:"members"`
			}{out})
		} else {
			grants, err := h.service.ListGrants(ctx, actor, group)
			if err != nil {
				h.fail(w, err)
				return
			}
			out := make([]apiGrant, 0, len(grants))
			for _, g := range grants {
				out = append(out, apiGrant{g.GroupID, g.DatabaseID, g.Level})
			}
			writeData(w, struct {
				Grants []apiGrant `json:"grants"`
			}{out})
		}
		return
	}
	if !validID(parts[3]) {
		WriteError(w, 400, "INVALID_REQUEST", "ID tidak valid.")
		return
	}
	if parts[2] == "grants" && r.Method == http.MethodGet {
		grants, err := h.service.ListGrants(ctx, actor, group)
		if err != nil {
			h.fail(w, err)
			return
		}
		for _, item := range grants {
			if item.DatabaseID == contracts.DatabaseID(parts[3]) {
				writeData(w, struct {
					Grant apiGrant `json:"grant"`
				}{apiGrant{item.GroupID, item.DatabaseID, item.Level}})
				return
			}
		}
		h.fail(w, platform.ErrNotFound)
		return
	}
	if r.Method != http.MethodPut && r.Method != http.MethodDelete {
		if parts[2] == "grants" {
			methodNotAllowed(w, "GET, PUT, DELETE")
		} else {
			methodNotAllowed(w, "PUT, DELETE")
		}
		return
	}
	var err error
	if parts[2] == "members" {
		err = h.service.SetMembership(ctx, actor, contracts.UserID(parts[3]), group, r.Method == http.MethodPut)
	} else {
		level := contracts.AccessNone
		if r.Method == http.MethodPut {
			var input struct {
				Level *contracts.AccessLevel `json:"level"`
			}
			if !decodePlatform(w, r, &input) {
				return
			}
			if input.Level == nil || *input.Level < contracts.AccessViewOnly || *input.Level > contracts.AccessViewEdit {
				WriteError(w, 422, "VALIDATION_FAILED", "Level akses tidak valid.")
				return
			}
			level = *input.Level
		}
		err = h.service.SetGrant(ctx, actor, group, contracts.DatabaseID(parts[3]), level)
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(204)
}

func validID(s string) bool {
	n, err := strconv.ParseInt(s, 10, 64)
	return err == nil && n > 0 && strconv.FormatInt(n, 10) == s
}

func methodNotAllowed(w http.ResponseWriter, methods string) {
	w.Header().Set("Allow", methods)
	WriteError(w, 405, "METHOD_NOT_ALLOWED", "Metode permintaan tidak didukung.")
}

func (h *platformHandler) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, contracts.ErrForbidden):
		WriteError(w, 403, "FORBIDDEN", "Akses ditolak.")
	case errors.Is(err, platform.ErrNotFound):
		WriteError(w, 404, "NOT_FOUND", "Data tidak ditemukan.")
	case errors.Is(err, platform.ErrConflict):
		WriteError(w, 409, "CONFLICT", "Perubahan bertentangan dengan data saat ini.")
	case errors.Is(err, platform.ErrInvalidInput):
		WriteError(w, 422, "VALIDATION_FAILED", "Isian tidak valid.")
	default:
		WriteError(w, 503, "SERVICE_UNAVAILABLE", "Layanan sementara tidak tersedia.")
	}
}

func decodePlatform(w http.ResponseWriter, r *http.Request, dst any) bool {
	return decodeJSON(w, r, dst, 16<<10)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, limit int64) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		WriteError(w, 415, "UNSUPPORTED_MEDIA_TYPE", "Gunakan format JSON.")
		return false
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		WriteError(w, 413, "REQUEST_TOO_LARGE", "Permintaan terlalu besar.")
		return false
	}
	// Inspect top-level keys before decoding into a struct: encoding/json otherwise accepts duplicates.
	keys := json.NewDecoder(bytes.NewReader(data))
	tok, err := keys.Token()
	if err != nil || tok != json.Delim('{') {
		WriteError(w, 400, "INVALID_REQUEST", "Body JSON tidak valid.")
		return false
	}
	seen := map[string]bool{}
	for keys.More() {
		tok, err = keys.Token()
		if err != nil {
			break
		}
		key, ok := tok.(string)
		if !ok || seen[key] {
			err = errors.New("duplicate field")
			break
		}
		seen[key] = true
		var value json.RawMessage
		err = keys.Decode(&value)
		if err != nil {
			break
		}
		if bytes.Equal(value, []byte("null")) {
			err = errors.New("null field")
			break
		}
	}
	if err == nil {
		_, err = keys.Token()
	}
	if err != nil {
		WriteError(w, 400, "INVALID_REQUEST", "Body JSON tidak valid.")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(dst); err != nil {
		WriteError(w, 400, "INVALID_REQUEST", "Body JSON tidak valid.")
		return false
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		WriteError(w, 400, "INVALID_REQUEST", "Body JSON tidak valid.")
		return false
	}
	return true
}
