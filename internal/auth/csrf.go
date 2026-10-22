package auth

import (
	"errors"
	"net/http"

	"github.com/gorilla/csrf"
)

var ErrInvalidCSRFKey = errors.New("CSRF authentication key must be 32 bytes")

// CSRFProtection wraps a handler with synchronizer-token protection, including unauthenticated forms such as login.
func CSRFProtection(key []byte, secure bool, next http.Handler) (http.Handler, error) {
	if len(key) != 32 {
		return nil, ErrInvalidCSRFKey
	}
	protect := csrf.Protect(
		key,
		csrf.Secure(secure),
		csrf.SameSite(csrf.SameSiteLaxMode),
		csrf.Path("/"),
	)
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
