package capacity

import "embed"

// Migrations are this module's SQL files, applied by the migrate step in booking.
//
//go:embed migrations/*.sql
var Migrations embed.FS
