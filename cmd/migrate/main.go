package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"if3110.local/db-platform/db/migrations"
	"if3110.local/db-platform/internal/config"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	dsn, err := config.LoadPlatformDatabaseURL()
	if err != nil {
		return err
	}
	db, err := sql.Open("pgx", dsn)
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
	log.Printf("platform migrations up to date (%d total, latest version %s)", len(embedded), latestVersion)
	return nil
}

