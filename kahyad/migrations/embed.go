// Package migrations embeds the goose SQL migration files into the kahyad
// binary so migrations run from the compiled artifact without needing the
// source tree on disk at runtime (HANDOFF §4 stack ⚑: goose runs at kahyad
// startup before any other work).
package migrations

import "embed"

// FS holds every goose migration file (NNNN_*.sql) under this directory.
//
//go:embed *.sql
var FS embed.FS
