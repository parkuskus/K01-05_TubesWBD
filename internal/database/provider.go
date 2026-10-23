package database

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"if3110.local/db-platform/internal/contracts"
)

type cachedPool struct {
	db       *sql.DB
	revision int64
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
