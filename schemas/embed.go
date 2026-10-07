// Package schemas embeds Foundation schemas for offline runtime validation.
package schemas

import "embed"

// Files contains the checked-in JSON schemas.
//
//go:embed inventory-manifest.schema.json
var Files embed.FS
