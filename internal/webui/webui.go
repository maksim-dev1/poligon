// Package webui embeds the dashboard's static assets.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:index.html
var files embed.FS

// FS returns the embedded dashboard file system (rooted at the asset dir).
func FS() fs.FS { return files }
