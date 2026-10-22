package auth

import "time"

// Tests reload the registry quickly instead of after the production debounce.
func init() { registryDebounce = 20 * time.Millisecond }

// SetClock replaces the BFF's clock. Call it before the first request.
func SetClock(b *BFF, now func() time.Time) { b.now = now }

// WatchRegistryReloads is WatchRegistry reporting every reload's outcome, the
// failed ones included.
var WatchRegistryReloads = watchRegistry
