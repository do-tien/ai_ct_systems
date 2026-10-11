// Package migrations embeds the LC-1 goose SQL migrations so the migration
// command can run them from the compiled binary without reading the repository
// filesystem at runtime.
package migrations

import "embed"

// Files holds the embedded goose SQL migrations. It is read-only bookkeeping:
// the application never writes to it.
//
//go:embed *.sql
var Files embed.FS
