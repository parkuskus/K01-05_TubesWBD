package auth

import (
	"errors"
	"net/http"

	"github.com/gorilla/csrf"
)

var ErrInvalidCSRFKey = errors.New("CSRF authentication key must be 32 bytes")

// CSRFProtection wraps a handler with synchronizer-token protection, including unauthenticated forms such as login.
func CSRFProtection(key []byte, secure bool, next http.Handler, options ...csrf.Option) (http.Handler, error) {
	if len(key) != 32 {
		return nil, ErrInvalidCSRFKey
	}
	defaults := []csrf.Option{
		csrf.Secure(secure),
		csrf.SameSite(csrf.SameSiteLaxMode),
		csrf.Path("/"),
	}
	protect := csrf.Protect(key, append(defaults, options...)...)
	protected := protect(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !secure && r.TLS == nil {
			r = csrf.PlaintextHTTPRequest(r)
		}
		protected.ServeHTTP(w, r)
	}), nil
}

func CSRFToken(r *http.Request) string {
	return csrf.Token(r)
}
