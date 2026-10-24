package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"if3110.local/db-platform/db/migrations"
	"if3110.local/db-platform/internal/auth"
	"if3110.local/db-platform/internal/config"
	"if3110.local/db-platform/internal/database"
	"if3110.local/db-platform/internal/migration"
	"if3110.local/db-platform/internal/platform"
	"if3110.local/db-platform/internal/web"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := sql.Open("pgx", cfg.PlatformDatabaseURL)
	if err != nil {
		return fmt.Errorf("open platform database: %w", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("connect to platform database: %w", err)
	}
	if err := migrations.Apply(ctx, db); err != nil {
		return err
	}
	authenticator := auth.NewAuthenticator(db)
	if err := authenticator.EnsureInitialAdmin(ctx, cfg.InitialAdminUsername, cfg.InitialAdminPassword); err != nil {
		return fmt.Errorf("initialize platform admin: %w", err)
	}
	sessions, err := auth.NewSessionManager(db, auth.SessionConfig{SecureCookie: cfg.SecureCookies})
	if err != nil {
		return fmt.Errorf("configure sessions: %w", err)
	}
	platformService := platform.NewService(db)
	dbManager, err := database.NewManager(db, platformService, cfg.DatabaseCredentialKey)
	if err != nil {
		return fmt.Errorf("configure database manager: %w", err)
	}
	defer dbManager.Close()
	migrationService, err := migration.NewService(platformService, dbManager)
	if err != nil {
		return fmt.Errorf("configure migration service: %w", err)
	}
	handler, err := web.New(web.Config{
		Authenticator: authenticator,
		Sessions:      sessions,
		Authorizer:    platformService,
		Platform:      platformService,
		Databases:     dbManager,
		Migrations:    migrationService,
		CSRFKey:       cfg.CSRFKey,
		SecureCookies: cfg.SecureCookies,
	})
	if err != nil {
		return fmt.Errorf("configure web routes: %w", err)
	}

	log.Printf("listening on :%s", cfg.Port)
	return http.ListenAndServe(":"+cfg.Port, handler)
}
