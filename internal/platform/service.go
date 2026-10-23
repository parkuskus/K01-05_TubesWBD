package platform

import (
	"context"
	"database/sql"
	"errors"
	"strconv"

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

type Service struct{ users userRepository }

func NewService(db *sql.DB) *Service { return &Service{users: &postgresRepository{db: db}} }

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
