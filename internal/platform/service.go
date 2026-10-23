package platform

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"

	"if3110.local/db-platform/internal/auth"
	"if3110.local/db-platform/internal/contracts"
)

var (
	ErrInvalidInput = errors.New("invalid input")
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("conflict")
)

type userRepository interface {
	listUsers(context.Context, int64) ([]User, error)
	createUser(context.Context, int64, string, string) (contracts.UserID, error)
	updateUsername(context.Context, int64, int64, string) error
	setPassword(context.Context, int64, int64, string) error
	setPlatformAdmin(context.Context, int64, int64, bool) error
	deleteUser(context.Context, int64, int64) error
}

type groupRepository interface {
	listGroups(context.Context, int64) ([]Group, error)
	createGroup(context.Context, int64, string) (contracts.GroupID, error)
	renameGroup(context.Context, int64, int64, string) error
	deleteGroup(context.Context, int64, int64) error
	listMembers(context.Context, int64, int64) ([]User, error)
	setMembership(context.Context, int64, int64, int64, bool) error
}

type grantRepository interface {
	listGrants(context.Context, int64, int64) ([]Grant, error)
	setGrant(context.Context, int64, int64, int64, contracts.AccessLevel) error
}

type authorizationRepository interface {
	isPlatformAdmin(context.Context, int64) (bool, error)
	databaseAccess(context.Context, int64, int64) ([]contracts.AccessLevel, error)
}

type Service struct {
	users         userRepository
	groups        groupRepository
	grants        grantRepository
	authorization authorizationRepository
}

func NewService(db *sql.DB) *Service {
	repository := &postgresRepository{db: db}
	return &Service{users: repository, groups: repository, grants: repository, authorization: repository}
}

func positiveID(id contracts.UserID) (int64, error) {
	n, err := strconv.ParseInt(string(id), 10, 64)
	if err != nil || n <= 0 {
		return 0, ErrInvalidInput
	}
	return n, nil
}

func (s *Service) ListUsers(ctx context.Context, actorID contracts.UserID) ([]User, error) {
	actor, err := positiveID(actorID)
	if err != nil {
		return nil, err
	}
	return s.users.listUsers(ctx, actor)
}

func (s *Service) CreateUser(ctx context.Context, actorID contracts.UserID, username, password string) (contracts.UserID, error) {
	actor, err := positiveID(actorID)
	if err != nil {
		return "", err
	}
	username, err = auth.NormalizeUsername(username)
	if err != nil {
		return "", ErrInvalidInput
	}
	if password == "" || len(password) > 72 {
		return "", ErrInvalidInput
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return "", err
	}
	return s.users.createUser(ctx, actor, username, hash)
}

func (s *Service) UpdateUsername(ctx context.Context, actorID, userID contracts.UserID, username string) error {
	actor, err := positiveID(actorID)
	if err != nil {
		return err
	}
	user, err := positiveID(userID)
	if err != nil {
		return err
	}
	username, err = auth.NormalizeUsername(username)
	if err != nil {
		return ErrInvalidInput
	}
	return s.users.updateUsername(ctx, actor, user, username)
}

func (s *Service) SetPassword(ctx context.Context, actorID, userID contracts.UserID, password string) error {
	actor, err := positiveID(actorID)
	if err != nil {
		return err
	}
	user, err := positiveID(userID)
	if err != nil {
		return err
	}
	if password == "" || len(password) > 72 {
		return ErrInvalidInput
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	return s.users.setPassword(ctx, actor, user, hash)
}

func (s *Service) SetPlatformAdmin(ctx context.Context, actorID, userID contracts.UserID, admin bool) error {
	actor, err := positiveID(actorID)
	if err != nil {
		return err
	}
	user, err := positiveID(userID)
	if err != nil {
		return err
	}
	return s.users.setPlatformAdmin(ctx, actor, user, admin)
}

func (s *Service) DeleteUser(ctx context.Context, actorID, userID contracts.UserID) error {
	actor, err := positiveID(actorID)
	if err != nil {
		return err
	}
	user, err := positiveID(userID)
	if err != nil {
		return err
	}
	return s.users.deleteUser(ctx, actor, user)
}

func groupName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return "", ErrInvalidInput
	}
	return name, nil
}

func (s *Service) ListGroups(ctx context.Context, actorID contracts.UserID) ([]Group, error) {
	actor, err := positiveID(actorID)
	if err != nil {
		return nil, err
	}
	return s.groups.listGroups(ctx, actor)
}

func (s *Service) CreateGroup(ctx context.Context, actorID contracts.UserID, name string) (contracts.GroupID, error) {
	actor, err := positiveID(actorID)
	if err != nil {
		return "", err
	}
	name, err = groupName(name)
	if err != nil {
		return "", err
	}
	return s.groups.createGroup(ctx, actor, name)
}

func (s *Service) RenameGroup(ctx context.Context, actorID contracts.UserID, groupID contracts.GroupID, name string) error {
	actor, err := positiveID(actorID)
	if err != nil {
		return err
	}
	group, err := positiveID(contracts.UserID(groupID))
	if err != nil {
		return err
	}
	name, err = groupName(name)
	if err != nil {
		return err
	}
	return s.groups.renameGroup(ctx, actor, group, name)
}

func (s *Service) DeleteGroup(ctx context.Context, actorID contracts.UserID, groupID contracts.GroupID) error {
	actor, err := positiveID(actorID)
	if err != nil {
		return err
	}
	group, err := positiveID(contracts.UserID(groupID))
	if err != nil {
		return err
	}
	return s.groups.deleteGroup(ctx, actor, group)
}

func (s *Service) ListMembers(ctx context.Context, actorID contracts.UserID, groupID contracts.GroupID) ([]User, error) {
	actor, err := positiveID(actorID)
	if err != nil {
		return nil, err
	}
	group, err := positiveID(contracts.UserID(groupID))
	if err != nil {
		return nil, err
	}
	return s.groups.listMembers(ctx, actor, group)
}

func (s *Service) SetMembership(ctx context.Context, actorID, userID contracts.UserID, groupID contracts.GroupID, member bool) error {
	actor, err := positiveID(actorID)
	if err != nil {
		return err
	}
	user, err := positiveID(userID)
	if err != nil {
		return err
	}
	group, err := positiveID(contracts.UserID(groupID))
	if err != nil {
		return err
	}
	return s.groups.setMembership(ctx, actor, user, group, member)
}

func (s *Service) ListGrants(ctx context.Context, actorID contracts.UserID, groupID contracts.GroupID) ([]Grant, error) {
	actor, err := positiveID(actorID)
	if err != nil {
		return nil, err
	}
	group, err := positiveID(contracts.UserID(groupID))
	if err != nil {
		return nil, err
	}
	return s.grants.listGrants(ctx, actor, group)
}

// AccessNone removes a grant instead of storing an access level of zero.
func (s *Service) SetGrant(ctx context.Context, actorID contracts.UserID, groupID contracts.GroupID, databaseID contracts.DatabaseID, level contracts.AccessLevel) error {
	actor, err := positiveID(actorID)
	if err != nil {
		return err
	}
	group, err := positiveID(contracts.UserID(groupID))
	if err != nil {
		return err
	}
	database, err := positiveID(contracts.UserID(databaseID))
	if err != nil {
		return err
	}
	if level > contracts.AccessViewEdit {
		return ErrInvalidInput
	}
	return s.grants.setGrant(ctx, actor, group, database, level)
}
