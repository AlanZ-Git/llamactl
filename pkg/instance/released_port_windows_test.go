//go:build windows

package instance

import (
	"context"
	"net"
	"testing"
	"time"
)

// Native-only diagnosis: establish an accepted connection and server-side active
// close, prove TIME_WAIT in the OS table, then release an actual owned Job tree.
func TestWindowsOwnerReleaseWithAcceptedTimeWait(t *testing.T) {
	inst, g, dir := treeFixture(t, "stubborn")
	addr := treeAddress(t, dir, "root")
	acceptedTreeConnection(t, addr)
	port := inst.GetPort()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		table, err := windowsTCPTable(ctx, false)
		if err != nil {
			t.Fatal(err)
		}
		found, err := tcpTablePortHasState(table, false, port, tcpStateTimeWait)
		if err != nil {
			t.Fatal(err)
		}
		if found {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("accepted connection did not establish TIME_WAIT evidence")
		}
		time.Sleep(10 * time.Millisecond)
	}
	ticket := releaseTicket(t, g, "immediate")
	state, code := inst.ReleaseOwned(ticket)
	ticket.Finish(state, code)
	waitTreeGone(t, inst.process.tree)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	table, err := windowsTCPTable(ctx2, false)
	if err != nil {
		t.Fatal(err)
	}
	listening, err := tcpTablePortHasState(table, false, port, tcpStateListen)
	if err != nil || listening {
		t.Fatalf("listener after Job exit: %v %v", listening, err)
	}
	timewait, err := tcpTablePortHasState(table, false, port, tcpStateTimeWait)
	if err != nil || !timewait {
		t.Fatalf("missing post-exit TIME_WAIT evidence: %v %v", timewait, err)
	}
	// Diagnostic only: bind behavior is not the release oracle anymore. Preserve
	// whether this native platform actually reproduces the suspected bind error.
	probe, bindErr := net.Listen("tcp4", addr)
	if probe != nil {
		_ = probe.Close()
	}
	t.Logf("owned tree exited, TCP LISTEN=false, TIME_WAIT=true, diagnostic rebind error=%v", bindErr)
	if state != "released" {
		t.Fatalf("actual exit + no listener was not released: %s/%s", state, code)
	}
}

func TestWindowsReleasedPortRejectsIPv6ForeignListener(t *testing.T) {
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	defer ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Conservative across addresses/families: an IPv6 foreign listener may not be
	// declared released by checking only the configured IPv4 endpoint.
	if err := releasedPort(ctx, "127.0.0.1", ln.Addr().(*net.TCPAddr).Port); err == nil {
		t.Fatal("foreign IPv6 listener ignored")
	}
}
