//go:build !linux

package candidate

import "stable/internal/platform/secfile"

// Other platforms do not yet offer the required atomic no-replace metadata
// primitive. Preserve both roots for reconciliation instead of risking a
// concurrently created destination.
func moveMetadataNoReplace(source, destination string) error {
	return secfile.ErrUnsupported
}
