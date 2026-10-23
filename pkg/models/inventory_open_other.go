//go:build !linux && !windows

package models

import (
	"fmt"
	"os"
)

// Platforms without a safe-open implementation remain fail-closed. Linux and
// Windows have separate implementations; Windows execution-window handle
// compatibility still requires target-runtime evidence.
func safeInventoryOpen(root, relative string) (*os.File, error) {
	return nil, fmt.Errorf("inventory_safe_open_unsupported_platform")
}
func inventoryFileID(_ *os.File, info os.FileInfo) string { return "unsupported" }
