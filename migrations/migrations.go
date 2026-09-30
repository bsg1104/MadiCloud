// Package migrations embeds the control-plane schema migrations.
//
// Files are named NNNN_lower_snake.sql and applied in version order by
// madicloudd migrate. An applied file must never be edited; add a new one.
package migrations

import (
	"embed"
	"io/fs"
)

//go:embed *.sql
var files embed.FS

// FS returns the embedded migration files.
func FS() fs.FS { return files }
