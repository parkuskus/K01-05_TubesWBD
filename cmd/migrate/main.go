package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"if3110.local/db-platform/db/migrations"
	"if3110.local/db-platform/internal/config"
	"if3110.local/db-platform/internal/telemetry"
)

func main() {
	if err := run(); err != nil {
		slog.Error("migrate.stopped", "status", "FAILED", "error", err)
		os.Exit(1)
	}
}

func run() error {
	shutdown, err := telemetry.Setup("teras-data-migrate", os.Stdout, os.Stderr)
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
	dsn, err := config.LoadPlatformDatabaseURL()
	if err != nil {
		return err
	}
	db, err := telemetry.OpenPlatform(dsn)
	if err != nil {
		return fmt.Errorf("open platform database: %w", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("connect to platform database: %w", err)
	}
	if err := migrations.Apply(ctx, db); err != nil {
		return err
	}
	if err := migrations.VerifyUpToDate(ctx, db); err != nil {
		return err
	}

	embedded, err := migrations.LoadEmbedded()
	if err != nil {
		return err
	}
	latestVersion := "none"
	if len(embedded) > 0 {
		latestVersion = embedded[len(embedded)-1].Version
	}
	slog.Info("migrate.completed", "status", "SUCCESS", "total", len(embedded), "latest_version", latestVersion)
	return nil
}

