param(
    [string]$PgHost = $(if ($env:PGHOST) { $env:PGHOST } else { "localhost" }),
    [string]$PgPort = $(if ($env:PGPORT) { $env:PGPORT } else { "5432" }),
    [string]$PgAdminUser = $(if ($env:PGADMIN_USER) { $env:PGADMIN_USER } else { "postgres" }),
    [string]$AppRole = $(if ($env:APP_ROLE) { $env:APP_ROLE } else { "platform_app" }),
    [string]$AppPassword = $(if ($env:APP_PASSWORD) { $env:APP_PASSWORD } else { "platform_dev_password" }),
    [string]$RoRole = $(if ($env:RO_ROLE) { $env:RO_ROLE } else { "platform_readonly" }),
    [string]$RoPassword = $(if ($env:RO_PASSWORD) { $env:RO_PASSWORD } else { "platform_ro_password" })
)

$ErrorActionPreference = "Stop"

function Invoke-PsqlAdmin {
    param(
        [string]$Database,
        [string]$Command
    )
    & psql -v ON_ERROR_STOP=1 -h $PgHost -p $PgPort -U $PgAdminUser -d $Database -c $Command
    if ($LASTEXITCODE -ne 0) {
        throw "psql command failed on database '$Database'"
    }
}

Write-Host "==> Ensuring test roles ($AppRole, $RoRole) on ${PgHost}:${PgPort}..."
$roleSQL = @"
DO `$`$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '$AppRole') THEN
        CREATE ROLE $AppRole WITH LOGIN PASSWORD '$AppPassword' CREATEDB;
    ELSE
        ALTER ROLE $AppRole WITH LOGIN PASSWORD '$AppPassword' CREATEDB;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '$RoRole') THEN
        CREATE ROLE $RoRole WITH LOGIN PASSWORD '$RoPassword';
    ELSE
        ALTER ROLE $RoRole WITH LOGIN PASSWORD '$RoPassword';
    END IF;
END
`$`$;
"@
Invoke-PsqlAdmin -Database "postgres" -Command $roleSQL

foreach ($db in @("platform_test", "target_test", "target_test_b")) {
    Write-Host "==> Recreating disposable database: $db..."
    Invoke-PsqlAdmin -Database "postgres" -Command "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '$db' AND pid <> pg_backend_pid();"
    Invoke-PsqlAdmin -Database "postgres" -Command "DROP DATABASE IF EXISTS $db;"
    Invoke-PsqlAdmin -Database "postgres" -Command "CREATE DATABASE $db OWNER $AppRole;"
}

foreach ($db in @("target_test", "target_test_b")) {
    Write-Host "==> Configuring read-only privileges on $db for $RoRole..."
    Invoke-PsqlAdmin -Database $db -Command "GRANT CONNECT ON DATABASE $db TO $RoRole; GRANT USAGE ON SCHEMA public TO $RoRole; ALTER ROLE $RoRole IN DATABASE $db SET default_transaction_read_only = on;"
}

Write-Host "==> Disposable test databases ready."

