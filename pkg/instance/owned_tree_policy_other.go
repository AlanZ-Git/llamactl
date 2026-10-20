//go:build !windows

package instance

import (
	"context"
	"llamactl/pkg/resourcepolicy"
)

// These platforms issue their shutdown syscall in the manager, not a helper.
func (t *ownedTree) bindReleasePolicy(_ *resourcepolicy.Gate) {}
func (t *ownedTree) stopSignal() error                        { return t.signal(false) }
func (t *ownedTree) signalRelease(_ context.Context, force bool, send func(*ownedTree, bool) error) error {
	return send(t, force)
}
