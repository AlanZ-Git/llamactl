package instance

import (
	"context"
	"fmt"
	"llamactl/pkg/resourcepolicy"
	"log"
	"net"
	"time"
)

// ReleaseOwned bypasses ordinary maintenance contention, never process ownership.
// It captures one generation and signals it synchronously through the ticket's
// revocable side-effect boundary. All waits occur outside that boundary.
func (i *Instance) ReleaseOwned(ticket *resourcepolicy.ReleaseTicket) (string, string) {
	if i.process == nil || i.IsRemote() {
		return "released", ""
	}
	p := i.process
	ctx := ticket.Context()
	for !p.mu.TryRLock() {
		select {
		case <-ctx.Done():
			return "unreleased_unknown", "release_timeout"
		case <-time.After(5 * time.Millisecond):
		}
	}
	tree, stdin, cancel, done := p.tree, p.stdin, p.cancel, p.monitorDone
	treeHost, treePort, treeDocker := p.treeHost, p.treePort, p.treeDocker
	signalTree := p.signalTree
	opts := i.GetOptions()
	p.mu.RUnlock()
	externalRuntime := treeDocker || (opts.DockerEnabled != nil && *opts.DockerEnabled)
	if tree != nil {
		if !tree.supportsRelease() {
			return "unreleased_unknown", "tree_ownership_unsupported"
		}
		err := ticket.Effect(func() error {
			if ticket.Mode() == "immediate" && cancel != nil {
				cancel()
			}
			if stdin != nil {
				_ = stdin.Close()
			}
			return tree.signalRelease(ctx, ticket.Mode() == "immediate", signalTree)
		})
		if err != nil {
			if ctx.Err() != nil {
				return "unreleased_unknown", "release_timeout"
			}
			return "failed", "termination_failed"
		}
		select {
		case <-tree.done:
		case <-ctx.Done():
			return "unreleased_unknown", "release_timeout"
		}
		if done != nil {
			select {
			case <-done:
			case <-ctx.Done():
				return "unreleased_unknown", "release_timeout"
			}
		}
	}
	// The local launcher tree is still ours to stop, but it cannot certify the
	// resources of an external container/runtime daemon.
	if externalRuntime {
		return "unreleased_unknown", "external_runtime"
	}
	if tree != nil {
		if err := releasedPort(ctx, treeHost, treePort); err != nil {
			log.Printf("Instance %q owner release captured-port evidence failed (captured_host=%q captured_port=%d configured_host=%q configured_port=%d): %v", i.Name, treeHost, treePort, opts.BackendOptions.GetHost(), opts.BackendOptions.GetPort(), err)
			return "unreleased_unknown", "port_ownership_unknown"
		}
	}
	if err := releasedPort(ctx, opts.BackendOptions.GetHost(), opts.BackendOptions.GetPort()); err != nil {
		log.Printf("Instance %q owner release configured-port evidence failed (captured_host=%q captured_port=%d configured_host=%q configured_port=%d): %v", i.Name, treeHost, treePort, opts.BackendOptions.GetHost(), opts.BackendOptions.GetPort(), err)
		return "unreleased_unknown", "port_ownership_unknown"
	}
	return "released", ""
}

// Verify port evidence only after owned tree exit. Windows distinguishes TCP
// LISTEN state from closed connections in TIME_WAIT; other platforms retain the
// existing bind check. Neither path targets a listener's PID.
func releasedPort(ctx context.Context, host string, port int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if port <= 0 {
		return nil
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}
	if host != "" && net.ParseIP(host) == nil {
		return fmt.Errorf("nonliteral bind address")
	}
	if port > 65535 {
		return fmt.Errorf("invalid TCP port")
	}
	return releasedPortState(ctx, host, port)
}
