.PHONY: migrate-up migrate-down

# Load .env if present (optional)
-include .env
export

# This service runs on Turso / libSQL, not Postgres. Set DATABASE_URL to a libsql
# URL (libsql://<db>.turso.io?authToken=...) or set TURSO_DATABASE_URL +
# TURSO_AUTH_TOKEN in .env. There is deliberately no default: a wrong one produces
# a confusing driver error instead of an obvious "not configured" message.

migrate-up:
	go run ./cmd/migrate -up

migrate-down:
	go run ./cmd/migrate -down
