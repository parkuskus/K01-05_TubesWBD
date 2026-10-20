# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

Platform administrators and group members use accounts to access the database management platform. The initial account is a platform administrator; public registration is not part of the current login scope.

## Product Purpose

The platform helps a team manage access to shared PostgreSQL databases, inspect database structure and data, and review or apply database migrations.

## Positioning

No distinct market positioning has been established for this course project.

## Operating Context

The application is a server-rendered Go web app using Templ and PostgreSQL. Administrators manage users, groups, database connections, and group access; members use database access granted through their groups.

## Capabilities and Constraints

- The Milestone 1 target DBMS is PostgreSQL.
- Users authenticate with username and password; passwords are stored as hashes.
- Session data is stored server-side in PostgreSQL; browser sessions use secure cookies and CSRF protection.
- Database access is group-based. Platform-admin status does not grant automatic access to every target database.
- The first administrator is provisioned during initial setup; public account registration is not in scope.
- Database viewer, authorization repositories, and migration features remain future work.

## Evidence on Hand

- `extra/IF3110 - Milestone 1 - Pengembangan Aplikasi Web.md` is the project requirements source.
- The repository contains a basic server-rendered home page and stylesheet; no logo, customer evidence, or approved brand system is present.

## Product Principles

- Enforce authentication and authorization in the backend.
- Keep platform account/session storage separate from user-managed target databases.
- Let users review database changes and preserve a reliable migration history.

## Accessibility & Inclusion

Use semantic HTML, visible labels, keyboard-operable forms, and clear error states.
