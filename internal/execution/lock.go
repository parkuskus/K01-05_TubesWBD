package execution

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"if3110.local/db-platform/internal/contracts"
)

// TargetLockNamespace ("IF31" in ASCII) namespaces all target database session
// advisory locks acquired by RangkaiDB workers.
const TargetLockNamespace int32 = 0x49463331

const unlockTimeout = 5 * time.Second

var (
	ErrTargetLockBusy    = errors.New("target database session advisory lock is already held")
	ErrTargetLockNotHeld = errors.New("target database session advisory lock was not held on unlock")
	ErrNilTargetConn     = errors.New("target executor connection is nil")
)

// HashDatabaseLockKey derives a deterministic 32-bit advisory lock key from a
// physical PostgreSQL database name.
func HashDatabaseLockKey(databaseName string) int32 {
	normalized := strings.ToLower(strings.TrimSpace(databaseName))
	h := fnv.New32a()
	_, _ = h.Write([]byte("rangkaiedb.target.lock:"))
	_, _ = h.Write([]byte(normalized))
	return int32(h.Sum32())
}

// TargetSessionLock represents an active PostgreSQL session-level advisory lock
// held on a single pinned *sql.Conn across one or more target transactions.
type TargetSessionLock struct {
	mu           sync.Mutex
	conn         *sql.Conn
	databaseName string
	key1         int32
	key2         int32
	released     bool
}

// AcquireTargetSessionLock attempts to acquire a non-blocking PostgreSQL session
// advisory lock (pg_try_advisory_lock) on the provided pinned *sql.Conn.
//
// Because the lock is bound to the session rather than a single transaction,
// it stays held across multiple batch COMMITs on the same *sql.Conn.
// If the lock is currently held by another session, AcquireTargetSessionLock
// returns ErrTargetLockBusy without executing any target DDL/DML.
func AcquireTargetSessionLock(ctx context.Context, conn *sql.Conn) (*TargetSessionLock, error) {
	if conn == nil {
		return nil, ErrNilTargetConn
	}

	var dbName string
	if err := conn.QueryRowContext(ctx, `SELECT current_database()`).Scan(&dbName); err != nil {
		return nil, fmt.Errorf("query target current_database: %w", err)
	}

	key1 := TargetLockNamespace
	key2 := HashDatabaseLockKey(dbName)

	var acquired bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1, $2)`, key1, key2).Scan(&acquired); err != nil {
		return nil, fmt.Errorf("acquire target advisory lock: %w", err)
	}
	if !acquired {
		return nil, ErrTargetLockBusy
	}

	return &TargetSessionLock{
		conn:         conn,
		databaseName: dbName,
		key1:         key1,
		key2:         key2,
	}, nil
}

// Conn returns the pinned *sql.Conn holding the session advisory lock.
func (l *TargetSessionLock) Conn() *sql.Conn {
	if l == nil {
		return nil
	}
	return l.conn
}

// DatabaseName returns the physical database name reported by current_database().
func (l *TargetSessionLock) DatabaseName() string {
	if l == nil {
		return ""
	}
	return l.databaseName
}

// Keys returns the (key1, key2) pair used for pg_try_advisory_lock / pg_advisory_unlock.
func (l *TargetSessionLock) Keys() (int32, int32) {
	if l == nil {
		return 0, 0
	}
	return l.key1, l.key2
}

// Release unlocks the PostgreSQL session advisory lock on the pinned connection
// and closes the *sql.Conn.
//
// If pg_advisory_unlock fails or returns false, Release marks the underlying
// driver connection with driver.ErrBadConn before closing so database/sql
// discards the physical TCP session and never returns a locked session to any pool.
func (l *TargetSessionLock) Release(ctx context.Context) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return nil
	}
	l.released = true

	if l.conn == nil {
		return nil
	}

	baseCtx := context.Background()
	if ctx != nil {
		baseCtx = context.WithoutCancel(ctx)
	}
	unlockCtx, cancel := context.WithTimeout(baseCtx, unlockTimeout)
	defer cancel()

	var unlocked bool
	unlockErr := l.conn.QueryRowContext(unlockCtx, `SELECT pg_advisory_unlock($1, $2)`, l.key1, l.key2).Scan(&unlocked)
	if unlockErr != nil || !unlocked {
		discardPhysicalConn(l.conn)
		_ = l.conn.Close()
		if unlockErr != nil {
			return fmt.Errorf("unlock target advisory lock: %w", unlockErr)
		}
		return ErrTargetLockNotHeld
	}

	if closeErr := l.conn.Close(); closeErr != nil {
		return fmt.Errorf("close target executor conn: %w", closeErr)
	}
	return nil
}

func discardPhysicalConn(conn *sql.Conn) {
	if conn == nil {
		return
	}
	_ = conn.Raw(func(_ any) error {
		return driver.ErrBadConn
	})
}

// GateRecord represents an active row in the platform database_execution_gates table.
type GateRecord struct {
	DatabaseID  contracts.DatabaseID
	ExecutionID contracts.ExecutionID
	JobID       contracts.JobID
	Reason      contracts.GateReason
	Details     string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// UpsertExecutionGate sets or updates an execution gate on the platform database
// to block subsequent migrations while a database is in PARTIAL, PARTIALLY_REVERTED, or UNKNOWN state.
func UpsertExecutionGate(
	ctx context.Context,
	platformDB *sql.DB,
	databaseID contracts.DatabaseID,
	executionID contracts.ExecutionID,
	jobID contracts.JobID,
	reason contracts.GateReason,
	details string,
) error {
	if platformDB == nil {
		return errors.New("platform database is required")
	}
	if !reason.IsValid() {
		return fmt.Errorf("invalid execution gate reason %q", reason)
	}
	_, err := platformDB.ExecContext(
		ctx,
		`INSERT INTO database_execution_gates (database_id, execution_id, job_id, reason, details, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, NOW(), NOW())
		 ON CONFLICT (database_id) DO UPDATE
		 SET execution_id = EXCLUDED.execution_id,
		     job_id = EXCLUDED.job_id,
		     reason = EXCLUDED.reason,
		     details = EXCLUDED.details,
		     updated_at = NOW()`,
		string(databaseID),
		string(executionID),
		string(jobID),
		string(reason),
		details,
	)
	if err != nil {
		return fmt.Errorf("upsert execution gate for %s: %w", databaseID, err)
	}
	return nil
}

// ClearExecutionGate removes an execution gate from the platform database once recovery completes.
func ClearExecutionGate(ctx context.Context, platformDB *sql.DB, databaseID contracts.DatabaseID) error {
	if platformDB == nil {
		return errors.New("platform database is required")
	}
	_, err := platformDB.ExecContext(
		ctx,
		`DELETE FROM database_execution_gates WHERE database_id = $1`,
		string(databaseID),
	)
	if err != nil {
		return fmt.Errorf("clear execution gate for %s: %w", databaseID, err)
	}
	return nil
}

