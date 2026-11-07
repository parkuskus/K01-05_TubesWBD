#!/usr/bin/env bash
set -euo pipefail

# Setup isolated disposable PostgreSQL databases for Milestone 2 testing:
# - platform_test   : platform metadata test database
# - target_test     : primary disposable target database (Target A)
# - target_test_b   : secondary disposable target database (Target B for parallel multi-target tests)
# Plus a read-only role (platform_readonly) for read-only migration integration tests.

PGHOST="${PGHOST:-localhost}"
PGPORT="${PGPORT:-5432}"
PGADMIN_USER="${PGADMIN_USER:-postgres}"
APP_ROLE="${APP_ROLE:-platform_app}"
APP_PASSWORD="${APP_PASSWORD:-platform_dev_password}"
RO_ROLE="${RO_ROLE:-platform_readonly}"
RO_PASSWORD="${RO_PASSWORD:-platform_ro_password}"

psql_admin() {
	psql -v ON_ERROR_STOP=1 -h "$PGHOST" -p "$PGPORT" -U "$PGADMIN_USER" "$@"
}

echo "==> Ensuring test roles (${APP_ROLE}, ${RO_ROLE}) on ${PGHOST}:${PGPORT}..."
psql_admin -d postgres <<-EOSQL
	DO \$\$
	BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '${APP_ROLE}') THEN
			CREATE ROLE ${APP_ROLE} WITH LOGIN PASSWORD '${APP_PASSWORD}' CREATEDB;
		ELSE
			ALTER ROLE ${APP_ROLE} WITH LOGIN PASSWORD '${APP_PASSWORD}' CREATEDB;
		END IF;

		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '${RO_ROLE}') THEN
			CREATE ROLE ${RO_ROLE} WITH LOGIN PASSWORD '${RO_PASSWORD}';
		ELSE
			ALTER ROLE ${RO_ROLE} WITH LOGIN PASSWORD '${RO_PASSWORD}';
		END IF;
	END
	\$\$;
EOSQL

for db in platform_test target_test target_test_a target_test_b; do
	echo "==> Recreating disposable database: ${db}..."
	psql_admin -d postgres -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '${db}' AND pid <> pg_backend_pid();" >/dev/null
	psql_admin -d postgres -c "DROP DATABASE IF EXISTS ${db};"
	psql_admin -d postgres -c "CREATE DATABASE ${db} OWNER ${APP_ROLE};"
done

echo "==> Configuring read-only privileges on target_test, target_test_a, and target_test_b for ${RO_ROLE}..."
for db in target_test target_test_a target_test_b; do
	psql_admin -d "$db" <<-EOSQL
		GRANT CONNECT ON DATABASE ${db} TO ${RO_ROLE};
		GRANT USAGE ON SCHEMA public TO ${RO_ROLE};
		ALTER role ${RO_ROLE} IN DATABASE ${db} SET default_transaction_read_only = on;
	EOSQL
done

echo "==> Disposable test databases ready:"
echo "  PLATFORM_TEST_DATABASE_URL=postgres://${APP_ROLE}:<password>@${PGHOST}:${PGPORT}/platform_test?sslmode=disable"
echo "  TARGET_TEST_DATABASE_URL=postgres://${APP_ROLE}:<password>@${PGHOST}:${PGPORT}/target_test_a?sslmode=disable"
echo "  TARGET_TEST_DATABASE_B_URL=postgres://${APP_ROLE}:<password>@${PGHOST}:${PGPORT}/target_test_b?sslmode=disable"
echo "  MIGRATION_READONLY_TEST_DATABASE_URL=postgres://${RO_ROLE}:<password>@${PGHOST}:${PGPORT}/target_test_a?sslmode=disable"

