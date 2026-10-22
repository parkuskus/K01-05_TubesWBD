package contracts

import (
	"context"
	"errors"
)

var ErrForbidden = errors.New("forbidden")

// Authorizer checks platform-level and database-level permissions after authentication.
// Platform-admin access does not imply database access; callers must check both
// independently when an operation requires both permissions.
type Authorizer interface {
	RequirePlatformAdmin(ctx context.Context, userID UserID) error
	RequireDatabaseAccess(ctx context.Context, userID UserID, databaseID DatabaseID, action DatabaseAction) error
}
