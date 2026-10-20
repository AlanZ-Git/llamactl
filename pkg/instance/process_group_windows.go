//go:build windows

package instance

import "os/exec"

func setProcAttrs(cmd *exec.Cmd) {
	// Native startup installs a Job and inherited-handle list in owned_tree_windows.go.
}

// signalStop is only the legacy/manual-command fallback. Owned Windows trees
// use their private-console Ctrl-C helper and explicit Job termination.
func signalStop(cmd *exec.Cmd) {
	// No-op on Windows — see setProcAttrs.
}
