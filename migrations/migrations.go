// Package migrations embeds the SQL migration files so the binary can apply
// them on boot without shipping the .sql files alongside it.
package migrations

import "embed"

//go:embed *.up.sql
var FS embed.FS
