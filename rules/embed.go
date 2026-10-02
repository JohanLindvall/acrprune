// Package presets provides the example rules shipped with crprune.
package presets

import "embed"

// Files contains the bundled JSON rule files, available without a checkout.
//
//go:embed *.json
var Files embed.FS
