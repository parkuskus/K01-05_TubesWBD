package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	defaultBenchmarkRows = int64(1_000_000)
	maxCalibrateRows     = int64(10_000_000)

	benchmarkSingleUpdateSQL = `
		UPDATE public.bench_orders
		SET amount_cents = amount_cents + 100,
		    loyalty_tier = 'GOLD',
		    updated_flag = TRUE
		WHERE status = 'SETTLED'
	`
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	dsnFlag := flag.String("dsn", "", "PostgreSQL target database DSN (defaults to TARGET_TEST_DATABASE_URL or PLATFORM_DATABASE_URL)")
	rowsFlag := flag.Int64("rows", defaultBenchmarkRows, "Number of deterministic rows to generate with generate_series(1, N)")
	modeFlag := flag.String("mode", "seed", "Operation mode: seed | reset | calibrate")
	minDurationFlag := flag.Duration("min-duration", 1100*time.Millisecond, "Minimum single UPDATE + COMMIT duration when mode=calibrate")
	flag.Parse()

	if err := run(logger, *dsnFlag, *rowsFlag, *modeFlag, *minDurationFlag); err != nil {
		logger.Error("seedbench failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run(logger *slog.Logger, dsn string, rows int64, mode string, minDuration time.Duration) error {
	dsn = resolveDSN(dsn)
	if dsn == "" {
		return errors.New("database DSN is required (-dsn or TARGET_TEST_DATABASE_URL)")
	}
	if rows <= 0 {
		rows = defaultBenchmarkRows
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() {
		_ = db.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "seed":
		start := time.Now()
		if err := seedBenchmarkTable(ctx, db, rows); err != nil {
			return err
		}
		logger.Info(
			"benchmark dataset seeded",
			slog.Int64("rows", rows),
			slog.Duration("duration", time.Since(start)),
		)
		return nil

	case "reset":
		start := time.Now()
		if err := resetBenchmarkTable(ctx, db, rows); err != nil {
			return err
		}
		logger.Info(
			"benchmark dataset reset to identical baseline",
			slog.Int64("rows", rows),
			slog.Duration("duration", time.Since(start)),
		)
		return nil

	case "calibrate":
		calibratedRows, elapsed, err := calibrateBenchmarkRows(ctx, logger, db, rows, minDuration)
		if err != nil {
			return err
		}
		logger.Info(
			"benchmark dataset calibrated and restored to clean initial state",
			slog.Int64("calibrated_rows", calibratedRows),
			slog.Duration("single_update_duration", elapsed),
		)
		return nil

	default:
		return fmt.Errorf("unsupported mode %q (expected seed, reset, or calibrate)", mode)
	}
}

func resolveDSN(explicit string) string {
	if s := strings.TrimSpace(explicit); s != "" {
		return s
	}
	for _, envKey := range []string{
		"TARGET_TEST_DATABASE_URL",
		"TEST_TARGET_DATABASE_URL",
		"PLATFORM_TEST_DATABASE_URL",
		"PLATFORM_DATABASE_URL",
	} {
		if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
			return v
		}
	}
	return ""
}

func seedBenchmarkTable(ctx context.Context, db *sql.DB, totalRows int64) error {
	const dropAndCreateSQL = `
		DROP TABLE IF EXISTS public.bench_orders;

		CREATE TABLE public.bench_orders (
			id BIGINT PRIMARY KEY,
			customer_code TEXT NOT NULL,
			amount_cents BIGINT NOT NULL,
			status TEXT NOT NULL,
			loyalty_tier TEXT NOT NULL DEFAULT 'STANDARD',
			updated_flag BOOLEAN NOT NULL DEFAULT FALSE
		);
	`
	if _, err := db.ExecContext(ctx, dropAndCreateSQL); err != nil {
		return fmt.Errorf("create bench_orders table: %w", err)
	}

	const insertSeriesSQL = `
		INSERT INTO public.bench_orders (
			id,
			customer_code,
			amount_cents,
			status,
			loyalty_tier,
			updated_flag
		)
		SELECT
			gs AS id,
			'CUST-' || lpad(((gs % 100000) + 1)::text, 6, '0') AS customer_code,
			((gs * 37) % 500000) + 1000 AS amount_cents,
			CASE
				WHEN gs % 10 = 0 THEN 'CANCELLED'
				ELSE 'SETTLED'
			END AS status,
			'STANDARD' AS loyalty_tier,
			FALSE AS updated_flag
		FROM generate_series(1, $1::bigint) AS gs
	`
	if _, err := db.ExecContext(ctx, insertSeriesSQL, totalRows); err != nil {
		return fmt.Errorf("populate bench_orders via generate_series(1, %d): %w", totalRows, err)
	}

	const createIndexesSQL = `
		CREATE INDEX bench_orders_status_id_idx
			ON public.bench_orders (status, id);

		CREATE INDEX bench_orders_loyalty_tier_idx
			ON public.bench_orders (loyalty_tier, customer_code);
	`
	if _, err := db.ExecContext(ctx, createIndexesSQL); err != nil {
		return fmt.Errorf("create bench_orders indexes: %w", err)
	}

	if _, err := db.ExecContext(ctx, `VACUUM ANALYZE public.bench_orders`); err != nil {
		return fmt.Errorf("vacuum analyze bench_orders: %w", err)
	}
	return nil
}

// resetBenchmarkTable rebuilds public.bench_orders with identical initial rows,
// identical B-tree indexes, and zero dead-tuple bloat outside the benchmark timer.
func resetBenchmarkTable(ctx context.Context, db *sql.DB, totalRows int64) error {
	return seedBenchmarkTable(ctx, db, totalRows)
}

func calibrateBenchmarkRows(
	ctx context.Context,
	logger *slog.Logger,
	db *sql.DB,
	initialRows int64,
	minDuration time.Duration,
) (int64, time.Duration, error) {
	if minDuration <= 0 {
		minDuration = 1100 * time.Millisecond
	}
	rows := initialRows
	if rows <= 0 {
		rows = defaultBenchmarkRows
	}

	for {
		if err := seedBenchmarkTable(ctx, db, rows); err != nil {
			return 0, 0, err
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return 0, 0, fmt.Errorf("begin calibration tx: %w", err)
		}

		start := time.Now()
		if _, err := tx.ExecContext(ctx, benchmarkSingleUpdateSQL); err != nil {
			_ = tx.Rollback()
			return 0, 0, fmt.Errorf("calibration single update: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return 0, 0, fmt.Errorf("commit calibration tx: %w", err)
		}
		elapsed := time.Since(start)

		logger.Info(
			"calibration probe finished",
			slog.Int64("rows", rows),
			slog.Duration("elapsed", elapsed),
			slog.Duration("target_min_duration", minDuration),
		)

		if elapsed >= minDuration || rows >= maxCalibrateRows {
			if err := resetBenchmarkTable(ctx, db, rows); err != nil {
				return 0, 0, err
			}
			return rows, elapsed, nil
		}

		nextRows := rows * 2
		if elapsed > 100*time.Millisecond {
			ratio := float64(minDuration) / float64(elapsed)
			scaled := int64(float64(rows) * ratio * 1.25)
			if scaled > rows {
				nextRows = scaled
			}
		}
		if nextRows > maxCalibrateRows {
			nextRows = maxCalibrateRows
		}
		rows = nextRows
	}
}
