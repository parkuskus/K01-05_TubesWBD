package auth

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"if3110.local/db-platform/internal/contracts"
)

type fakeSessionStore struct {
	nextID   int64
	sessions map[int64]*fakeSession
	tokens   map[[sha256.Size]byte]fakeToken
}

type fakeSession struct {
	record  sessionRecord
	revoked bool
}

type fakeToken struct {
	sessionID  int64
	validUntil time.Time
}

func newFakeSessionStore() *fakeSessionStore {
	return &fakeSessionStore{sessions: make(map[int64]*fakeSession), tokens: make(map[[sha256.Size]byte]fakeToken)}
}

func (s *fakeSessionStore) Create(_ context.Context, userID contracts.UserID, tokenHash, previousTokenHash []byte, now, expiresAt time.Time, idleTimeout time.Duration) (sessionRecord, error) {
	if len(previousTokenHash) == sha256.Size {
		var previous [sha256.Size]byte
		copy(previous[:], previousTokenHash)
		if token, ok := s.tokens[previous]; ok {
			s.sessions[token.sessionID].revoked = true
		}
	}
	s.sweep(now, idleTimeout)
	s.nextID++
	record := sessionRecord{ID: s.nextID, UserID: userID, CreatedAt: now, LastSeenAt: now, AbsoluteExpiresAt: expiresAt, RotatedAt: now}
	s.sessions[record.ID] = &fakeSession{record: record}
	var hash [sha256.Size]byte
	copy(hash[:], tokenHash)
	s.tokens[hash] = fakeToken{sessionID: record.ID}
	return record, nil
}

func (s *fakeSessionStore) LoadAndTouch(_ context.Context, tokenHash []byte, now time.Time, idleTimeout time.Duration) (sessionRecord, error) {
	var hash [sha256.Size]byte
	copy(hash[:], tokenHash)
	token, ok := s.tokens[hash]
	if !ok || (!token.validUntil.IsZero() && !now.Before(token.validUntil)) {
		return sessionRecord{}, ErrUnauthenticated
	}
	session := s.sessions[token.sessionID]
	if session == nil || session.revoked || !now.Before(session.record.AbsoluteExpiresAt) || now.Sub(session.record.LastSeenAt) >= idleTimeout {
		return sessionRecord{}, ErrUnauthenticated
	}
	session.record.LastSeenAt = now
	result := session.record
	result.TokenIsAlias = !token.validUntil.IsZero()
	return result, nil
}

func (s *fakeSessionStore) Rotate(_ context.Context, sessionID int64, presentedHash, newHash []byte, now time.Time, idleTimeout, grace time.Duration) (sessionRecord, error) {
	var presented, replacement [sha256.Size]byte
	copy(presented[:], presentedHash)
	copy(replacement[:], newHash)
	token, ok := s.tokens[presented]
	session := s.sessions[sessionID]
	if !ok || token.sessionID != sessionID || session == nil || session.revoked || (!token.validUntil.IsZero() && !now.Before(token.validUntil)) || !now.Before(session.record.AbsoluteExpiresAt) || now.Sub(session.record.LastSeenAt) >= idleTimeout {
		return sessionRecord{}, ErrUnauthenticated
	}
	for hash, current := range s.tokens {
		if current.sessionID == sessionID && current.validUntil.IsZero() {
			current.validUntil = now.Add(grace)
			s.tokens[hash] = current
		}
	}
	s.tokens[replacement] = fakeToken{sessionID: sessionID}
	session.record.LastSeenAt = now
	session.record.RotatedAt = now
	return session.record, nil
}

func (s *fakeSessionStore) RevokeToken(_ context.Context, tokenHash []byte, _ time.Time) error {
	var hash [sha256.Size]byte
	copy(hash[:], tokenHash)
	if token, ok := s.tokens[hash]; ok {
		s.sessions[token.sessionID].revoked = true
	}
	return nil
}

func (s *fakeSessionStore) RevokeUser(_ context.Context, userID contracts.UserID, _ time.Time) error {
	for _, session := range s.sessions {
		if session.record.UserID == userID {
			session.revoked = true
		}
	}
	return nil
}

func (s *fakeSessionStore) sweep(now time.Time, idleTimeout time.Duration) {
	for id, session := range s.sessions {
		if session.revoked || !now.Before(session.record.AbsoluteExpiresAt) || now.Sub(session.record.LastSeenAt) >= idleTimeout {
			delete(s.sessions, id)
			for token, entry := range s.tokens {
				if entry.sessionID == id {
					delete(s.tokens, token)
				}
			}
		}
	}
}

func TestCreateSessionSetsOpaqueSecureCookie(t *testing.T) {
	store := newFakeSessionStore()
	manager := newTestSessionManager(t, store)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/login", nil)

	if err := manager.Create(response, request, contracts.UserID("user-1")); err != nil {
		t.Fatal(err)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies, want 1", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != "session_id" || len(cookie.Value) != 43 {
		t.Fatalf("cookie = (%q, %d chars), want session_id with an opaque 256-bit token", cookie.Name, len(cookie.Value))
	}
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" {
		t.Fatalf("unsafe session cookie attributes: %+v", cookie)
	}
	if response.Body.Len() != 0 {
		t.Fatalf("session token leaked into response body: %q", response.Body.String())
	}
	if len(store.tokens) != 1 {
		t.Fatalf("stored token count = %d, want 1", len(store.tokens))
	}
	for storedHash := range store.tokens {
		if storedHash != sha256.Sum256([]byte(cookie.Value)) {
			t.Fatal("stored session identifier hash does not match the cookie token")
		}
	}
}

func TestRequireSessionRotatesAndHonorsGraceAndAbsoluteExpiration(t *testing.T) {
	store := newFakeSessionStore()
	manager := newTestSessionManager(t, store)
	loginResponse := httptest.NewRecorder()
	if err := manager.Create(loginResponse, httptest.NewRequest(http.MethodPost, "/login", nil), "user-1"); err != nil {
		t.Fatal(err)
	}
	oldCookie := loginResponse.Result().Cookies()[0]
	originalExpiry := store.sessions[1].record.AbsoluteExpiresAt
	manager.now = func() time.Time { return time.Now().Add(DefaultRotationInterval + time.Second) }

	request := httptest.NewRequest(http.MethodGet, "/private", nil)
	request.AddCookie(oldCookie)
	response := httptest.NewRecorder()
	manager.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, ok := CurrentUser(r)
		if !ok || userID != "user-1" {
			t.Errorf("CurrentUser() = (%q, %t), want (user-1, true)", userID, ok)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.Code)
	}
	newCookie := response.Result().Cookies()[0]
	if newCookie.Value == oldCookie.Value {
		t.Fatal("session identifier was not rotated")
	}
	if got := store.sessions[1].record.AbsoluteExpiresAt; !got.Equal(originalExpiry) {
		t.Fatalf("absolute expiration changed during rotation: got %v, want %v", got, originalExpiry)
	}

	graceRequest := httptest.NewRequest(http.MethodGet, "/private", nil)
	graceRequest.AddCookie(oldCookie)
	graceResponse := httptest.NewRecorder()
	manager.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })).ServeHTTP(graceResponse, graceRequest)
	if graceResponse.Code != http.StatusNoContent {
		t.Fatalf("old identifier during grace period returned %d, want 204", graceResponse.Code)
	}
	if graceResponse.Result().Cookies()[0].Value == oldCookie.Value {
		t.Fatal("grace-period request did not receive a refreshed cookie")
	}

	manager.now = func() time.Time { return originalExpiry }
	expiredRequest := httptest.NewRequest(http.MethodGet, "/private", nil)
	expiredRequest.AddCookie(newCookie)
	expiredResponse := httptest.NewRecorder()
	manager.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { t.Error("expired session reached handler") })).ServeHTTP(expiredResponse, expiredRequest)
	if expiredResponse.Code != http.StatusUnauthorized {
		t.Fatalf("expired session status = %d, want 401", expiredResponse.Code)
	}
}

func TestIdleExpirationAndLogoutRevokeSession(t *testing.T) {
	store := newFakeSessionStore()
	manager := newTestSessionManager(t, store)
	loginResponse := httptest.NewRecorder()
	if err := manager.Create(loginResponse, httptest.NewRequest(http.MethodPost, "/login", nil), "user-1"); err != nil {
		t.Fatal(err)
	}
	cookie := loginResponse.Result().Cookies()[0]
	manager.now = func() time.Time { return time.Now().Add(DefaultIdleTimeout + time.Second) }
	request := httptest.NewRequest(http.MethodGet, "/private", nil)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	manager.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { t.Error("idle-expired session reached handler") })).ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("idle-expired session status = %d, want 401", response.Code)
	}

	manager.now = time.Now
	logoutRequest := httptest.NewRequest(http.MethodPost, "/logout", nil)
	logoutRequest.AddCookie(cookie)
	logoutResponse := httptest.NewRecorder()
	if err := manager.Logout(logoutResponse, logoutRequest); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}
	if !store.sessions[1].revoked {
		t.Fatal("Logout() did not revoke the backend session")
	}
	if cleared := logoutResponse.Result().Cookies()[0]; cleared.MaxAge >= 0 {
		t.Fatalf("logout cookie MaxAge = %d, want a negative value", cleared.MaxAge)
	}
}

func TestSessionConfigRejectsInvalidLifetimes(t *testing.T) {
	if _, err := normalizeSessionConfig(SessionConfig{IdleTimeout: -time.Second}); err == nil {
		t.Fatal("negative idle timeout was accepted")
	}
	if _, err := normalizeSessionConfig(SessionConfig{IdleTimeout: time.Hour, AbsoluteLifetime: time.Minute}); err == nil {
		t.Fatal("absolute lifetime shorter than idle timeout was accepted")
	}
}

func newTestSessionManager(t *testing.T, store sessionStore) *SessionManager {
	t.Helper()
	manager, err := newSessionManager(store, SessionConfig{SecureCookie: true})
	if err != nil {
		t.Fatal(err)
	}
	manager.now = time.Now
	return manager
}
