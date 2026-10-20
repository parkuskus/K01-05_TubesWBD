package contracts

import (
	"context"
	"database/sql"
)

// DatabaseProvider returns the managed pool for a target database.
// The provider owns the pool lifecycle; callers must not close it.
type DatabaseProvider interface {
	Pool(ctx context.Context, databaseID DatabaseID) (*sql.DB, error)
}
