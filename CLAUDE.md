# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Backend-only Go service (module `selfmail`) that provisions self-hosted email on top of Stalwart. One binary runs the API and the worker, backed by a single Postgres. SMTP/IMAP/DKIM/spam/quota are delegated to Stalwart. README and PRD (`docs/PRD  Product Requirements Document-2026-11-10.md`, note the double space in the filename) are written in Indonesian.

## Commands

No Makefile, CI, or linter config. Use `gofmt`, `go vet ./...`, `go build ./...`.

- Run locally: `go run ./cmd/selfmail` (migrations run at startup)
- Unit tests: `go test ./...`
- E2E tests (`internal/api/e2e_test.go`, real Postgres + fake Stalwart/Cloudflare/DNS): `TEST_DATABASE_URL=postgres://... go test ./...`. Skipped when the variable is unset.
  - **The database at `TEST_DATABASE_URL` is wiped with `DROP SCHEMA public`.** Only use a dedicated throwaway database.
- Stalwart client against a real server: `STALWART_TEST_URL=http://localhost:8081 STALWART_TEST_USER=... STALWART_TEST_PASS=... go test ./internal/stalwart`
- Create an API token: `selfmail token create <name>` (printed once)

## Setup

Required env: `DATABASE_URL`, `STALWART_URL`, `STALWART_USER`, `STALWART_PASSWORD`, `MAIL_HOST` (see `.env.example` for the rest).

1. `cp .env.example .env`; `export STALWART_RECOVERY_ADMIN=admin:...`
2. `docker compose up -d postgres stalwart`
3. Run `scripts/bootstrap-stalwart.sh` (needs `curl`, `python3`; inputs `STALWART_URL`, `STALWART_RECOVERY_ADMIN`, `MAIL_HOST`, `DEFAULT_DOMAIN`, optional `TLS=true`). Put the printed `STALWART_USER`/`STALWART_PASSWORD` in `.env`, then `docker compose restart stalwart`.
4. `docker compose up -d --build selfmail`

## Gotchas

- Stalwart generates the DKIM selector (e.g. `v1-rsa-20261110`); the app reads it from Stalwart, so `DKIM_SELECTOR` is not authoritative.
- Stalwart does not reject over-quota mail at SMTP time (replies `250`, fails later with `451`). This differs from PRD FR-05d.
- DNS verification fails closed: `verified` requires MX, SPF, DKIM and DMARC all `ok`. A mismatch is never overwritten.
- If the usage cache is stale and Stalwart is unreachable, the API returns 503, not 0.
- Passwords are returned once and never stored or logged.
- `.gitignore` excludes `.claude/` and `graphify-out/`, so project-level Claude config is untracked.
