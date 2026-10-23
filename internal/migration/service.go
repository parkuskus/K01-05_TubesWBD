package migration

import (
	"context"
	"strings"
	"time"

	"if3110.local/db-platform/internal/contracts"
)

// layanan utama buat ngatur alur migration dan cek hak akses user
type Service struct {
	authorizer contracts.Authorizer
	dbProvider contracts.DatabaseProvider
	now        func() time.Time
}

func NewService(authorizer contracts.Authorizer, dbProvider contracts.DatabaseProvider) (*Service, error) {
	if authorizer == nil || dbProvider == nil {
		return nil, ErrInvalidServiceConfig
	}
	return &Service{
		authorizer: authorizer,
		dbProvider: dbProvider,
		now:        time.Now,
	}, nil
}

// ambil daftar riwayat migration secara linear buat database tertentu
func (s *Service) ListHistory(ctx context.Context, userID contracts.UserID, dbID contracts.DatabaseID) ([]MigrationRecord, error) {
	if err := validateIdentity(userID, dbID); err != nil {
		return nil, err
	}
	if err := s.authorizer.RequireDatabaseAccess(ctx, userID, dbID, contracts.ActionViewMigrationHistory); err != nil {
		return nil, err
	}

	store, err := s.openStore(ctx, dbID)
	if err != nil {
		return nil, err
	}
	return store.List(ctx)
}

// ambil detail satu migration berdasarkan nomor versinya
func (s *Service) GetMigration(ctx context.Context, userID contracts.UserID, dbID contracts.DatabaseID, version int64) (MigrationRecord, error) {
	if err := validateIdentity(userID, dbID); err != nil {
		return MigrationRecord{}, err
	}
	if version <= 0 {
		return MigrationRecord{}, ErrInvalidVersion
	}
	if err := s.authorizer.RequireDatabaseAccess(ctx, userID, dbID, contracts.ActionViewMigrationHistory); err != nil {
		return MigrationRecord{}, err
	}

	store, err := s.openStore(ctx, dbID)
	if err != nil {
		return MigrationRecord{}, err
	}
	return store.GetByVersion(ctx, version)
}

// cek apakah user punya hak buat bikin migration di database target
func (s *Service) RequireCreateAccess(ctx context.Context, userID contracts.UserID, dbID contracts.DatabaseID) error {
	if err := validateIdentity(userID, dbID); err != nil {
		return err
	}
	return s.authorizer.RequireDatabaseAccess(ctx, userID, dbID, contracts.ActionCreateMigration)
}

// buat draft migration baru (wajib punya akses view + edit)
func (s *Service) CreateDraft(ctx context.Context, userID contracts.UserID, dbID contracts.DatabaseID, title, sqlText string) (MigrationRecord, error) {
	if err := validateIdentity(userID, dbID); err != nil {
		return MigrationRecord{}, err
	}
	if err := s.authorizer.RequireDatabaseAccess(ctx, userID, dbID, contracts.ActionCreateMigration); err != nil {
		return MigrationRecord{}, err
	}
	if _, err := normalizeSQL(sqlText); err != nil {
		return MigrationRecord{}, err
	}

	store, err := s.openStore(ctx, dbID)
	if err != nil {
		return MigrationRecord{}, err
	}
	return store.CreateDraft(ctx, title, sqlText, userID, s.currentTime())
}

// ubah draft migration yang belum pernah dieksekusi (wajib punya akses view + edit)
func (s *Service) UpdateDraft(ctx context.Context, userID contracts.UserID, dbID contracts.DatabaseID, version int64, title, sqlText string) (MigrationRecord, error) {
	if err := validateIdentity(userID, dbID); err != nil {
		return MigrationRecord{}, err
	}
	if version <= 0 {
		return MigrationRecord{}, ErrInvalidVersion
	}
	if err := s.authorizer.RequireDatabaseAccess(ctx, userID, dbID, contracts.ActionEditMigration); err != nil {
		return MigrationRecord{}, err
	}
	if _, err := normalizeSQL(sqlText); err != nil {
		return MigrationRecord{}, err
	}

	store, err := s.openStore(ctx, dbID)
	if err != nil {
		return MigrationRecord{}, err
	}
	return store.UpdateDraft(ctx, version, title, sqlText, s.currentTime())
}

// jalankan draft migration yang udah disimpan di dalam transaksi
func (s *Service) ExecuteMigration(ctx context.Context, userID contracts.UserID, dbID contracts.DatabaseID, version int64) (MigrationRecord, error) {
	if err := validateIdentity(userID, dbID); err != nil {
		return MigrationRecord{}, err
	}
	if version <= 0 {
		return MigrationRecord{}, ErrInvalidVersion
	}
	if err := s.authorizer.RequireDatabaseAccess(ctx, userID, dbID, contracts.ActionRunMigration); err != nil {
		return MigrationRecord{}, err
	}

	store, err := s.openStore(ctx, dbID)
	if err != nil {
		return MigrationRecord{}, err
	}
	return store.ExecuteDraft(ctx, version, userID, s.currentTime())
}

// buat dan langsung eksekusi sql migration baru di dalam transaksi
func (s *Service) RunMigration(ctx context.Context, userID contracts.UserID, dbID contracts.DatabaseID, title, sqlText string) (MigrationRecord, error) {
	if err := validateIdentity(userID, dbID); err != nil {
		return MigrationRecord{}, err
	}
	if err := s.authorizer.RequireDatabaseAccess(ctx, userID, dbID, contracts.ActionRunMigration); err != nil {
		return MigrationRecord{}, err
	}
	if _, err := normalizeSQL(sqlText); err != nil {
		return MigrationRecord{}, err
	}

	store, err := s.openStore(ctx, dbID)
	if err != nil {
		return MigrationRecord{}, err
	}
	return store.RunNew(ctx, title, sqlText, userID, s.currentTime())
}

// ambil pool koneksi dari provider dan pastikan tabel riwayat udah siap
func (s *Service) openStore(ctx context.Context, dbID contracts.DatabaseID) (*PostgresStore, error) {
	db, err := s.dbProvider.Pool(ctx, dbID)
	if err != nil {
		return nil, err
	}
	store, err := NewPostgresStore(db, dbID)
	if err != nil {
		return nil, err
	}
	if err := store.EnsureSchema(ctx); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Service) currentTime() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

func validateIdentity(userID contracts.UserID, dbID contracts.DatabaseID) error {
	if strings.TrimSpace(string(userID)) == "" {
		return ErrInvalidUser
	}
	if strings.TrimSpace(string(dbID)) == "" {
		return ErrInvalidDatabaseID
	}
	return nil
}

