package web

import (
	"bytes"
	"errors"
	"net/http"

	"if3110.local/db-platform/internal/auth"
)

type authHandlers struct {
	authenticator Authenticator
	sessions      SessionManager
}

func (h authHandlers) loginPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	h.renderLogin(w, r, http.StatusOK, "")
}

func (h authHandlers) login(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if query.Has("username") || query.Has("password") {
		h.renderLogin(w, r, http.StatusBadRequest, "Kirim kredensial melalui form login.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		h.renderLogin(w, r, http.StatusBadRequest, "Form tidak valid. Coba lagi.")
		return
	}

	userID, err := h.authenticator.Authenticate(r.Context(), r.PostForm.Get("username"), r.PostForm.Get("password"))
	if errors.Is(err, auth.ErrInvalidCredentials) {
		h.renderLogin(w, r, http.StatusUnauthorized, "Username atau password salah.")
		return
	}
	if err != nil {
		http.Error(w, "Login sementara tidak tersedia.", http.StatusServiceUnavailable)
		return
	}
	if err := h.sessions.Create(w, r, userID); err != nil {
		http.Error(w, "Login sementara tidak tersedia.", http.StatusServiceUnavailable)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h authHandlers) logout(w http.ResponseWriter, r *http.Request) {
	if err := h.sessions.Logout(w, r); err != nil {
		http.Error(w, "Logout sementara tidak tersedia.", http.StatusServiceUnavailable)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (authHandlers) renderLogin(w http.ResponseWriter, r *http.Request, status int, message string) {
	var body bytes.Buffer
	if err := login(auth.CSRFToken(r), message).Render(r.Context(), &body); err != nil {
		http.Error(w, "Login page unavailable.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body.Bytes())
}
