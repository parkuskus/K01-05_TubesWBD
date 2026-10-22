package contracts

import (
	"context"
	"database/sql"
)

// DatabaseProvider resolves a registered target connection by ID and returns its managed pool.
// Connection metadata and credentials stay inside the provider boundary; consumers
// receive no DSN or password. The provider owns pool lifecycle, so callers must not close it.
type DatabaseProvider interface {
	Pool(ctx context.Context, databaseID DatabaseID) (*sql.DB, error)
}
