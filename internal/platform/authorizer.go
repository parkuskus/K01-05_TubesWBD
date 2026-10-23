package platform

import (
	"context"

	"if3110.local/db-platform/internal/contracts"
)

var _ contracts.Authorizer = (*Service)(nil)

func (s *Service) RequirePlatformAdmin(ctx context.Context, userID contracts.UserID) error {
	user, err := positiveID(userID)
	if err != nil {
		return contracts.ErrForbidden
	}
	admin, err := s.authorization.isPlatformAdmin(ctx, user)
	if err != nil {
		return err
	}
	if !admin {
		return contracts.ErrForbidden
	}
	return nil
}

func (s *Service) RequireDatabaseAccess(ctx context.Context, userID contracts.UserID, databaseID contracts.DatabaseID, action contracts.DatabaseAction) error {
	user, err := positiveID(userID)
	if err != nil {
		return contracts.ErrForbidden
	}
	database, err := positiveID(contracts.UserID(databaseID))
	if err != nil {
		return contracts.ErrForbidden
	}
	if !contracts.AccessViewEdit.Allows(action) {
		return contracts.ErrForbidden
	}
	grants, err := s.authorization.databaseAccess(ctx, user, database)
	if err != nil {
		return err
	}
	if !contracts.EffectiveAccess(grants).Allows(action) {
		return contracts.ErrForbidden
	}
	return nil
}
