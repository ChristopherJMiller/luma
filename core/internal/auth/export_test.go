package auth

import "time"

// Tests reload the registry quickly instead of after the production debounce.
func init() { registryDebounce = 20 * time.Millisecond }

// NewBFFWithClock is NewBFF on a test clock. The clock is set before the
// background discovery starts, so nothing races on it.
var NewBFFWithClock = newBFF

// DiscoveryRetry is how long a failed discovery is remembered.
const DiscoveryRetry = discoveryRetry

// WatchRegistryReloads is WatchRegistry reporting every reload's outcome, the
// failed ones included.
var WatchRegistryReloads = watchRegistry
