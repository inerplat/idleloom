package recipes

import "embed"

// FS contains immutable, versioned recipe definitions and manifest templates.
//
//go:embed native worker
var FS embed.FS
