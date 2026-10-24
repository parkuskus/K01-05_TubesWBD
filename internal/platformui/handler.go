package platformui

import (
	"bytes"
	"context"
	"errors"
	"mime"
	"net/http"
	"strconv"

	"github.com/a-h/templ"
	"if3110.local/db-platform/internal/auth"
	"if3110.local/db-platform/internal/contracts"
	"if3110.local/db-platform/internal/platform"
)

type AdminService interface {
	ListUsers(context.Context, contracts.UserID) ([]platform.User, error)
	CreateUser(context.Context, contracts.UserID, string, string) (contracts.UserID, error)
	UpdateUsername(context.Context, contracts.UserID, contracts.UserID, string) error
	SetPassword(context.Context, contracts.UserID, contracts.UserID, string) error
	SetPlatformAdmin(context.Context, contracts.UserID, contracts.UserID, bool) error
	DeleteUser(context.Context, contracts.UserID, contracts.UserID) error
	ListGroups(context.Context, contracts.UserID) ([]platform.Group, error)
	CreateGroup(context.Context, contracts.UserID, string) (contracts.GroupID, error)
	RenameGroup(context.Context, contracts.UserID, contracts.GroupID, string) error
	DeleteGroup(context.Context, contracts.UserID, contracts.GroupID) error
	ListMembers(context.Context, contracts.UserID, contracts.GroupID) ([]platform.User, error)
	SetMembership(context.Context, contracts.UserID, contracts.UserID, contracts.GroupID, bool) error
	ListGrants(context.Context, contracts.UserID, contracts.GroupID) ([]platform.Grant, error)
	SetGrant(context.Context, contracts.UserID, contracts.GroupID, contracts.DatabaseID, contracts.AccessLevel) error
}

type Handler struct {
	service     AdminService
	authorizer  contracts.Authorizer
	layout      func(string, templ.Component) templ.Component
	currentUser func(*http.Request) (contracts.UserID, bool)
	routes      *http.ServeMux
}

var ErrInvalidConfig = errors.New("platform UI requires service, authorizer and layout")

// Mount under /platform/ inside the application's session and CSRF middleware.
func NewHandler(service AdminService, authorizer contracts.Authorizer, layout func(string, templ.Component) templ.Component) (*Handler, error) {
	if service == nil || authorizer == nil || layout == nil {
		return nil, ErrInvalidConfig
	}
	h := &Handler{service: service, authorizer: authorizer, layout: layout, currentUser: auth.CurrentUser, routes: http.NewServeMux()}
	h.routes.HandleFunc("GET /platform/users", h.users)
	h.routes.HandleFunc("POST /platform/users", h.users)
	h.routes.HandleFunc("POST /platform/users/{id}/{action}", h.userAction)
	h.routes.HandleFunc("GET /platform/groups", h.groups)
	h.routes.HandleFunc("POST /platform/groups", h.groups)
	h.routes.HandleFunc("GET /platform/groups/{id}", h.groupDetail)
	h.routes.HandleFunc("POST /platform/groups/{id}/{action}", h.groupAction)
	return h, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	actor, ok := h.currentUser(r)
	if !ok || actor == "" {
		http.Error(w, "Silakan login terlebih dahulu.", http.StatusUnauthorized)
		return
	}
	if err := h.authorizer.RequirePlatformAdmin(r.Context(), actor); err != nil {
		h.fail(w, err)
		return
	}
	if r.Method == http.MethodPost && auth.CSRFToken(r) == "" {
		http.Error(w, "Form tidak valid.", http.StatusForbidden)
		return
	}
	h.routes.ServeHTTP(w, r)
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	status, message := http.StatusServiceUnavailable, "Layanan sementara tidak tersedia."
	switch {
	case errors.Is(err, contracts.ErrForbidden):
		status, message = http.StatusForbidden, "Akses ditolak."
	case errors.Is(err, platform.ErrInvalidInput):
		status, message = http.StatusBadRequest, "Input tidak valid."
	case errors.Is(err, platform.ErrNotFound):
		status, message = http.StatusNotFound, "Data tidak ditemukan."
	case errors.Is(err, platform.ErrConflict):
		status, message = http.StatusConflict, "Data bertentangan dengan data yang sudah ada."
	}
	http.Error(w, message, status)
}

func (h *Handler) render(w http.ResponseWriter, r *http.Request, title string, content templ.Component) {
	var body bytes.Buffer
	if err := h.layout(title, content).Render(r.Context(), &body); err != nil {
		http.Error(w, "Halaman tidak tersedia.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(body.Bytes())
}

func form(w http.ResponseWriter, r *http.Request) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if r.URL.RawQuery != "" || r.ContentLength > 16384 || err != nil || mediaType != "application/x-www-form-urlencoded" {
		return platform.ErrInvalidInput
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	if err := r.ParseForm(); err != nil || len(r.PostForm.Encode()) > 16384 {
		return platform.ErrInvalidInput
	}
	return nil
}

func positiveID(raw string) error {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return platform.ErrInvalidInput
	}
	return nil
}

func redirect(w http.ResponseWriter, r *http.Request, path string) {
	http.Redirect(w, r, path, http.StatusSeeOther)
}

func (h *Handler) users(w http.ResponseWriter, r *http.Request) {
	actor, _ := h.currentUser(r)
	if r.Method == http.MethodPost {
		if err := form(w, r); err != nil {
			h.fail(w, err)
			return
		}
		_, err := h.service.CreateUser(r.Context(), actor, r.PostForm.Get("username"), r.PostForm.Get("password"))
		if err != nil {
			h.fail(w, err)
			return
		}
		redirect(w, r, "/platform/users")
		return
	}
	users, err := h.service.ListUsers(r.Context(), actor)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.render(w, r, "Kelola pengguna", usersPage(users, auth.CSRFToken(r)))
}

func (h *Handler) userAction(w http.ResponseWriter, r *http.Request) {
	actor, _ := h.currentUser(r)
	id := r.PathValue("id")
	if err := positiveID(id); err != nil {
		h.fail(w, err)
		return
	}
	if err := form(w, r); err != nil {
		h.fail(w, err)
		return
	}
	var err error
	switch r.PathValue("action") {
	case "username":
		err = h.service.UpdateUsername(r.Context(), actor, contracts.UserID(id), r.PostForm.Get("username"))
	case "password":
		err = h.service.SetPassword(r.Context(), actor, contracts.UserID(id), r.PostForm.Get("password"))
	case "admin":
		if r.PostForm.Get("admin") != "true" && r.PostForm.Get("admin") != "false" {
			err = platform.ErrInvalidInput
			break
		}
		err = h.service.SetPlatformAdmin(r.Context(), actor, contracts.UserID(id), r.PostForm.Get("admin") == "true")
	case "delete":
		err = h.service.DeleteUser(r.Context(), actor, contracts.UserID(id))
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	redirect(w, r, "/platform/users")
}

func (h *Handler) groups(w http.ResponseWriter, r *http.Request) {
	actor, _ := h.currentUser(r)
	if r.Method == http.MethodPost {
		if err := form(w, r); err != nil {
			h.fail(w, err)
			return
		}
		_, err := h.service.CreateGroup(r.Context(), actor, r.PostForm.Get("name"))
		if err != nil {
			h.fail(w, err)
			return
		}
		redirect(w, r, "/platform/groups")
		return
	}
	groups, err := h.service.ListGroups(r.Context(), actor)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.render(w, r, "Kelola grup", groupsPage(groups, auth.CSRFToken(r)))
}

func (h *Handler) groupDetail(w http.ResponseWriter, r *http.Request) {
	actor, _ := h.currentUser(r)
	id := r.PathValue("id")
	if err := positiveID(id); err != nil {
		h.fail(w, err)
		return
	}
	group := contracts.GroupID(id)
	groups, err := h.service.ListGroups(r.Context(), actor)
	if err != nil {
		h.fail(w, err)
		return
	}
	var name string
	for _, g := range groups {
		if g.ID == group {
			name = g.Name
			break
		}
	}
	if name == "" {
		h.fail(w, platform.ErrNotFound)
		return
	}
	members, err := h.service.ListMembers(r.Context(), actor, group)
	if err != nil {
		h.fail(w, err)
		return
	}
	grants, err := h.service.ListGrants(r.Context(), actor, group)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.render(w, r, "Kelola grup "+name, groupPage(id, name, members, grants, auth.CSRFToken(r)))
}

func (h *Handler) groupAction(w http.ResponseWriter, r *http.Request) {
	actor, _ := h.currentUser(r)
	id := r.PathValue("id")
	if err := positiveID(id); err != nil {
		h.fail(w, err)
		return
	}
	if err := form(w, r); err != nil {
		h.fail(w, err)
		return
	}
	group := contracts.GroupID(id)
	var err error
	switch r.PathValue("action") {
	case "name":
		err = h.service.RenameGroup(r.Context(), actor, group, r.PostForm.Get("name"))
	case "delete":
		err = h.service.DeleteGroup(r.Context(), actor, group)
	case "members":
		user := r.PostForm.Get("user_id")
		if err = positiveID(user); err != nil {
			break
		}
		member := r.PostForm.Get("member")
		if member != "true" && member != "false" {
			err = platform.ErrInvalidInput
			break
		}
		err = h.service.SetMembership(r.Context(), actor, contracts.UserID(user), group, member == "true")
	case "grants":
		database := r.PostForm.Get("database_id")
		if err = positiveID(database); err != nil {
			break
		}
		var level int
		level, err = strconv.Atoi(r.PostForm.Get("level"))
		if err != nil || level < int(contracts.AccessNone) || level > int(contracts.AccessViewEdit) {
			err = platform.ErrInvalidInput
			break
		}
		err = h.service.SetGrant(r.Context(), actor, group, contracts.DatabaseID(database), contracts.AccessLevel(level))
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	if r.PathValue("action") == "delete" {
		redirect(w, r, "/platform/groups")
	} else {
		redirect(w, r, "/platform/groups/"+id)
	}
}
