package web

import (
	"context"
	"errors"
	"net/http"

	"if3110.local/db-platform/internal/auth"
)

type Authenticator interface {
	AuthenticateForSession(ctx context.Context, username, password string) (auth.LoginProof, error)
}

type SessionManager interface {
	Create(w http.ResponseWriter, r *http.Request, proof auth.LoginProof) error
	RequireSession(next http.Handler) http.Handler
	Logout(w http.ResponseWriter, r *http.Request) error
}

type Config struct {
	Authenticator Authenticator
	Sessions      SessionManager
	CSRFKey       []byte
	SecureCookies bool
}

var ErrInvalidConfig = errors.New("web server requires auth services")

func New(config Config) (http.Handler, error) {
	if config.Authenticator == nil || config.Sessions == nil {
		return nil, ErrInvalidConfig
	}
	mux := http.NewServeMux()
	handlers := authHandlers{authenticator: config.Authenticator, sessions: config.Sessions}
	mux.HandleFunc("GET /login", handlers.loginPage)
	mux.HandleFunc("POST /login", handlers.login)
	mux.Handle("POST /logout", config.Sessions.RequireSession(http.HandlerFunc(handlers.logout)))
	mux.Handle("GET /", config.Sessions.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := home(auth.CSRFToken(r)).Render(r.Context(), w); err != nil {
			http.Error(w, "page unavailable", http.StatusInternalServerError)
		}
	})))
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("web/static"))))
	return auth.CSRFProtection(config.CSRFKey, config.SecureCookies, mux)
}
