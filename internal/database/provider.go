package database

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"if3110.local/db-platform/internal/contracts"
)

const defaultExecutorStatementTimeout = 10 * time.Minute

type cachedPool struct {
	db       *sql.DB
	revision int64
}

type executorSettings struct {
	timeout time.Duration
	openDB  func(Connection, string, time.Duration) (*sql.DB, error)
}

var managerExecutorSettings sync.Map // map[*Manager]executorSettings

// SetExecutorTimeout configures the default statement_timeout applied to pinned
// worker connections acquired through AcquireExecutorConn on this Manager.
func (m *Manager) SetExecutorTimeout(timeout time.Duration) {
	if m == nil || timeout <= 0 {
		return
	}
	cfg := m.getExecutorSettings()
	cfg.timeout = timeout
	managerExecutorSettings.Store(m, cfg)
}

func (m *Manager) setExecutorOpener(openDB func(Connection, string, time.Duration) (*sql.DB, error)) {
	if m == nil {
		return
	}
	cfg := m.getExecutorSettings()
	cfg.openDB = openDB
	managerExecutorSettings.Store(m, cfg)
}

func (m *Manager) getExecutorSettings() executorSettings {
	cfg := executorSettings{
		timeout: defaultExecutorStatementTimeout,
		openDB:  openTargetExecutorPool,
	}
	if m == nil {
		return cfg
	}
	if raw, ok := managerExecutorSettings.Load(m); ok {
		stored := raw.(executorSettings)
		if stored.timeout > 0 {
			cfg.timeout = stored.timeout
		}
		if stored.openDB != nil {
			cfg.openDB = stored.openDB
		}
	}
	return cfg
}

// Pool resolves current metadata on every call, so other managers' edits/deletes
// are detected too. Callers must authorize access first and must not close the pool.
func (m *Manager) Pool(ctx context.Context, id contracts.DatabaseID) (*sql.DB, error) {
	if !validID(id) {
		return nil, ErrInvalidInput
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r, err := m.store.get(ctx, id)
	if err != nil {
		err = storageError(err)
		if errors.Is(err, ErrNotFound) {
			m.invalidate(id)
		}
		return nil, err
	}
	if cached, ok := m.pools[id]; ok {
		if cached.revision == r.revision {
			if err := cached.db.PingContext(ctx); err != nil {
				m.invalidate(id)
				return nil, connectionError(ctx)
			}
			return cached.db, nil
		}
		m.invalidate(id)
	}
	password, err := m.credentials.open(r.ciphertext)
	if err != nil {
		return nil, err
	}
	db, err := m.openPool(r.Connection, password)
	if err != nil {
		return nil, connectionError(ctx)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, connectionError(ctx)
	}
	m.pools[id] = cachedPool{db: db, revision: r.revision}
	return db, nil
}

// AcquireExecutorConn opens an isolated, single-connection *sql.Conn pinned for
// worker migration execution along with the connection revision active at the
// moment of acquisition.
//
// Unlike Pool, this connection is not shared with the interactive viewer pool
// and uses a dedicated worker statement_timeout (default 10m) instead of the
// viewer's 30s limit. The caller owns the returned *sql.Conn and must close it.
func (m *Manager) AcquireExecutorConn(ctx context.Context, id contracts.DatabaseID) (*sql.Conn, int64, error) {
	cfg := m.getExecutorSettings()
	return m.AcquireExecutorConnWithTimeout(ctx, id, cfg.timeout)
}

// AcquireExecutorConnWithTimeout opens an isolated, single-connection *sql.Conn
// configured with the given statementTimeout and returns the connection together
// with the current connection_revision.
func (m *Manager) AcquireExecutorConnWithTimeout(
	ctx context.Context,
	id contracts.DatabaseID,
	statementTimeout time.Duration,
) (*sql.Conn, int64, error) {
	if !validID(id) {
		return nil, 0, ErrInvalidInput
	}
	cfg := m.getExecutorSettings()
	if statementTimeout <= 0 {
		statementTimeout = cfg.timeout
	}

	opCtx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, 0, ErrClosed
	}
	if err := opCtx.Err(); err != nil {
		return nil, 0, err
	}

	r, err := m.store.get(opCtx, id)
	if err != nil {
		err = storageError(err)
		if errors.Is(err, ErrNotFound) {
			m.invalidate(id)
		}
		return nil, 0, err
	}

	password, err := m.credentials.open(r.ciphertext)
	if err != nil {
		return nil, 0, err
	}

	db, err := cfg.openDB(r.Connection, password, statementTimeout)
	if err != nil {
		return nil, 0, connectionError(opCtx)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(0)

	conn, err := db.Conn(opCtx)
	if err != nil {
		_ = db.Close()
		return nil, 0, connectionError(opCtx)
	}
	if err := conn.PingContext(opCtx); err != nil {
		_ = conn.Close()
		_ = db.Close()
		return nil, 0, connectionError(opCtx)
	}
	return conn, r.revision, nil
}

func connectionError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrConnection
}

// invalidate must be called with mu held. Closing a pool prevents new operations;
// database/sql permits queries already in progress to finish.
func (m *Manager) invalidate(id contracts.DatabaseID) {
	if cached, ok := m.pools[id]; ok {
		delete(m.pools, id)
		_ = cached.db.Close()
	}
}

func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	managerExecutorSettings.Delete(m)
	var result error
	for id, cached := range m.pools {
		if err := cached.db.Close(); err != nil {
			result = ErrUnavailable
		}
		delete(m.pools, id)
	}
	return result
}

func targetConfig(c Connection, password string) (*pgx.ConnConfig, error) {
	// Use URL encoding for metadata and set the actual password separately, so
	// special characters work and pgx's retained ConnString contains no secret.
	u := url.URL{Scheme: "postgres", Host: net.JoinHostPort(c.Host, strconv.Itoa(c.Port)),
		Path: "/" + c.Database, User: url.UserPassword(c.Username, "unused")}
	query := url.Values{"sslmode": {c.SSLMode}, "connect_timeout": {"5"}}
	u.RawQuery = query.Encode()
	cfg, err := pgx.ParseConfig(u.String())
	if err != nil {
		return nil, ErrConnection
	}
	cfg.Password = password
	// Do not inherit PGOPTIONS/search_path from the platform process.
	cfg.RuntimeParams = map[string]string{"statement_timeout": "30000", "application_name": "if3110-db-platform"}
	return cfg, nil
}

func executorTargetConfig(c Connection, password string, statementTimeout time.Duration) (*pgx.ConnConfig, error) {
	cfg, err := targetConfig(c, password)
	if err != nil {
		return nil, err
	}
	if statementTimeout <= 0 {
		statementTimeout = defaultExecutorStatementTimeout
	}
	timeoutMs := statementTimeout.Milliseconds()
	if timeoutMs <= 0 {
		timeoutMs = 1
	}
	cfg.RuntimeParams["statement_timeout"] = strconv.FormatInt(timeoutMs, 10)
	cfg.RuntimeParams["application_name"] = "if3110-db-platform-worker"
	return cfg, nil
}

func openTargetPool(c Connection, password string) (*sql.DB, error) {
	cfg, err := targetConfig(c, password)
	if err != nil {
		return nil, err
	}
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(5 * time.Minute)
	db.SetConnMaxLifetime(30 * time.Minute)
	return db, nil
}

func openTargetExecutorPool(c Connection, password string, statementTimeout time.Duration) (*sql.DB, error) {
	cfg, err := executorTargetConfig(c, password, statementTimeout)
	if err != nil {
		return nil, err
	}
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(0)
	return db, nil
}
