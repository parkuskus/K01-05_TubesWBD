#!/usr/bin/env bash
set -euo pipefail

TARGET_DSN="${TARGET_TEST_DATABASE_URL:-postgres://postgres:postgres@localhost:5432/target_test_a?sslmode=disable}"
ROWS="${BENCHMARK_ROWS:-1000000}"

echo "==> [1/2] Calibrating deterministic benchmark dataset (>1s single UPDATE) on ${TARGET_DSN}"
go run ./cmd/seedbench -dsn "${TARGET_DSN}" -rows "${ROWS}" -mode calibrate

echo "==> [2/2] Running PERF-01 Single UPDATE vs Mode Batch & Fail+Resume benchmark"
TARGET_TEST_DATABASE_URL="${TARGET_DSN}" RUN_PERF_BENCHMARK=1 \
  go test -v -count=1 -timeout 25m -run TestPerf01BatchVsSingleUpdateBenchmarkIntegration ./internal/execution/...
