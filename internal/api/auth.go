package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"

	"if3110.local/db-platform/internal/auth"
	"if3110.local/db-platform/internal/contracts"
)

const MaxAuthBodyBytes = 4096

type Authenticator interface {
	AuthenticateForSession(context.Context, string, string) (auth.LoginProof, error)
	Identity(context.Context, contracts.UserID) (auth.Identity, error)
}

type Sessions interface {
	Create(http.ResponseWriter, *http.Request, auth.LoginProof) error
	Logout(http.ResponseWriter, *http.Request) error
	RequireSessionWithError(http.Handler, func(http.ResponseWriter, *http.Request, error)) http.Handler
}

type AuthConfig struct {
	Authenticator Authenticator
	Sessions      Sessions
}

type authHandler struct {
	authenticator Authenticator
	sessions      Sessions
	me            http.Handler
	logout        http.Handler
}

type sessionUser struct {
	ID              contracts.UserID `json:"id"`
	Username        string           `json:"username"`
	IsPlatformAdmin bool             `json:"is_platform_admin"`
}

func NewAuthHandler(config AuthConfig) (http.Handler, error) {
	if config.Authenticator == nil || config.Sessions == nil {
		return nil, errors.New("auth API requires authentication and session services")
	}
	h := &authHandler{authenticator: config.Authenticator, sessions: config.Sessions}
	h.me = h.sessions.RequireSessionWithError(http.HandlerFunc(h.currentSession), h.sessionError)
	h.logout = h.sessions.RequireSessionWithError(http.HandlerFunc(h.endSession), h.sessionError)
	return h, nil
}

func (h *authHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var method string
	var handler http.Handler
	switch r.URL.Path {
	case "/api/v1/csrf":
		method, handler = http.MethodGet, http.HandlerFunc(h.csrf)
	case "/api/v1/auth/login":
		method, handler = http.MethodPost, http.HandlerFunc(h.login)
	case "/api/v1/auth/logout":
		method, handler = http.MethodPost, h.logout
	case "/api/v1/auth/me":
		method, handler = http.MethodGet, h.me
	default:
		WriteError(w, http.StatusNotFound, "NOT_FOUND", "Endpoint tidak ditemukan.")
		return
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		WriteError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Metode permintaan tidak didukung.")
		return
	}
	handler.ServeHTTP(w, r)
}

func (h *authHandler) csrf(w http.ResponseWriter, r *http.Request) {
	token := auth.CSRFToken(r)
	if token == "" {
		WriteError(w, http.StatusServiceUnavailable, "CSRF_UNAVAILABLE", "Sesi belum dapat disiapkan.")
		return
	}
	writeData(w, struct {
		Token string `json:"csrf_token"`
	}{Token: token})
}

func (h *authHandler) login(w http.ResponseWriter, r *http.Request) {
	if auth.CSRFToken(r) == "" {
		WriteError(w, http.StatusForbidden, "CSRF_INVALID", "Sesi formulir tidak valid. Muat ulang halaman.")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		WriteError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Gunakan format JSON.")
		return
	}
	if r.URL.RawQuery != "" {
		WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "Kirim kredensial melalui body permintaan.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxAuthBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var input *struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decoder.Decode(&input); err != nil {
		writeLoginDecodeError(w, err)
		return
	}
	if input == nil {
		WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "Permintaan login tidak valid.")
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeLoginDecodeError(w, err)
		return
	}
	fields := make(map[string]string)
	input.Username = strings.TrimSpace(input.Username)
	if input.Username == "" || len(input.Username) > 64 || !utf8.ValidString(input.Username) {
		fields["username"] = "Isi username yang valid, maksimal 64 byte."
	}
	if input.Password == "" || len(input.Password) > 72 || !utf8.ValidString(input.Password) {
		fields["password"] = "Isi password yang valid, maksimal 72 byte."
	}
	if len(fields) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, struct {
			Error errorDetail `json:"error"`
		}{Error: errorDetail{Code: "VALIDATION_FAILED", Message: "Periksa isian login.", FieldErrors: fields}})
		return
	}
	proof, err := h.authenticator.AuthenticateForSession(r.Context(), input.Username, input.Password)
	if errors.Is(err, auth.ErrInvalidCredentials) {
		WriteError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "Username atau password salah.")
		return
	}
	if err != nil {
		h.sessionError(w, r, err)
		return
	}
	identity, err := h.authenticator.Identity(r.Context(), proof.UserID)
	if err != nil {
		h.sessionError(w, r, err)
		return
	}
	if err := h.sessions.Create(w, r, proof); err != nil {
		h.sessionError(w, r, err)
		return
	}
	writeSession(w, identity)
}

func writeLoginDecodeError(w http.ResponseWriter, err error) {
	var limit *http.MaxBytesError
	if errors.As(err, &limit) {
		WriteError(w, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "Permintaan login terlalu besar.")
		return
	}
	WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "Permintaan login tidak valid.")
}

func (h *authHandler) currentSession(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.CurrentUser(r)
	if !ok {
		h.sessionError(w, r, auth.ErrUnauthenticated)
		return
	}
	identity, err := h.authenticator.Identity(r.Context(), id)
	if err != nil {
		h.sessionError(w, r, err)
		return
	}
	writeSession(w, identity)
}

func writeSession(w http.ResponseWriter, identity auth.Identity) {
	writeData(w, struct {
		User sessionUser `json:"user"`
	}{User: sessionUser{ID: identity.ID, Username: identity.Username, IsPlatformAdmin: identity.IsPlatformAdmin}})
}

func (h *authHandler) endSession(w http.ResponseWriter, r *http.Request) {
	if auth.CSRFToken(r) == "" {
		WriteError(w, http.StatusForbidden, "CSRF_INVALID", "Sesi formulir tidak valid. Muat ulang halaman.")
		return
	}
	if err := h.sessions.Logout(w, r); err != nil {
		h.sessionError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *authHandler) sessionError(w http.ResponseWriter, _ *http.Request, err error) {
	if errors.Is(err, auth.ErrUnauthenticated) {
		WriteError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "Sesi Anda berakhir. Silakan masuk kembali.")
		return
	}
	WriteError(w, http.StatusServiceUnavailable, "AUTH_UNAVAILABLE", "Layanan login sementara tidak tersedia.")
}
