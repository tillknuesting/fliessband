// Package web embeds the factory's browser UI.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var files embed.FS

// Static is the built UI (see web/ui; `make ui` rebuilds it).
var Static, _ = fs.Sub(files, "static")
