// Package db owns brain.db schema migrations and the sqlc-generated query layer.
//
// Placeholder: W12-02 replaces this with the real goose migration runner that
// executes at kahyad startup before any work (HANDOFF §4 stack).
package db

import "github.com/pressly/goose/v3"

// Keep the pinned goose dependency referenced until W12-02 lands the real
// run-at-startup migration runner.
var _ = goose.SetBaseFS //nolint:unused
