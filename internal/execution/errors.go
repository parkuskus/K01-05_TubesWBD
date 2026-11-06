package execution

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"if3110.local/db-platform/internal/contracts"
)

// ErrorCategory classifies execution failures into permanent (non-retryable),
// transient (eligible for bounded backoff retry when occurring before commit),
// or unknown-commit (ambiguous commit outcome requiring proof reconciliation).
type ErrorCategory string

const (
	ErrorCategoryPermanent     ErrorCategory = "PERMANENT"
	ErrorCategoryTransient     ErrorCategory = "TRANSIENT"
	ErrorCategoryUnknownCommit ErrorCategory = "UNKNOWN_COMMIT"
)

// ClassifyPreCommitError classifies an error that occurred before tx.Commit()
// was invoked (when the target transaction is guaranteed to roll back).
func ClassifyPreCommitError(err error) ErrorCategory {
	if err == nil {
		return ErrorCategoryPermanent
	}
	if errors.Is(err, contracts.ErrStaleApproval) ||
		errors.Is(err, contracts.ErrHeadMismatch) ||
		errors.Is(err, contracts.ErrIrreversibleMigration) ||
		errors.Is(err, contracts.ErrExecutionGateBlocked) ||
		errors.Is(err, contracts.ErrInvalidExecutionPlan) ||
		errors.Is(err, ErrForbiddenManualTxControl) ||
		errors.Is(err, ErrForbiddenInternalSchema) ||
		errors.Is(err, ErrUnsupportedDirection) {
		return ErrorCategoryPermanent
	}

	if errors.Is(err, ErrTargetLockBusy) ||
		errors.Is(err, driver.ErrBadConn) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, context.DeadlineExceeded) {
		return ErrorCategoryTransient
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return ErrorCategoryTransient
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		code := strings.TrimSpace(pgErr.Code)
		switch {
		// Class 08 — Connection Exception
		case strings.HasPrefix(code, "08"):
			return ErrorCategoryTransient
		// Class 40 — Transaction Rollback (40001 serialization_failure, 40P01 deadlock_detected)
		case strings.HasPrefix(code, "40"):
			return ErrorCategoryTransient
		// 55P03 — lock_not_available
		case code == "55P03":
			return ErrorCategoryTransient
		// 53300 — too_many_connections
		case code == "53300":
			return ErrorCategoryTransient
		// 57014 — query_canceled (e.g. statement_timeout) or 57P01/57P02/57P03 admin/crash shutdown
		case code == "57P01" || code == "57P02" || code == "57P03":
			return ErrorCategoryTransient
		default:
			// Syntax errors (42xxx), constraint violations (23xxx), data exceptions (22xxx), etc.
			return ErrorCategoryPermanent
		}
	}

	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "connection reset") ||
		strings.Contains(lower, "broken pipe") ||
		strings.Contains(lower, "connection refused") ||
		strings.Contains(lower, "deadlock detected") ||
		strings.Contains(lower, "could not serialize access") ||
		strings.Contains(lower, "lock timeout") {
		return ErrorCategoryTransient
	}

	return ErrorCategoryPermanent
}

// ClassifyCommitError classifies an error returned by tx.Commit().
//
// Unless PostgreSQL explicitly returned a deterministic SQLSTATE proving that the
// server rolled back the transaction on commit (such as a deferred constraint
// violation 23xxx or transaction rollback 40xxx), a commit error is classified
// as ErrorCategoryUnknownCommit because the commit may have succeeded on the
// server before the connection dropped.
func ClassifyCommitError(err error) ErrorCategory {
	if err == nil {
		return ErrorCategoryPermanent
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		code := strings.TrimSpace(pgErr.Code)
		switch {
		case strings.HasPrefix(code, "23"), strings.HasPrefix(code, "22"), strings.HasPrefix(code, "42"):
			return ErrorCategoryPermanent
		case strings.HasPrefix(code, "40"):
			return ErrorCategoryTransient
		}
	}

	return ErrorCategoryUnknownCommit
}

// IsTransientPreCommitError reports whether a pre-commit error is safe for
// bounded automatic retry with exponential backoff.
func IsTransientPreCommitError(err error) bool {
	return ClassifyPreCommitError(err) == ErrorCategoryTransient
}

