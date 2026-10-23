package database

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"if3110.local/db-platform/internal/contracts"
)

const operationTimeout = 5 * time.Second

// Manager owns registered connections and target pools. CRUD is admin-only;
// Pool is an internal provider whose callers must first authorize database access.
type Manager struct {
	store       connectionStore
	authorizer  contracts.Authorizer
	credentials *credentialCipher
	mu          sync.Mutex // Serializes pool creation with edits/deletion and shutdown.
	pools       map[contracts.DatabaseID]cachedPool
	closed      bool
	openPool    func(Connection, string) (*sql.DB, error)
}

var _ contracts.DatabaseProvider = (*Manager)(nil)

// NewManager does not read environment variables or open target connections.
// Pass a stable, dedicated 32-byte key, decoded by the application configuration.
// Close releases target pools only; the caller still owns platformDB.
func NewManager(platformDB *sql.DB, authorizer contracts.Authorizer, key []byte) (*Manager, error) {
	if platformDB == nil || authorizer == nil {
		return nil, ErrInvalidConfig
	}
	credentials, err := newCredentialCipher(key)
	if err != nil {
		return nil, err
	}
	return &Manager{store: &postgresStore{db: platformDB}, authorizer: authorizer,
		credentials: credentials, pools: make(map[contracts.DatabaseID]cachedPool), openPool: openTargetPool}, nil
}

func (m *Manager) requireAdmin(ctx context.Context, actor contracts.UserID) error {
	if actor == "" {
		return contracts.ErrForbidden
	}
	if err := m.authorizer.RequirePlatformAdmin(ctx, actor); err != nil {
		if errors.Is(err, contracts.ErrForbidden) {
			return contracts.ErrForbidden
		}
		return storageError(err)
	}
	return nil
}

func (m *Manager) Create(ctx context.Context, actor contracts.UserID, input ConnectionInput) (Connection, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if err := m.requireAdmin(ctx, actor); err != nil {
		return Connection{}, err
	}
	in, err := input.validate(true)
	if err != nil {
		return Connection{}, err
	}
	ciphertext, err := m.credentials.seal(*in.Password)
	if err != nil {
		return Connection{}, err
	}
	in.Password = nil // The repository only receives encrypted credentials.
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Connection{}, ErrClosed
	}
	c, err := m.store.create(ctx, in, ciphertext)
	if err != nil {
		return Connection{}, storageError(err)
	}
	return c, nil
}

func (m *Manager) Get(ctx context.Context, actor contracts.UserID, id contracts.DatabaseID) (Connection, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if err := m.requireAdmin(ctx, actor); err != nil {
		return Connection{}, err
	}
	if !validID(id) {
		return Connection{}, ErrInvalidInput
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Connection{}, ErrClosed
	}
	r, err := m.store.get(ctx, id)
	if err != nil {
		return Connection{}, storageError(err)
	}
	return r.Connection, nil
}

// List is for connection administration, not the user's accessible-database list.
func (m *Manager) List(ctx context.Context, actor contracts.UserID) ([]Connection, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if err := m.requireAdmin(ctx, actor); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	items, err := m.store.list(ctx)
	if err != nil {
		return nil, storageError(err)
	}
	return items, nil
}

func (m *Manager) Update(ctx context.Context, actor contracts.UserID, id contracts.DatabaseID, input ConnectionInput) (Connection, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if err := m.requireAdmin(ctx, actor); err != nil {
		return Connection{}, err
	}
	if !validID(id) {
		return Connection{}, ErrInvalidInput
	}
	in, err := input.validate(false)
	if err != nil {
		return Connection{}, err
	}
	var ciphertext []byte
	if in.Password != nil {
		ciphertext, err = m.credentials.seal(*in.Password)
		if err != nil {
			return Connection{}, err
		}
	}
	in.Password = nil
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Connection{}, ErrClosed
	}
	c, err := m.store.update(ctx, id, in, ciphertext)
	if err != nil {
		return Connection{}, storageError(err)
	}
	m.invalidate(id)
	return c, nil
}

func (m *Manager) Delete(ctx context.Context, actor contracts.UserID, id contracts.DatabaseID) error {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if err := m.requireAdmin(ctx, actor); err != nil {
		return err
	}
	if !validID(id) {
		return ErrInvalidInput
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	if err := m.store.delete(ctx, id); err != nil {
		return storageError(err)
	}
	m.invalidate(id)
	return nil
}
