// Package migrations embeds the forward-only SQL migrations shipped with GoStack.
package migrations

import "embed"

// SQLite holds migrations for the main database (directory "sqlite").
//
//go:embed sqlite/*.sql
var SQLite embed.FS

// Analytics holds migrations for the isolated analytics database (directory "analytics").
//
//go:embed analytics/*.sql
var Analytics embed.FS
