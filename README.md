# IF3110 Milestone 1 — Database Management Platform

Starter scaffold for the IF3110 web development project.

## Tech stack

- **Go 1.23+** for the backend.
- **Templ** for server-rendered HTML; this is not an SPA.
- **`net/http`** and its standard `ServeMux` for routing.
- **PostgreSQL** as the target database. Use `pgx/v5` when the database layer is implemented; it is not wired into this scaffold yet.
- Plain CSS for the initial page; no frontend framework or client-side router.
- Go's standard `testing` and `httptest` packages.

## Structure

```text
cmd/web/             Application entry point
internal/web/        HTTP routes, handlers, and Templ pages
internal/auth/       Authentication and sessions (placeholder)
internal/config/     Application configuration (placeholder)
internal/database/   PostgreSQL connections and database viewer (placeholder)
internal/migration/  User-managed migrations (placeholder)
internal/platform/   Users, groups, and platform administration (placeholder)
db/migrations/       Schema migrations for this platform (placeholder)
web/static/          Static CSS and other frontend assets
```

The placeholder packages are intentionally empty so the team can decide ownership and interfaces before implementing features.

## Run and test

Requires Go 1.23 or newer and network access the first time to download Templ.

```sh
make run
make test
```

The server listens on `:8080` by default; set `PORT` to change it. `make generate` regenerates Go code from `.templ` files. Generated files are committed so a Go build does not need the Templ CLI separately.
