// Package webui embeds the dashboard's static assets.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed index.html ios-screen.html
var files embed.FS

// FS returns the embedded dashboard file system (rooted at the asset dir).
func FS() fs.FS { return files }

// File returns one embedded asset's bytes.
func File(name string) ([]byte, error) { return files.ReadFile(name) }
