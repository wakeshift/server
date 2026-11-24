// Package web embeds the built React app (dist/) so the server is a single
// binary. dist/ holds only a placeholder until `make web` builds it.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist is the built app's files.
var Dist, _ = fs.Sub(dist, "dist")
