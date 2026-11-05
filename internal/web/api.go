package web

import (
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/gorilla/csrf"
	"if3110.local/db-platform/internal/api"
	"if3110.local/db-platform/internal/auth"
)

func protectRoutes(config Config, next http.Handler) (http.Handler, error) {
	failure := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			api.WriteError(w, http.StatusForbidden, "CSRF_INVALID", "Sesi formulir tidak valid. Muat ulang halaman.")
			return
		}
		http.Error(w, "Forbidden - CSRF token invalid", http.StatusForbidden)
	})
	hosts := make([]string, 0, len(config.FrontendOrigins))
	for _, origin := range config.FrontendOrigins {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Host == "" {
			return nil, ErrInvalidConfig
		}
		hosts = append(hosts, parsed.Host)
	}
	protected, err := auth.CSRFProtection(config.CSRFKey, config.SecureCookies, next,
		csrf.ErrorHandler(failure), csrf.TrustedOrigins(hosts))
	if err != nil {
		return nil, err
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isAPI := strings.HasPrefix(r.URL.Path, "/api/")
		if isAPI {
			w.Header().Set("Cache-Control", "no-store")
			if config.API == nil {
				api.WriteError(w, http.StatusNotFound, "NOT_FOUND", "Endpoint tidak ditemukan.")
				return
			}
			if r.URL.Path == "/api/v1/auth/login" {
				r.Body = http.MaxBytesReader(w, r.Body, api.MaxAuthBodyBytes)
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if origin := r.Header.Get("Origin"); origin != "" {
				scheme := "http"
				if config.SecureCookies || r.TLS != nil {
					scheme = "https"
				}
				if origin != scheme+"://"+r.Host && !slices.Contains(config.FrontendOrigins, origin) {
					failure.ServeHTTP(w, r)
					return
				}
			}
		}
		protected.ServeHTTP(w, r)
	}), nil
}
