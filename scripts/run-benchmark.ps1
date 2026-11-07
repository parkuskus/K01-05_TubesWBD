param(
    [string]$TargetDsn = $env:TARGET_TEST_DATABASE_URL,
    [int64]$Rows = 1000000
)

$ErrorActionPreference = "Stop"

if ([string]::IsNullOrWhiteSpace($TargetDsn)) {
    $TargetDsn = "postgres://postgres:postgres@localhost:5432/target_test_a?sslmode=disable"
}

Write-Host "==> [1/2] Calibrating deterministic benchmark dataset (>1s single UPDATE) on $TargetDsn"
go run ./cmd/seedbench -dsn "$TargetDsn" -rows $Rows -mode calibrate

Write-Host "==> [2/2] Running PERF-01 Single UPDATE vs Mode Batch & Fail+Resume benchmark"
$env:TARGET_TEST_DATABASE_URL = $TargetDsn
$env:RUN_PERF_BENCHMARK = "1"
go test -v -count=1 -timeout 25m -run TestPerf01BatchVsSingleUpdateBenchmarkIntegration ./internal/execution/...
