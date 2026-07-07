// Package db owns brain.db schema migrations (goose) and, from W12-02 on, the
// sqlc-generated query layer. Migrations run at kahyad startup before any work.
package db

import "github.com/pressly/goose/v3"

// Placeholder reference so the pinned goose dependency survives `go mod tidy`
// until W12-02 lands the real run-at-startup migration runner.
//
//nolint:unused
var _ = goose.SetBaseFS
