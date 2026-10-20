//go:build windows

package instance

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Native Windows regression: failure to install the creation-time Job attribute
// must fail before any backend code can run. There is no unowned Start fallback.
func TestOwnerWindowsJobAssociationFailure(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestOwnerTreeHelper$", "--", "--owner-tree-fixture", "root", "stubborn", dir, "0")
	tree, err := startOwnedTreeAttribute(cmd, ^uintptr(0))
	if err == nil || tree != nil {
		t.Fatal("invalid Job association accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "root.addr")); !os.IsNotExist(err) {
		t.Fatal("backend ran without Job ownership", err)
	}
}
