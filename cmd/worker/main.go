package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"if3110.local/db-platform/db/migrations"
	"if3110.local/db-platform/internal/config"
	"if3110.local/db-platform/internal/database"
	"if3110.local/db-platform/internal/execution"
	"if3110.local/db-platform/internal/jobs"
	"if3110.local/db-platform/internal/platform"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("worker exited with error", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	db, err := sql.Open("pgx", cfg.PlatformDatabaseURL)
	if err != nil {
		return fmt.Errorf("open platform database: %w", err)
	}
	defer func() {
		_ = db.Close()
	}()

	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelStartup()

	if err := db.PingContext(startupCtx); err != nil {
		return fmt.Errorf("connect to platform database: %w", err)
	}
	if err := migrations.Apply(startupCtx, db); err != nil {
		return fmt.Errorf("apply platform migrations: %w", err)
	}
	if err := migrations.VerifyUpToDate(startupCtx, db); err != nil {
		return fmt.Errorf("verify platform migrations: %w", err)
	}

	platformService := platform.NewService(db)
	dbManager, err := database.NewManager(db, platformService, cfg.DatabaseCredentialKey)
	if err != nil {
		return fmt.Errorf("configure database manager: %w", err)
	}
	defer func() {
		_ = dbManager.Close()
	}()
	dbManager.SetExecutorTimeout(cfg.TargetExecutionTimeout)

	jobStore := jobs.NewPostgresJobStore(db).WithDefaultMaxRetries(cfg.JobMaxRetries)

	worker, err := execution.NewWorker(execution.WorkerConfig{
		WorkerID:          cfg.WorkerID,
		Concurrency:       cfg.WorkerConcurrency,
		PollInterval:      cfg.WorkerPollInterval,
		LeaseDuration:     cfg.JobLeaseDuration,
		HeartbeatInterval: cfg.JobHeartbeatInterval,
		ExecutionTimeout:  cfg.TargetExecutionTimeout,
		Queue:             jobStore,
		ConnProvider:      dbManager,
		Logger:            logger,
	})
	if err != nil {
		return fmt.Errorf("configure worker: %w", err)
	}

	runCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	logger.Info(
		"worker started",
		slog.String("worker_id", cfg.WorkerID),
		slog.Int("concurrency", cfg.WorkerConcurrency),
		slog.Duration("poll_interval", cfg.WorkerPollInterval),
		slog.Duration("lease_duration", cfg.JobLeaseDuration),
		slog.Duration("heartbeat_interval", cfg.JobHeartbeatInterval),
		slog.Duration("execution_timeout", cfg.TargetExecutionTimeout),
	)

	if err := worker.Run(runCtx); err != nil {
		return err
	}

	logger.Info("worker stopped gracefully", slog.String("worker_id", cfg.WorkerID))
	return nil
}

