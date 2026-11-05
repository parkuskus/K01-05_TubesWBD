package execution

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type fakeLockServer struct {
	mu    sync.Mutex
	locks map[[2]int32]*fakeLockSession
}

type fakeLockConnector struct {
	server     *fakeLockServer
	dbName     string
	failUnlock bool
	unlockErr  error
	closed     atomic.Int32
	resetCount atomic.Int32
}

func (c *fakeLockConnector) Connect(context.Context) (driver.Conn, error) {
	return &fakeLockSession{
		connector: c,
		held:      make(map[[2]int32]bool),
	}, nil
}

func (*fakeLockConnector) Driver() driver.Driver { return fakeLockDriver{} }

type fakeLockDriver struct{}

func (fakeLockDriver) Open(string) (driver.Conn, error) { return nil, errors.New("unused") }

type fakeLockSession struct {
	connector *fakeLockConnector
	held      map[[2]int32]bool
	txCommits int
}

func (*fakeLockSession) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }

func (s *fakeLockSession) Begin() (driver.Tx, error) {
	return &fakeLockTx{session: s}, nil
}

type fakeLockTx struct {
	session *fakeLockSession
}

func (tx *fakeLockTx) Commit() error {
	// Notice: committing a transaction does NOT release session-level advisory locks!
	tx.session.txCommits++
	return nil
}

func (*fakeLockTx) Rollback() error { return nil }

func (s *fakeLockSession) ResetSession(context.Context) error {
	s.connector.resetCount.Add(1)
	return nil
}

func (s *fakeLockSession) Close() error {
	s.connector.closed.Add(1)
	s.connector.server.mu.Lock()
	defer s.connector.server.mu.Unlock()
	for k := range s.held {
		if s.connector.server.locks[k] == s {
			delete(s.connector.server.locks, k)
		}
		delete(s.held, k)
	}
	return nil
}

func (s *fakeLockSession) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	trimmed := strings.TrimSpace(query)
	switch {
	case strings.HasPrefix(trimmed, "SELECT current_database()"):
		return &lockSingleValueRows{col: "current_database", val: s.connector.dbName}, nil

	case strings.HasPrefix(trimmed, "SELECT pg_try_advisory_lock"):
		k1 := int32(args[0].Value.(int64))
		k2 := int32(args[1].Value.(int64))
		pair := [2]int32{k1, k2}

		s.connector.server.mu.Lock()
		defer s.connector.server.mu.Unlock()
		if s.connector.server.locks == nil {
			s.connector.server.locks = make(map[[2]int32]*fakeLockSession)
		}
		owner, taken := s.connector.server.locks[pair]
		if taken && owner != s {
			return &lockSingleValueRows{col: "pg_try_advisory_lock", val: false}, nil
		}
		s.connector.server.locks[pair] = s
		s.held[pair] = true
		return &lockSingleValueRows{col: "pg_try_advisory_lock", val: true}, nil

	case strings.HasPrefix(trimmed, "SELECT pg_advisory_unlock"):
		if s.connector.unlockErr != nil {
			return nil, s.connector.unlockErr
		}
		if s.connector.failUnlock {
			return &lockSingleValueRows{col: "pg_advisory_unlock", val: false}, nil
		}
		k1 := int32(args[0].Value.(int64))
		k2 := int32(args[1].Value.(int64))
		pair := [2]int32{k1, k2}

		s.connector.server.mu.Lock()
		defer s.connector.server.mu.Unlock()
		if s.connector.server.locks[pair] == s {
			delete(s.connector.server.locks, pair)
			delete(s.held, pair)
			return &lockSingleValueRows{col: "pg_advisory_unlock", val: true}, nil
		}
		return &lockSingleValueRows{col: "pg_advisory_unlock", val: false}, nil

	default:
		return nil, errors.New("unexpected query: " + trimmed)
	}
}

type lockSingleValueRows struct {
	col  string
	val  any
	done bool
}

func (r *lockSingleValueRows) Columns() []string { return []string{r.col} }
func (r *lockSingleValueRows) Close() error      { return nil }
func (r *lockSingleValueRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.val
	return nil
}

func TestSessionAdvisoryLockPersistsAcrossBatchCommitsAndBlocksAliases(t *testing.T) {
	server := &fakeLockServer{}
	connA1 := &fakeLockConnector{server: server, dbName: "target_prod"}
	connA2Alias := &fakeLockConnector{server: server, dbName: "target_prod"}
	connB := &fakeLockConnector{server: server, dbName: "target_staging"}

	dbA1 := sql.OpenDB(connA1)
	dbA2 := sql.OpenDB(connA2Alias)
	dbB := sql.OpenDB(connB)
	t.Cleanup(func() {
		_ = dbA1.Close()
		_ = dbA2.Close()
		_ = dbB.Close()
	})

	ctx := context.Background()
	pinnedA1, err := dbA1.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pinnedA2, err := dbA2.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pinnedB, err := dbB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Worker 1 acquires session advisory lock on target_prod.
	lockA1, err := AcquireTargetSessionLock(ctx, pinnedA1)
	if err != nil {
		t.Fatalf("AcquireTargetSessionLock(A1) error = %v", err)
	}
	if lockA1.DatabaseName() != "target_prod" {
		t.Fatalf("DatabaseName() = %q, want target_prod", lockA1.DatabaseName())
	}
	k1, k2 := lockA1.Keys()
	if k1 != TargetLockNamespace || k2 != HashDatabaseLockKey("target_prod") {
		t.Fatalf("unexpected lock keys (%x, %x)", k1, k2)
	}

	// Simulate committing multiple batch transactions on pinnedA1; lock must remain held!
	for batch := 1; batch <= 3; batch++ {
		tx, err := lockA1.Conn().BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("BeginTx batch %d: %v", batch, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit batch %d: %v", batch, err)
		}
	}

	// Worker 2 attempting to lock the same physical database via an alias connection must get ErrTargetLockBusy.
	if _, err := AcquireTargetSessionLock(ctx, pinnedA2); !errors.Is(err, ErrTargetLockBusy) {
		t.Fatalf("AcquireTargetSessionLock(A2 alias) while A1 holds lock = %v, want ErrTargetLockBusy", err)
	}

	// Worker 3 locking a different physical database (target_staging) succeeds in parallel!
	lockB, err := AcquireTargetSessionLock(ctx, pinnedB)
	if err != nil {
		t.Fatalf("AcquireTargetSessionLock(B) on different database error = %v", err)
	}
	if err := lockB.Release(ctx); err != nil {
		t.Fatalf("lockB.Release() error = %v", err)
	}

	// Release lockA1 -> now alias connection A2 can acquire the lock on target_prod!
	if err := lockA1.Release(ctx); err != nil {
		t.Fatalf("lockA1.Release() error = %v", err)
	}
	lockA2, err := AcquireTargetSessionLock(ctx, pinnedA2)
	if err != nil {
		t.Fatalf("AcquireTargetSessionLock(A2) after A1 released error = %v", err)
	}
	if err := lockA2.Release(ctx); err != nil {
		t.Fatalf("lockA2.Release() error = %v", err)
	}
}

func TestReleaseDiscardsPhysicalConnectionWhenUnlockFails(t *testing.T) {
	server := &fakeLockServer{}
	connector := &fakeLockConnector{
		server:     server,
		dbName:     "target_test_a",
		failUnlock: true,
	}
	// Configure pool with MaxIdleConns(2) so a normal conn.Close() would keep the connection idle
	// UNLESS discardPhysicalConn marks it with driver.ErrBadConn!
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	pinned, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}

	lock, err := AcquireTargetSessionLock(ctx, pinned)
	if err != nil {
		t.Fatalf("AcquireTargetSessionLock error = %v", err)
	}

	// Because failUnlock=true, pg_advisory_unlock returns false.
	// Release must return ErrTargetLockNotHeld AND discard the physical connection via driver.ErrBadConn!
	err = lock.Release(ctx)
	if !errors.Is(err, ErrTargetLockNotHeld) {
		t.Fatalf("Release() when unlock returns false = %v, want ErrTargetLockNotHeld", err)
	}
	if connector.closed.Load() != 1 {
		t.Fatalf("expected physical driver.Conn to be discarded and closed (closed=1), got %d", connector.closed.Load())
	}
}

