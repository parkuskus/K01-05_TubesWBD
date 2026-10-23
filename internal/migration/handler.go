package migration

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/a-h/templ"
	"if3110.local/db-platform/internal/auth"
	"if3110.local/db-platform/internal/contracts"
)

const maxMigrationFormBytes = 1 << 20 // 1 MB batas ukuran form sql migration

var ErrInvalidHandlerConfig = errors.New("migration handler requires a migration service")

// kontrak service yang dipakai http handler migration
type MigrationService interface {
	ListHistory(ctx context.Context, userID contracts.UserID, dbID contracts.DatabaseID) ([]MigrationRecord, error)
	GetMigration(ctx context.Context, userID contracts.UserID, dbID contracts.DatabaseID, version int64) (MigrationRecord, error)
	CreateDraft(ctx context.Context, userID contracts.UserID, dbID contracts.DatabaseID, title, sqlText string) (MigrationRecord, error)
	UpdateDraft(ctx context.Context, userID contracts.UserID, dbID contracts.DatabaseID, version int64, title, sqlText string) (MigrationRecord, error)
	ExecuteMigration(ctx context.Context, userID contracts.UserID, dbID contracts.DatabaseID, version int64) (MigrationRecord, error)
	RunMigration(ctx context.Context, userID contracts.UserID, dbID contracts.DatabaseID, title, sqlText string) (MigrationRecord, error)
}

type createAccessChecker interface {
	RequireCreateAccess(ctx context.Context, userID contracts.UserID, dbID contracts.DatabaseID) error
}

// http handler buat halaman dan aksi migration
type Handler struct {
	service     MigrationService
	currentUser func(*http.Request) (contracts.UserID, bool)
}

func NewHandler(service MigrationService) (*Handler, error) {
	if service == nil {
		return nil, ErrInvalidHandlerConfig
	}
	return &Handler{
		service:     service,
		currentUser: auth.CurrentUser,
	}, nil
}

// daftarkan semua route modul migration ke mux
func RegisterRoutes(mux *http.ServeMux, service MigrationService, wrap ...func(http.Handler) http.Handler) error {
	h, err := NewHandler(service)
	if err != nil {
		return err
	}
	h.RegisterRoutes(mux, wrap...)
	return nil
}

// pasang endpoint migration pada ServeMux
func (h *Handler) RegisterRoutes(mux *http.ServeMux, wrap ...func(http.Handler) http.Handler) {
	apply := func(handler http.HandlerFunc) http.Handler {
		var wrapped http.Handler = handler
		for i := len(wrap) - 1; i >= 0; i-- {
			if wrap[i] != nil {
				wrapped = wrap[i](wrapped)
			}
		}
		return wrapped
	}

	mux.Handle("GET /migrations", apply(h.redirectRootMigrations))
	mux.Handle("GET /databases/{databaseID}/migrations", apply(h.listHistory))
	mux.Handle("GET /databases/{databaseID}/migrations/new", apply(h.newMigrationPage))
	mux.Handle("POST /databases/{databaseID}/migrations", apply(h.createMigration))
	mux.Handle("GET /databases/{databaseID}/migrations/{version}", apply(h.getMigrationDetail))
	mux.Handle("POST /databases/{databaseID}/migrations/{version}/edit", apply(h.updateMigrationDraft))
	mux.Handle("POST /databases/{databaseID}/migrations/{version}/execute", apply(h.executeMigration))
}

func (h *Handler) redirectRootMigrations(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.resolveUser(r); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	dbID := strings.TrimSpace(r.URL.Query().Get("database_id"))
	if dbID == "" {
		http.Error(w, "parameter database_id diperlukan", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, historyURL(dbID), http.StatusSeeOther)
}

func (h *Handler) listHistory(w http.ResponseWriter, r *http.Request) {
	userID, dbID, ok := h.requireUserAndDatabase(w, r)
	if !ok {
		return
	}

	records, err := h.service.ListHistory(r.Context(), userID, dbID)
	if err != nil {
		h.writeServiceError(w, err)
		return
	}

	renderComponent(w, r, http.StatusOK, historyPage(HistoryPageData{
		DatabaseID: string(dbID),
		CSRFToken:  auth.CSRFToken(r),
		Records:    records,
	}))
}

func (h *Handler) newMigrationPage(w http.ResponseWriter, r *http.Request) {
	userID, dbID, ok := h.requireUserAndDatabase(w, r)
	if !ok {
		return
	}

	if checker, ok := h.service.(createAccessChecker); ok {
		if err := checker.RequireCreateAccess(r.Context(), userID, dbID); err != nil {
			h.writeServiceError(w, err)
			return
		}
	}

	renderComponent(w, r, http.StatusOK, editorPage(EditorPageData{
		DatabaseID: string(dbID),
		CSRFToken:  auth.CSRFToken(r),
	}))
}

func (h *Handler) createMigration(w http.ResponseWriter, r *http.Request) {
	userID, dbID, ok := h.requireUserAndDatabase(w, r)
	if !ok {
		return
	}

	if !parsePostForm(w, r) {
		return
	}

	title := r.PostForm.Get("title")
	sqlText := r.PostForm.Get("sql_text")
	action := strings.ToLower(strings.TrimSpace(r.PostForm.Get("action")))

	if action == "run" {
		record, err := h.service.RunMigration(r.Context(), userID, dbID, title, sqlText)
		if err != nil {
			if record.Version > 0 && record.Status == StatusFailed {
				renderComponent(w, r, http.StatusUnprocessableEntity, detailPage(DetailPageData{
					DatabaseID:   string(dbID),
					CSRFToken:    auth.CSRFToken(r),
					ErrorMessage: "Eksekusi SQL gagal dan perubahan telah di-rollback.",
					Record:       record,
				}))
				return
			}
			if errors.Is(err, ErrInvalidSQL) {
				renderComponent(w, r, http.StatusBadRequest, editorPage(EditorPageData{
					DatabaseID:   string(dbID),
					CSRFToken:    auth.CSRFToken(r),
					ErrorMessage: "Query SQL migration tidak boleh kosong.",
					Title:        title,
					SQLText:      sqlText,
				}))
				return
			}
			h.writeServiceError(w, err)
			return
		}
		http.Redirect(w, r, detailMigrationURL(string(dbID), record.Version), http.StatusSeeOther)
		return
	}

	record, err := h.service.CreateDraft(r.Context(), userID, dbID, title, sqlText)
	if err != nil {
		if errors.Is(err, ErrInvalidSQL) {
			renderComponent(w, r, http.StatusBadRequest, editorPage(EditorPageData{
				DatabaseID:   string(dbID),
				CSRFToken:    auth.CSRFToken(r),
				ErrorMessage: "Query SQL migration tidak boleh kosong.",
				Title:        title,
				SQLText:      sqlText,
			}))
			return
		}
		h.writeServiceError(w, err)
		return
	}

	http.Redirect(w, r, detailMigrationURL(string(dbID), record.Version), http.StatusSeeOther)
}

func (h *Handler) getMigrationDetail(w http.ResponseWriter, r *http.Request) {
	userID, dbID, version, ok := h.requireUserDatabaseAndVersion(w, r)
	if !ok {
		return
	}

	record, err := h.service.GetMigration(r.Context(), userID, dbID, version)
	if err != nil {
		h.writeServiceError(w, err)
		return
	}

	renderComponent(w, r, http.StatusOK, detailPage(DetailPageData{
		DatabaseID: string(dbID),
		CSRFToken:  auth.CSRFToken(r),
		Record:     record,
	}))
}

func (h *Handler) updateMigrationDraft(w http.ResponseWriter, r *http.Request) {
	userID, dbID, version, ok := h.requireUserDatabaseAndVersion(w, r)
	if !ok {
		return
	}

	if !parsePostForm(w, r) {
		return
	}

	title := r.PostForm.Get("title")
	sqlText := r.PostForm.Get("sql_text")

	record, err := h.service.UpdateDraft(r.Context(), userID, dbID, version, title, sqlText)
	if err != nil {
		if errors.Is(err, ErrInvalidSQL) {
			http.Error(w, "query sql migration tidak boleh kosong", http.StatusBadRequest)
			return
		}
		h.writeServiceError(w, err)
		return
	}

	http.Redirect(w, r, detailMigrationURL(string(dbID), record.Version), http.StatusSeeOther)
}

func (h *Handler) executeMigration(w http.ResponseWriter, r *http.Request) {
	userID, dbID, version, ok := h.requireUserDatabaseAndVersion(w, r)
	if !ok {
		return
	}

	record, err := h.service.ExecuteMigration(r.Context(), userID, dbID, version)
	if err != nil {
		if record.Version > 0 && record.Status == StatusFailed {
			renderComponent(w, r, http.StatusUnprocessableEntity, detailPage(DetailPageData{
				DatabaseID:   string(dbID),
				CSRFToken:    auth.CSRFToken(r),
				ErrorMessage: "Eksekusi SQL gagal dan perubahan telah di-rollback.",
				Record:       record,
			}))
			return
		}
		h.writeServiceError(w, err)
		return
	}

	http.Redirect(w, r, detailMigrationURL(string(dbID), record.Version), http.StatusSeeOther)
}

func (h *Handler) requireUserAndDatabase(w http.ResponseWriter, r *http.Request) (contracts.UserID, contracts.DatabaseID, bool) {
	userID, ok := h.resolveUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", "", false
	}
	dbID := strings.TrimSpace(r.PathValue("databaseID"))
	if dbID == "" {
		http.Error(w, "id database tidak valid", http.StatusBadRequest)
		return "", "", false
	}
	return userID, contracts.DatabaseID(dbID), true
}

func (h *Handler) resolveUser(r *http.Request) (contracts.UserID, bool) {
	if h.currentUser != nil {
		return h.currentUser(r)
	}
	return auth.CurrentUser(r)
}

func (h *Handler) requireUserDatabaseAndVersion(w http.ResponseWriter, r *http.Request) (contracts.UserID, contracts.DatabaseID, int64, bool) {
	userID, dbID, ok := h.requireUserAndDatabase(w, r)
	if !ok {
		return "", "", 0, false
	}
	versionStr := strings.TrimSpace(r.PathValue("version"))
	version, err := strconv.ParseInt(versionStr, 10, 64)
	if err != nil || version <= 0 {
		http.Error(w, "nomor versi migration tidak valid", http.StatusBadRequest)
		return "", "", 0, false
	}
	return userID, dbID, version, true
}

func (h *Handler) writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrUnauthenticated), errors.Is(err, ErrInvalidUser):
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	case errors.Is(err, contracts.ErrForbidden):
		http.Error(w, "akses ditolak", http.StatusForbidden)
	case errors.Is(err, ErrMigrationNotFound):
		http.Error(w, "migration tidak ditemukan", http.StatusNotFound)
	case errors.Is(err, ErrMigrationAlreadyApplied):
		http.Error(w, "migration sudah pernah dieksekusi", http.StatusConflict)
	case errors.Is(err, ErrInvalidSQL), errors.Is(err, ErrInvalidVersion), errors.Is(err, ErrInvalidDatabaseID):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, "layanan migration tidak tersedia sementara", http.StatusServiceUnavailable)
	}
}

func parsePostForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxMigrationFormBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "form tidak valid", http.StatusBadRequest)
		return false
	}
	return true
}

func renderComponent(w http.ResponseWriter, r *http.Request, status int, component templ.Component) {
	var body bytes.Buffer
	if err := component.Render(r.Context(), &body); err != nil {
		http.Error(w, "halaman tidak tersedia", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body.Bytes())
}

