package database

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"if3110.local/db-platform/internal/contracts"
)

type databaseID = contracts.DatabaseID

func stringPointer(s string) *string { return &s }
func validInput() ConnectionInput {
	return ConnectionInput{Name: "Target", Host: "localhost", Port: 5432, Database: "target", Username: "target_user", SSLMode: "disable", Password: stringPointer("secret'@:#/$\\password")}
}

type fakeAuthorizer struct{ err error }

func (a fakeAuthorizer) RequirePlatformAdmin(_ context.Context, id contracts.UserID) error {
	if a.err != nil {
		return a.err
	}
	if id != "admin" {
		return contracts.ErrForbidden
	}
	return nil
}
func (fakeAuthorizer) RequireDatabaseAccess(context.Context, contracts.UserID, contracts.DatabaseID, contracts.DatabaseAction) error {
	return contracts.ErrForbidden // Admin does not imply a database grant.
}

type memoryStore struct {
	rows  map[databaseID]connectionRecord
	next  int
	calls int
	err   error
}

func (s *memoryStore) check(ctx context.Context) error {
	s.calls++
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.err
}
func (s *memoryStore) create(ctx context.Context, in ConnectionInput, ciphertext []byte) (Connection, error) {
	if err := s.check(ctx); err != nil {
		return Connection{}, err
	}
	s.next++
	id := databaseID(strconv.Itoa(s.next))
	c := metadata(in, id)
	s.rows[id] = connectionRecord{Connection: c, ciphertext: ciphertext, revision: 1}
	return c, nil
}
func metadata(in ConnectionInput, id databaseID) Connection {
	return Connection{ID: id, Name: in.Name, Host: in.Host, Port: in.Port, Database: in.Database, Username: in.Username, SSLMode: in.SSLMode}
}
func (s *memoryStore) get(ctx context.Context, id databaseID) (connectionRecord, error) {
	if err := s.check(ctx); err != nil {
		return connectionRecord{}, err
	}
	r, ok := s.rows[id]
	if !ok {
		return connectionRecord{}, sql.ErrNoRows
	}
	return r, nil
}
func (s *memoryStore) list(ctx context.Context) ([]Connection, error) {
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	var result []Connection
	for _, r := range s.rows {
		result = append(result, r.Connection)
	}
	return result, nil
}
func (s *memoryStore) update(ctx context.Context, id databaseID, in ConnectionInput, ciphertext []byte) (Connection, error) {
	r, err := s.get(ctx, id)
	if err != nil {
		return Connection{}, err
	}
	r.Connection = metadata(in, id)
	if ciphertext != nil {
		r.ciphertext = ciphertext
	}
	r.revision++
	s.rows[id] = r
	return r.Connection, nil
}
func (s *memoryStore) delete(ctx context.Context, id databaseID) error {
	if _, err := s.get(ctx, id); err != nil {
		return err
	}
	delete(s.rows, id)
	return nil
}

func testManager(t *testing.T) (*Manager, *memoryStore) {
	t.Helper()
	c, err := newCredentialCipher(bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	s := &memoryStore{rows: make(map[databaseID]connectionRecord)}
	m := &Manager{store: s, authorizer: fakeAuthorizer{}, credentials: c, pools: make(map[databaseID]cachedPool), openPool: openTargetPool}
	t.Cleanup(func() { _ = m.Close() })
	return m, s
}

func TestManagerCRUDEncryptsAndHidesPassword(t *testing.T) {
	m, store := testManager(t)
	ctx := context.Background()
	in := validInput()
	c, err := m.Create(ctx, "admin", in)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(store.rows[c.ID].ciphertext, []byte(*in.Password)) {
		t.Fatal("stored plaintext password")
	}
	detail, err := m.Get(ctx, "admin", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	list, err := m.List(ctx, "admin")
	if err != nil || len(list) != 1 {
		t.Fatal("list failed")
	}
	for _, value := range []any{c, detail, list} {
		b, _ := json.Marshal(value)
		if strings.Contains(string(b), *in.Password) || strings.Contains(string(b), "ciphertext") {
			t.Fatal("response leaked credentials")
		}
	}
	oldCiphertext := append([]byte(nil), store.rows[c.ID].ciphertext...)
	in.Name = "Renamed"
	in.Password = nil
	if _, err := m.Update(ctx, "admin", c.ID, in); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(oldCiphertext, store.rows[c.ID].ciphertext) {
		t.Fatal("nil update replaced password")
	}
	in.Password = stringPointer("replacement")
	if _, err := m.Update(ctx, "admin", c.ID, in); err != nil {
		t.Fatal(err)
	}
	got, err := m.credentials.open(store.rows[c.ID].ciphertext)
	if err != nil || got != "replacement" {
		t.Fatal("replacement password not encrypted")
	}
	if err := m.Delete(ctx, "admin", c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(ctx, "admin", c.ID); err != ErrNotFound {
		t.Fatalf("deleted get: %v", err)
	}
	if err := m.Delete(ctx, "admin", c.ID); err != ErrNotFound {
		t.Fatalf("missing delete: %v", err)
	}
}

func TestManagerRejectsEveryNonAdminOperationBeforeStorage(t *testing.T) {
	m, store := testManager(t)
	for _, actor := range []contracts.UserID{"", "viewer", "editor"} {
		for name, operation := range adminOperations(m, actor) {
			t.Run(string(actor)+"/"+name, func(t *testing.T) {
				if err := operation(); err != contracts.ErrForbidden {
					t.Fatalf("error = %v", err)
				}
			})
		}
	}
	if store.calls != 0 {
		t.Fatal("unauthorized request reached storage")
	}
}

func adminOperations(m *Manager, actor contracts.UserID) map[string]func() error {
	ctx := context.Background()
	return map[string]func() error{
		"create": func() error { _, err := m.Create(ctx, actor, validInput()); return err },
		"get":    func() error { _, err := m.Get(ctx, actor, "1"); return err },
		"list":   func() error { _, err := m.List(ctx, actor); return err },
		"update": func() error { _, err := m.Update(ctx, actor, "1", validInput()); return err },
		"delete": func() error { return m.Delete(ctx, actor, "1") },
	}
}

func TestManagerSanitizesStorageAndAuthorizationErrors(t *testing.T) {
	m, store := testManager(t)
	store.err = errors.New("driver failed: secret-password postgres://private")
	for name, operation := range adminOperations(m, "admin") {
		t.Run(name, func(t *testing.T) {
			if err := operation(); err != ErrUnavailable {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
	store.calls = 0
	m.authorizer = fakeAuthorizer{err: errors.New("private authorization details")}
	for _, operation := range adminOperations(m, "admin") {
		if err := operation(); err != ErrUnavailable {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if store.calls != 0 {
		t.Fatal("failed authorization reached storage")
	}
	for _, test := range []struct{ err, want error }{
		{&pgconn.PgError{Code: "23505", Detail: "secret"}, ErrConflict},
		{&pgconn.PgError{Code: "23503", Detail: "secret"}, ErrInUse},
		{sql.ErrNoRows, ErrNotFound}, {context.Canceled, context.Canceled},
		{context.DeadlineExceeded, context.DeadlineExceeded},
	} {
		if got := storageError(test.err); got != test.want {
			t.Fatalf("error = %v, want %v", got, test.want)
		}
	}
}

func TestManagerRejectsInvalidInputAndClosedState(t *testing.T) {
	m, store := testManager(t)
	in := validInput()
	in.Port = -1
	if _, err := m.Create(context.Background(), "admin", in); err != ErrInvalidInput {
		t.Fatal(err)
	}
	if _, err := m.Update(context.Background(), "admin", "1", in); err != ErrInvalidInput {
		t.Fatal(err)
	}
	if store.calls != 0 {
		t.Fatal("invalid input reached storage")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	for _, operation := range adminOperations(m, "admin") {
		if err := operation(); err != ErrClosed {
			t.Fatalf("closed error = %v", err)
		}
	}
	if _, err := NewManager(nil, fakeAuthorizer{}, make([]byte, 32)); err != ErrInvalidConfig {
		t.Fatal(err)
	}
}
