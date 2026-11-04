.PHONY: generate migrate run worker test test-unit test-integration setup-test-dbs

generate:
	go run github.com/a-h/templ/cmd/templ@v0.3.960 generate

migrate:
	go run ./cmd/migrate

run: generate
	go run ./cmd/web

worker:
	go run ./cmd/worker

test: generate
	go test ./...

test-unit: generate
	PLATFORM_TEST_DATABASE_URL= TARGET_TEST_DATABASE_URL= TARGET_TEST_DATABASE_B_URL= MIGRATION_READONLY_TEST_DATABASE_URL= go test -count=1 ./...

test-integration: generate
	go test -count=1 -v ./db/migrations/... ./internal/...

setup-test-dbs:
	bash ./scripts/setup-test-dbs.sh
