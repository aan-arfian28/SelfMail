// Package migrations embeds SQL migrations.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
