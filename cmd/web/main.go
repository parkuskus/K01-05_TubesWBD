package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"if3110.local/db-platform/db/migrations"
	"if3110.local/db-platform/internal/api"
	"if3110.local/db-platform/internal/approval"
	"if3110.local/db-platform/internal/auth"
	"if3110.local/db-platform/internal/config"
	"if3110.local/db-platform/internal/database"
	"if3110.local/db-platform/internal/definition"
	"if3110.local/db-platform/internal/execution"
	"if3110.local/db-platform/internal/jobs"
	"if3110.local/db-platform/internal/migration"
	"if3110.local/db-platform/internal/platform"
	"if3110.local/db-platform/internal/telemetry"
	"if3110.local/db-platform/internal/web"
)

func main() {
	if err := run(); err != nil {
		slog.Error("web.stopped", "status", "FAILED", "error", err)
		os.Exit(1)
	}
}

func run() error {
	shutdown, err := telemetry.Setup("teras-data-web", os.Stdout, os.Stderr)
	if err != nil {
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown(ctx); err != nil {
			slog.Error("telemetry.shutdown_failed", "error", err)
		}
	}()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := telemetry.OpenPlatform(cfg.PlatformDatabaseURL)
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
	targetEvidence, err := execution.NewTargetCommitEvidenceReader(dbManager)
	if err != nil {
		return fmt.Errorf("configure target commit evidence reader: %w", err)
	}
	migrationService, err := migration.NewService(platformService, dbManager)
	if err != nil {
		return fmt.Errorf("configure migration service: %w", err)
	}
	apiHandler, err := api.NewAuthHandler(api.AuthConfig{Authenticator: authenticator, Sessions: sessions})
	if err != nil {
		return fmt.Errorf("configure auth API: %w", err)
	}
	platformAPI, err := api.NewPlatformHandler(platformService, platformService, sessions)
	if err != nil {
		return fmt.Errorf("configure platform API: %w", err)
	}
	viewer, err := database.NewViewer(platformService, dbManager)
	if err != nil {
		return fmt.Errorf("configure database viewer: %w", err)
	}
	databaseAPI, err := api.NewDatabaseHandler(dbManager, viewer, platformService, sessions)
	if err != nil {
		return fmt.Errorf("configure database API: %w", err)
	}
	definitions, err := definition.NewService(db, platformService)
	if err != nil {
		return fmt.Errorf("configure migration definitions: %w", err)
	}
	definitionAPI, err := api.NewDefinitionHandler(definitions, sessions)
	if err != nil {
		return fmt.Errorf("configure definition API: %w", err)
	}
	approvals, err := approval.NewService(db, platformService, definitions)
	if err != nil {
		return fmt.Errorf("configure migration approvals: %w", err)
	}
	approvalAPI, err := api.NewApprovalHandler(approvals, sessions, targetEvidence, jobs.NewPostgresJobStore(db).AsContractPublisher())
	if err != nil {
		return fmt.Errorf("configure approval API: %w", err)
	}
	apiRoutes := http.NewServeMux()
	apiRoutes.Handle("/api/v1/users", platformAPI)
	apiRoutes.Handle("/api/v1/users/", platformAPI)
	apiRoutes.Handle("/api/v1/groups", platformAPI)
	apiRoutes.Handle("/api/v1/groups/", platformAPI)
	apiRoutes.Handle("/api/v1/connections", databaseAPI)
	apiRoutes.Handle("/api/v1/connections/", databaseAPI)
	apiRoutes.Handle("/api/v1/databases", databaseAPI)
	apiRoutes.Handle("/api/v1/databases/", databaseAPI)
	apiRoutes.Handle("/api/v1/databases/{id}/migrations", definitionAPI)
	apiRoutes.Handle("/api/v1/databases/{id}/migrations/", definitionAPI)
	apiRoutes.Handle("/api/v1/databases/{id}/operation-requests", approvalAPI)
	apiRoutes.Handle("/api/v1/operation-requests/", approvalAPI)
	apiRoutes.Handle("/api/", apiHandler)
	handler, err := web.New(web.Config{
		Authenticator:   authenticator,
		Sessions:        sessions,
		Authorizer:      platformService,
		Platform:        platformService,
		Databases:       dbManager,
		Migrations:      migrationService,
		CSRFKey:         cfg.CSRFKey,
		SecureCookies:   cfg.SecureCookies,
		API:             apiRoutes,
		FrontendOrigins: cfg.FrontendOrigins,
	})
	if err != nil {
		return fmt.Errorf("configure web routes: %w", err)
	}

	server := &http.Server{Addr: ":" + cfg.Port, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	runCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	slog.Info("web.started", "port", cfg.Port, "status", "RUNNING")
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-runCtx.Done():
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			_ = server.Close()
			return err
		}
		slog.Info("web.stopped", "status", "SUCCESS")
		return nil
	}
}
