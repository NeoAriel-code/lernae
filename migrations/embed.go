// Package migrations provides the versioned SQL files embedded in the Server.
package migrations

import "embed"

// Files contains the versioned schema migrations.
//
//go:embed *.sql
var Files embed.FS
