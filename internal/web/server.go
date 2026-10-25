package web

import (
	"bytes"
	"context"
	"errors"
	"net/http"

	"if3110.local/db-platform/internal/auth"
	"if3110.local/db-platform/internal/contracts"
	"if3110.local/db-platform/internal/database"
	"if3110.local/db-platform/internal/migration"
	"if3110.local/db-platform/internal/platformui"
	"if3110.local/db-platform/internal/ui"
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
	Authorizer    contracts.Authorizer
	Platform      platformui.AdminService
	Databases     *database.Manager
	Migrations    migration.MigrationService
	CSRFKey       []byte
	SecureCookies bool
}

var ErrInvalidConfig = errors.New("web server requires auth services")

func New(config Config) (http.Handler, error) {
	if config.Authenticator == nil || config.Sessions == nil {
		return nil, ErrInvalidConfig
	}
	if config.Authorizer == nil && config.Platform != nil {
		if authorizer, ok := config.Platform.(contracts.Authorizer); ok {
			config.Authorizer = authorizer
		}
	}

	mux := http.NewServeMux()
	handlers := authHandlers{authenticator: config.Authenticator, sessions: config.Sessions}
	mux.HandleFunc("GET /login", handlers.loginPage)
	mux.HandleFunc("POST /login", handlers.login)
	mux.Handle("POST /logout", config.Sessions.RequireSession(http.HandlerFunc(handlers.logout)))
	mux.Handle("GET /{$}", config.Sessions.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		isAdmin := false
		if config.Authorizer != nil {
			actor, ok := auth.CurrentUser(r)
			if !ok || actor == "" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if err := config.Authorizer.RequirePlatformAdmin(r.Context(), actor); err == nil {
				isAdmin = true
			} else if !errors.Is(err, contracts.ErrForbidden) {
				http.Error(w, "page unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		var body bytes.Buffer
		if err := home(auth.CSRFToken(r), isAdmin).Render(r.Context(), &body); err != nil {
			http.Error(w, "page unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(body.Bytes())
	})))

	if config.Platform != nil {
		platformHandler, err := platformui.NewHandler(config.Platform, config.Authorizer, ui.Layout)
		if err != nil {
			return nil, err
		}
		mux.Handle("GET /platform/", config.Sessions.RequireSession(platformHandler))
		mux.Handle("POST /platform/", config.Sessions.RequireSession(platformHandler))
	}

	if config.Migrations != nil {
		if err := migration.RegisterRoutes(mux, config.Migrations, config.Sessions.RequireSession); err != nil {
			return nil, err
		}
	}

	if config.Databases != nil {
		databaseHandler, err := database.NewHandler(config.Databases, ui.Layout, config.Migrations)
		if err != nil {
			return nil, err
		}
		mux.Handle("GET /databases/", config.Sessions.RequireSession(databaseHandler))
		mux.Handle("POST /databases/", config.Sessions.RequireSession(databaseHandler))
	}

	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("web/static"))))
	return auth.CSRFProtection(config.CSRFKey, config.SecureCookies, mux)
}
