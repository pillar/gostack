// Package migrations embeds the forward-only SQL migrations shipped with GoStack.
//
// Every dialect has the same version numbers: migrations/postgres/NNNN_x.sql is
// the PostgreSQL twin of migrations/sqlite/NNNN_x.sql (and postgres_analytics of
// analytics). A schema-equivalence test keeps them aligned.
package migrations

import (
	"embed"
	"io/fs"
)

// SQLite holds migrations for the main database (directory "sqlite").
//
//go:embed sqlite/*.sql
var SQLite embed.FS

// Analytics holds migrations for the isolated analytics database (directory "analytics").
//
//go:embed analytics/*.sql
var Analytics embed.FS

// Postgres holds PostgreSQL migrations for the main database (directory "postgres").
//
//go:embed postgres/*.sql
var Postgres embed.FS

// PostgresAnalytics holds PostgreSQL migrations for the analytics schema
// (directory "postgres_analytics").
//
//go:embed postgres_analytics/*.sql
var PostgresAnalytics embed.FS

// Main returns the main-database migrations of a dialect ("sqlite" or "postgres")
// as (filesystem, directory).
func Main(dialect string) (fs.FS, string) {
	if dialect == "postgres" {
		return Postgres, "postgres"
	}
	return SQLite, "sqlite"
}

// AnalyticsFor returns the analytics migrations of a dialect as (filesystem, directory).
func AnalyticsFor(dialect string) (fs.FS, string) {
	if dialect == "postgres" {
		return PostgresAnalytics, "postgres_analytics"
	}
	return Analytics, "analytics"
}
