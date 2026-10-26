//go:build windows

package session

import "os"

// lockShared is a no-op on Windows; the proof of concept targets the platforms
// Cake's session lock and the sibling wrappers support.
func lockShared(*os.File) error { return nil }
