package contracts

import (
	"context"
	"errors"
)

var ErrForbidden = errors.New("forbidden")

// Authorizer checks platform-level and database-level permissions.
// Authentication must happen before either method is called.
type Authorizer interface {
	RequirePlatformAdmin(ctx context.Context, userID UserID) error
	RequireDatabaseAccess(ctx context.Context, userID UserID, databaseID DatabaseID, action DatabaseAction) error
}
