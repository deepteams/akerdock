// Package db embeds the versioned SQL migrations applied at startup
// (ADR-025, instance-config §6.1 step 3).
package db

import "embed"

// Migrations holds the goose migration files, applied in order at boot.
//
//go:embed migrations/*.sql
var Migrations embed.FS
