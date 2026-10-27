// Package web embeds the built Vue UI (web/dist, produced by `npm run build`).
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS returns the UI files, or nil when the UI has not been built.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil
	}
	return sub
}
