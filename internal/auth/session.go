package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"io"
	"math"
	"net/http"
	"time"

	"if3110.local/db-platform/internal/contracts"
)

const (
	DefaultIdleTimeout      = 30 * time.Minute
	DefaultAbsoluteLifetime = 12 * time.Hour
	DefaultRotationInterval = 15 * time.Minute
	DefaultGracePeriod      = 30 * time.Second
	DefaultSessionCookie    = "session_id"
	sessionTokenBytes       = 32
)

var (
	ErrUnauthenticated      = errors.New("unauthenticated")
	ErrInvalidSessionConfig = errors.New("invalid session configuration")
)

type SessionConfig struct {
	IdleTimeout      time.Duration
	AbsoluteLifetime time.Duration
	RotationInterval time.Duration
	GracePeriod      time.Duration
	CookieName       string
	SecureCookie     bool
}

type sessionRecord struct {
	ID                int64
	UserID            contracts.UserID
	CreatedAt         time.Time
	LastSeenAt        time.Time
	AbsoluteExpiresAt time.Time
	RotatedAt         time.Time
	TokenIsAlias      bool
}

type sessionStore interface {
	Create(ctx context.Context, proof LoginProof, tokenHash, previousTokenHash []byte, now, expiresAt time.Time, idleTimeout time.Duration) (sessionRecord, error)
	LoadAndTouch(ctx context.Context, tokenHash []byte, now time.Time, idleTimeout time.Duration) (sessionRecord, error)
	Rotate(ctx context.Context, sessionID int64, presentedHash, newHash []byte, now time.Time, idleTimeout, grace time.Duration) (sessionRecord, error)
	RevokeToken(ctx context.Context, tokenHash []byte, now time.Time) error
	RevokeUser(ctx context.Context, userID contracts.UserID, now time.Time) error
}

type SessionManager struct {
	store  sessionStore
	config SessionConfig
	now    func() time.Time
	random io.Reader
}

type userContextKey struct{}

func NewSessionManager(db *sql.DB, config SessionConfig) (*SessionManager, error) {
	if db == nil {
		return nil, ErrInvalidSessionConfig
	}
	return newSessionManager(&postgresSessionStore{db: db}, config)
}

func newSessionManager(store sessionStore, config SessionConfig) (*SessionManager, error) {
	if store == nil {
		return nil, ErrInvalidSessionConfig
	}
	config, err := normalizeSessionConfig(config)
	if err != nil {
		return nil, err
	}
	return &SessionManager{store: store, config: config, now: time.Now, random: rand.Reader}, nil
}

func normalizeSessionConfig(config SessionConfig) (SessionConfig, error) {
	if config.IdleTimeout == 0 {
		config.IdleTimeout = DefaultIdleTimeout
	}
	if config.AbsoluteLifetime == 0 {
		config.AbsoluteLifetime = DefaultAbsoluteLifetime
	}
	if config.RotationInterval == 0 {
		config.RotationInterval = DefaultRotationInterval
	}
	if config.GracePeriod == 0 {
		config.GracePeriod = DefaultGracePeriod
	}
	if config.CookieName == "" {
		config.CookieName = DefaultSessionCookie
	}
	if config.IdleTimeout < time.Second || config.AbsoluteLifetime < config.IdleTimeout ||
		config.RotationInterval < time.Second || config.RotationInterval >= config.AbsoluteLifetime ||
		config.GracePeriod < 0 || config.GracePeriod >= config.RotationInterval ||
		(&http.Cookie{Name: config.CookieName}).Valid() != nil {
		return SessionConfig{}, ErrInvalidSessionConfig
	}
	return config, nil
}

// Create starts a fresh session only if the login proof still matches the account.
func (m *SessionManager) Create(w http.ResponseWriter, r *http.Request, proof LoginProof) error {
	if proof.UserID == "" || proof.passwordHash == "" {
		return ErrUnauthenticated
	}
	now := m.now()
	token, tokenHash, err := m.newToken()
	if err != nil {
		return err
	}
	var previousHash []byte
	if cookie, err := r.Cookie(m.config.CookieName); err == nil {
		if hash, err := hashSessionToken(cookie.Value); err == nil {
			previousHash = hash[:]
		}
	}
	record, err := m.store.Create(r.Context(), proof, tokenHash[:], previousHash, now, now.Add(m.config.AbsoluteLifetime), m.config.IdleTimeout)
	if err != nil {
		return err
	}
	m.setCookie(w, token, now, record.AbsoluteExpiresAt)
	return nil
}

// RequireSession protects a handler and exposes the authenticated user through CurrentUser.
func (m *SessionManager) RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(m.config.CookieName)
		if err != nil {
			m.clearCookie(w)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		presentedHash, err := hashSessionToken(cookie.Value)
		if err != nil {
			m.clearCookie(w)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		now := m.now()
		record, err := m.store.LoadAndTouch(r.Context(), presentedHash[:], now, m.config.IdleTimeout)
		if errors.Is(err, ErrUnauthenticated) {
			m.clearCookie(w)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if err != nil {
			http.Error(w, "session service unavailable", http.StatusServiceUnavailable)
			return
		}

		if record.TokenIsAlias || now.Sub(record.RotatedAt) >= m.config.RotationInterval {
			token, newHash, err := m.newToken()
			if err != nil {
				http.Error(w, "session service unavailable", http.StatusServiceUnavailable)
				return
			}
			record, err = m.store.Rotate(r.Context(), record.ID, presentedHash[:], newHash[:], now, m.config.IdleTimeout, m.config.GracePeriod)
			if errors.Is(err, ErrUnauthenticated) {
				m.clearCookie(w)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if err != nil {
				http.Error(w, "session service unavailable", http.StatusServiceUnavailable)
				return
			}
			m.setCookie(w, token, now, record.AbsoluteExpiresAt)
		} else {
			m.setCookie(w, cookie.Value, now, record.AbsoluteExpiresAt)
		}
		r = r.WithContext(WithUser(r.Context(), record.UserID))
		next.ServeHTTP(w, r)
	})
}

// Rotate replaces the current identifier immediately, for example after a privilege increase.
func (m *SessionManager) Rotate(w http.ResponseWriter, r *http.Request) error {
	cookie, err := r.Cookie(m.config.CookieName)
	if err != nil {
		return ErrUnauthenticated
	}
	presentedHash, err := hashSessionToken(cookie.Value)
	if err != nil {
		return ErrUnauthenticated
	}
	now := m.now()
	record, err := m.store.LoadAndTouch(r.Context(), presentedHash[:], now, m.config.IdleTimeout)
	if err != nil {
		return err
	}
	token, newHash, err := m.newToken()
	if err != nil {
		return err
	}
	record, err = m.store.Rotate(r.Context(), record.ID, presentedHash[:], newHash[:], now, m.config.IdleTimeout, m.config.GracePeriod)
	if err != nil {
		return err
	}
	m.setCookie(w, token, now, record.AbsoluteExpiresAt)
	return nil
}

// Logout revokes the current backend session and expires its cookie.
func (m *SessionManager) Logout(w http.ResponseWriter, r *http.Request) error {
	defer m.clearCookie(w)
	cookie, err := r.Cookie(m.config.CookieName)
	if err != nil {
		return nil
	}
	tokenHash, err := hashSessionToken(cookie.Value)
	if err != nil {
		return nil
	}
	return m.store.RevokeToken(r.Context(), tokenHash[:], m.now())
}

// RevokeUser invalidates all active sessions for a user.
func (m *SessionManager) RevokeUser(ctx context.Context, userID contracts.UserID) error {
	if userID == "" {
		return ErrUnauthenticated
	}
	return m.store.RevokeUser(ctx, userID, m.now())
}

func WithUser(ctx context.Context, userID contracts.UserID) context.Context {
	return context.WithValue(ctx, userContextKey{}, userID)
}

func CurrentUser(r *http.Request) (contracts.UserID, bool) {
	userID, ok := r.Context().Value(userContextKey{}).(contracts.UserID)
	return userID, ok && userID != ""
}

func (m *SessionManager) newToken() (string, [sha256.Size]byte, error) {
	var raw [sessionTokenBytes]byte
	if _, err := io.ReadFull(m.random, raw[:]); err != nil {
		return "", [sha256.Size]byte{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	return token, sha256.Sum256([]byte(token)), nil
}

func hashSessionToken(token string) ([sha256.Size]byte, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != sessionTokenBytes {
		return [sha256.Size]byte{}, ErrUnauthenticated
	}
	return sha256.Sum256([]byte(token)), nil
}

func (m *SessionManager) setCookie(w http.ResponseWriter, token string, now, absoluteExpiresAt time.Time) {
	expires := now.Add(m.config.IdleTimeout)
	if absoluteExpiresAt.Before(expires) {
		expires = absoluteExpiresAt
	}
	maxAge := int(math.Ceil(expires.Sub(now).Seconds()))
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{
		Name:     m.config.CookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   m.config.SecureCookie,
		SameSite: http.SameSiteLaxMode,
	})
}

func (m *SessionManager) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     m.config.CookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
		HttpOnly: true,
		Secure:   m.config.SecureCookie,
		SameSite: http.SameSiteLaxMode,
	})
}
