package database

import (
	"bytes"
	"context"
	"errors"
	"if3110.local/db-platform/internal/migration"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"github.com/a-h/templ"
	"if3110.local/db-platform/internal/auth"
	"if3110.local/db-platform/internal/contracts"
)

type viewerReader interface {
	Schemas(context.Context, contracts.UserID, contracts.DatabaseID) ([]string, error)
	Diff(context.Context, contracts.UserID, contracts.DatabaseID, string, string) (SchemaDiff, error)
	Tables(context.Context, contracts.UserID, contracts.DatabaseID, string) ([]Table, error)
	Details(context.Context, contracts.UserID, contracts.DatabaseID, string, string) (TableDetails, error)
	Data(context.Context, contracts.UserID, contracts.DatabaseID, string, string, int) (DataPage, error)
}

type draftCreator interface {
	CreateDraft(context.Context, contracts.UserID, contracts.DatabaseID, string, string) (migration.MigrationRecord, error)
}

type handlers struct {
	drafts      draftCreator
	manager     *Manager
	viewer      viewerReader
	layout      func(string, templ.Component) templ.Component
	currentUser func(*http.Request) (contracts.UserID, bool)
}

type pageNavigation struct {
	Admin    bool
	Database DatabaseChoice
	Schema   string
	Table    string
	View     string
}

func (h *handlers) isAdmin(ctx context.Context, actor contracts.UserID) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	err := h.manager.requireAdmin(ctx, actor)
	if errors.Is(err, contracts.ErrForbidden) {
		return false, nil
	}
	return err == nil, err
}

func (h *handlers) navigation(r *http.Request, actor contracts.UserID, action contracts.DatabaseAction, view string) (pageNavigation, error) {
	choice, err := h.manager.describe(r.Context(), actor, contracts.DatabaseID(r.PathValue("id")), action)
	if err != nil {
		return pageNavigation{}, err
	}
	admin, err := h.isAdmin(r.Context(), actor)
	if err != nil {
		return pageNavigation{}, err
	}
	return pageNavigation{Admin: admin, Database: choice, Schema: r.URL.Query().Get("schema"), Table: r.URL.Query().Get("table"), View: view}, nil
}

// NewHandler returns routes under /databases/. Mount without StripPrefix, inside
// the central session and CSRF middleware. layout is supplied by internal/ui's
// owner; this module only renders page content and never imports internal/web.
func NewHandler(manager *Manager, layout func(string, templ.Component) templ.Component, drafts ...draftCreator) (http.Handler, error) {
	if manager == nil || layout == nil {
		return nil, ErrInvalidConfig
	}
	viewer, err := NewViewer(manager.authorizer, manager)
	if err != nil {
		return nil, err
	}
	h := &handlers{manager: manager, viewer: viewer, layout: layout, currentUser: auth.CurrentUser}
	if len(drafts) > 0 {
		h.drafts = drafts[0]
	}
	return h.routes(), nil
}

func (h *handlers) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /databases/{$}", h.withUser(h.databaseList))
	mux.HandleFunc("GET /databases/connections/{$}", h.withUser(h.connectionList))
	mux.HandleFunc("GET /databases/connections/new", h.withUser(h.newConnection))
	mux.HandleFunc("POST /databases/connections/{$}", h.withUser(h.createConnection))
	mux.HandleFunc("GET /databases/connections/{id}/edit", h.withUser(h.editConnection))
	mux.HandleFunc("POST /databases/connections/{id}/edit", h.withUser(h.updateConnection))
	mux.HandleFunc("GET /databases/connections/{id}/delete", h.withUser(h.deletePage))
	mux.HandleFunc("POST /databases/connections/{id}/delete", h.withUser(h.deleteConnection))
	mux.HandleFunc("GET /databases/{id}/schemas", h.withUser(h.schemas))
	mux.HandleFunc("GET /databases/{id}/schemas/diff", h.withUser(h.schemaDiff))
	mux.HandleFunc("POST /databases/{id}/schemas/diff", h.withUser(h.createDiffDraft))
	mux.HandleFunc("GET /databases/{id}/tables", h.withUser(h.tables))
	mux.HandleFunc("GET /databases/{id}/table", h.withUser(h.details))
	mux.HandleFunc("GET /databases/{id}/data", h.withUser(h.data))
	return mux
}

func (h *handlers) withUser(next func(http.ResponseWriter, *http.Request, contracts.UserID)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		actor, ok := h.currentUser(r)
		if !ok || actor == "" {
			h.fail(w, r, auth.ErrUnauthenticated)
			return
		}
		// Fail closed if the integrator forgets the central CSRF middleware.
		if r.Method == http.MethodPost && auth.CSRFToken(r) == "" {
			http.Error(w, "Form tidak valid. Muat ulang halaman.", http.StatusForbidden)
			return
		}
		next(w, r, actor)
	}
}

func (h *handlers) render(w http.ResponseWriter, r *http.Request, status int, title string, content templ.Component) {
	var body bytes.Buffer
	if err := h.layout(title, content).Render(r.Context(), &body); err != nil {
		http.Error(w, "Halaman sementara tidak tersedia.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body.Bytes())
}

func errorResponse(err error) (int, string) {
	switch {
	case errors.Is(err, auth.ErrUnauthenticated):
		return http.StatusUnauthorized, "Sesi Anda berakhir atau belum masuk. Silakan login kembali."
	case errors.Is(err, contracts.ErrForbidden):
		return http.StatusForbidden, "Anda tidak memiliki akses untuk tindakan ini."
	case errors.Is(err, ErrInvalidInput):
		return http.StatusBadRequest, "Input tidak valid. Periksa kembali isian Anda."
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, "Database atau objek tidak ditemukan."
	case errors.Is(err, ErrConflict):
		return http.StatusConflict, "Nama koneksi sudah digunakan."
	case errors.Is(err, ErrInUse):
		return http.StatusConflict, "Koneksi masih digunakan dan belum dapat dihapus."
	default:
		return http.StatusServiceUnavailable, "Database sementara tidak tersedia. Coba lagi nanti."
	}
}
func (h *handlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, message := errorResponse(err)
	h.render(w, r, status, "Permintaan gagal", messagePage(message, status == http.StatusUnauthorized))
}

func (h *handlers) connectionList(w http.ResponseWriter, r *http.Request, actor contracts.UserID) {
	items, err := h.manager.List(r.Context(), actor)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	message := ""
	switch r.URL.Query().Get("result") {
	case "created":
		message = "Koneksi berhasil ditambahkan."
	case "updated":
		message = "Koneksi berhasil diperbarui."
	case "deleted":
		message = "Koneksi berhasil dihapus."
	}
	h.render(w, r, 200, "Kelola koneksi", connectionsPage(items, message))
}

func (h *handlers) newConnection(w http.ResponseWriter, r *http.Request, actor contracts.UserID) {
	if err := h.manager.requireAdmin(r.Context(), actor); err != nil {
		h.fail(w, r, err)
		return
	}
	h.render(w, r, 200, "Tambah koneksi", connectionForm("", Connection{Port: 5432, SSLMode: "disable"}, auth.CSRFToken(r), "", nil))
}
func (h *handlers) editConnection(w http.ResponseWriter, r *http.Request, actor contracts.UserID) {
	c, err := h.manager.Get(r.Context(), actor, contracts.DatabaseID(r.PathValue("id")))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.render(w, r, 200, "Edit koneksi", connectionForm(c.ID, c, auth.CSRFToken(r), "", nil))
}

func parseConnectionForm(w http.ResponseWriter, r *http.Request, edit bool) (ConnectionInput, error) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" || r.URL.RawQuery != "" || r.ContentLength > 16384 {
		return ConnectionInput{}, ErrInvalidInput
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	if err := r.ParseForm(); err != nil {
		return ConnectionInput{}, ErrInvalidInput
	}
	// CSRF middleware may already have parsed PostForm, so also check its size.
	if len(r.PostForm.Encode()) > 16384 {
		return ConnectionInput{}, ErrInvalidInput
	}
	port, _ := strconv.Atoi(r.PostForm.Get("port"))
	in := ConnectionInput{Name: r.PostForm.Get("name"), Host: r.PostForm.Get("host"), Port: port, Database: r.PostForm.Get("database"), Username: r.PostForm.Get("username"), SSLMode: r.PostForm.Get("sslmode")}
	password := r.PostForm.Get("password")
	if !edit || password != "" {
		in.Password = &password
	}
	return in, nil
}

func (h *handlers) createConnection(w http.ResponseWriter, r *http.Request, actor contracts.UserID) {
	h.save(w, r, actor, false)
}
func (h *handlers) updateConnection(w http.ResponseWriter, r *http.Request, actor contracts.UserID) {
	h.save(w, r, actor, true)
}
func (h *handlers) save(w http.ResponseWriter, r *http.Request, actor contracts.UserID, edit bool) {
	if err := h.manager.requireAdmin(r.Context(), actor); err != nil {
		h.fail(w, r, err)
		return
	}
	in, err := parseConnectionForm(w, r, edit)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	id := contracts.DatabaseID(r.PathValue("id"))
	if edit {
		_, err = h.manager.Update(r.Context(), actor, id, in)
	} else {
		_, err = h.manager.Create(r.Context(), actor, in)
	}
	if err != nil {
		if errors.Is(err, ErrInvalidInput) || errors.Is(err, ErrConflict) {
			status, message := errorResponse(err)
			_, fields := in.validateFields(!edit)
			if errors.Is(err, ErrConflict) {
				fields["name"] = "Nama koneksi sudah digunakan. Pilih nama lain."
			}
			c := Connection{Name: in.Name, Host: in.Host, Port: in.Port, Database: in.Database, Username: in.Username, SSLMode: in.SSLMode}
			if fields["host"] != "" {
				// A pasted DSN can contain a password; never echo invalid host input.
				c.Host = ""
			}
			h.render(w, r, status, "Simpan koneksi", connectionForm(id, c, auth.CSRFToken(r), message, fields))
			return
		}
		h.fail(w, r, err)
		return
	}
	result := "created"
	if edit {
		result = "updated"
	}
	http.Redirect(w, r, "/databases/connections/?result="+result, http.StatusSeeOther)
}

func (h *handlers) deletePage(w http.ResponseWriter, r *http.Request, actor contracts.UserID) {
	c, err := h.manager.Get(r.Context(), actor, contracts.DatabaseID(r.PathValue("id")))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.render(w, r, 200, "Hapus koneksi", deleteConfirmation(c, auth.CSRFToken(r)))
}
func (h *handlers) deleteConnection(w http.ResponseWriter, r *http.Request, actor contracts.UserID) {
	if r.URL.RawQuery != "" {
		h.fail(w, r, ErrInvalidInput)
		return
	}
	if err := h.manager.Delete(r.Context(), actor, contracts.DatabaseID(r.PathValue("id"))); err != nil {
		h.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/databases/connections/?result=deleted", http.StatusSeeOther)
}

func (h *handlers) databaseList(w http.ResponseWriter, r *http.Request, actor contracts.UserID) {
	items, err := h.manager.Accessible(r.Context(), actor)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	admin, err := h.isAdmin(r.Context(), actor)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.render(w, r, 200, "Database", databasesPage(items, admin))
}
func (h *handlers) schemas(w http.ResponseWriter, r *http.Request, actor contracts.UserID) {
	nav, err := h.navigation(r, actor, contracts.ActionViewSchema, "schemas")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	id := contracts.DatabaseID(r.PathValue("id"))
	items, err := h.viewer.Schemas(r.Context(), actor, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.render(w, r, 200, "Schema · "+nav.Database.Name, schemasPage(nav, items))
}

func (h *handlers) schemaDiff(w http.ResponseWriter, r *http.Request, actor contracts.UserID) {
	q := r.URL.Query()
	reference, target := q.Get("reference"), q.Get("target")
	if reference != "" || target != "" {
		if !visibleSchema(reference) || !visibleSchema(target) || reference == target {
			h.fail(w, r, ErrInvalidInput)
			return
		}
	}
	nav, err := h.navigation(r, actor, contracts.ActionViewSchema, "diff")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	id := nav.Database.ID
	schemas, err := h.viewer.Schemas(r.Context(), actor, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var diff SchemaDiff
	if reference != "" {
		if !slices.Contains(schemas, reference) || !slices.Contains(schemas, target) {
			h.fail(w, r, ErrNotFound)
			return
		}
		diff, err = h.viewer.Diff(r.Context(), actor, id, reference, target)
		if err != nil {
			h.fail(w, r, err)
			return
		}
	}
	h.render(w, r, 200, "Bandingkan schema · "+nav.Database.Name, schemaDiffPage(nav, schemas, reference, target, diff, auth.CSRFToken(r), h.drafts != nil))
}

func (h *handlers) createDiffDraft(w http.ResponseWriter, r *http.Request, actor contracts.UserID) {
	if h.drafts == nil {
		h.fail(w, r, ErrUnavailable)
		return
	}
	id := contracts.DatabaseID(r.PathValue("id"))
	if !validID(id) {
		h.fail(w, r, ErrInvalidInput)
		return
	}
	if err := h.manager.authorizer.RequireDatabaseAccess(r.Context(), actor, id, contracts.ActionCreateMigration); err != nil {
		h.fail(w, r, err)
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" || r.URL.RawQuery != "" || r.ContentLength > 16384 {
		h.fail(w, r, ErrInvalidInput)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	if err := r.ParseForm(); err != nil || len(r.PostForm.Encode()) > 16384 {
		h.fail(w, r, ErrInvalidInput)
		return
	}
	reference, target := r.PostForm.Get("reference"), r.PostForm.Get("target")
	if !visibleSchema(reference) || !visibleSchema(target) || reference == target {
		h.fail(w, r, ErrInvalidInput)
		return
	}
	schemas, err := h.viewer.Schemas(r.Context(), actor, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if !slices.Contains(schemas, reference) || !slices.Contains(schemas, target) {
		h.fail(w, r, ErrNotFound)
		return
	}
	diff, err := h.viewer.Diff(r.Context(), actor, id, reference, target)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if len(diff.Changes) == 0 || diff.SQL == "" {
		h.fail(w, r, ErrInvalidInput)
		return
	}
	record, err := h.drafts.CreateDraft(r.Context(), actor, id, "Schema "+reference+" → "+target, diff.SQL)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	http.Redirect(w, r, viewerURL(id, "migrations/"+strconv.FormatInt(record.Version, 10), "", "", 0), http.StatusSeeOther)
}
func (h *handlers) tables(w http.ResponseWriter, r *http.Request, actor contracts.UserID) {
	nav, err := h.navigation(r, actor, contracts.ActionViewSchema, "tables")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	id := contracts.DatabaseID(r.PathValue("id"))
	schema := r.URL.Query().Get("schema")
	items, err := h.viewer.Tables(r.Context(), actor, id, schema)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.render(w, r, 200, "Tabel · "+nav.Database.Name, tablesPage(nav, schema, items))
}
func (h *handlers) details(w http.ResponseWriter, r *http.Request, actor contracts.UserID) {
	nav, err := h.navigation(r, actor, contracts.ActionViewSchema, "table")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	id := contracts.DatabaseID(r.PathValue("id"))
	q := r.URL.Query()
	detail, err := h.viewer.Details(r.Context(), actor, id, q.Get("schema"), q.Get("table"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.render(w, r, 200, "Struktur tabel · "+nav.Database.Name, tablePage(nav, detail))
}
func (h *handlers) data(w http.ResponseWriter, r *http.Request, actor contracts.UserID) {
	nav, err := h.navigation(r, actor, contracts.ActionViewData, "data")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	id := contracts.DatabaseID(r.PathValue("id"))
	q := r.URL.Query()
	page := 1
	if raw := q.Get("page"); raw != "" {
		var err error
		page, err = strconv.Atoi(raw)
		if err != nil {
			h.fail(w, r, ErrInvalidInput)
			return
		}
	}
	result, err := h.viewer.Data(r.Context(), actor, id, q.Get("schema"), q.Get("table"), page)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.render(w, r, 200, "Data tabel · "+nav.Database.Name, dataPage(nav, q.Get("schema"), q.Get("table"), result))
}

func viewerURL(id contracts.DatabaseID, view, schema, table string, page int) string {
	q := url.Values{}
	if schema != "" {
		q.Set("schema", schema)
	}
	if table != "" {
		q.Set("table", table)
	}
	if page > 0 {
		q.Set("page", strconv.Itoa(page))
	}
	path := "/databases/" + url.PathEscape(string(id)) + "/" + view
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return path
}
func connectionURL(id contracts.DatabaseID, action string) string {
	return "/databases/connections/" + url.PathEscape(string(id)) + "/" + action
}
