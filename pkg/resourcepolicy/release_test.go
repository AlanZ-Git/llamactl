package resourcepolicy

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func receiveRelease(t *testing.T, ch <-chan *ReleaseTicket) *ReleaseTicket {
	t.Helper()
	select {
	case ticket := <-ch:
		return ticket
	case <-time.After(3 * time.Second):
		t.Fatal("release not launched")
		return nil
	}
}
func releaseGate(t *testing.T) (*Gate, chan *ReleaseTicket) {
	t.Helper()
	g, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	tickets := make(chan *ReleaseTicket, ReleaseCapacity+4)
	g.SetReleaseRunner(func(ticket *ReleaseTicket) { tickets <- ticket })
	t.Cleanup(func() { s := g.Snapshot(); _, _ = g.Update(true, s.BootID, s.Revision) })
	return g, tickets
}
func pauseRelease(t *testing.T, g *Gate) Snapshot {
	t.Helper()
	s := g.Snapshot()
	next, err := g.Update(false, s.BootID, s.Revision)
	if err != nil {
		t.Fatal(err)
	}
	return next
}

func TestReleaseLedgerCASCapacityAndOriginalOperation(t *testing.T) {
	g, tickets := releaseGate(t)
	s := g.Snapshot()
	if _, err := g.RequestRelease(s.BootID, s.Revision, "not-blocked", "immediate"); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	s = pauseRelease(t, g)
	auto := receiveRelease(t, tickets)
	if auto.Mode() != "graceful" || s.Release != "draining" {
		t.Fatal(s)
	}
	for _, pre := range []struct {
		boot string
		rev  uint64
	}{{"stale", s.Revision}, {s.BootID, s.Revision + 1}} {
		if _, err := g.RequestRelease(pre.boot, pre.rev, "stale", "immediate"); !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	first, err := g.RequestRelease(s.BootID, s.Revision, "same", "immediate")
	if err != nil {
		t.Fatal(err)
	}
	op := receiveRelease(t, tickets)
	if auto.Context().Err() == nil {
		t.Fatal("immediate failed to preempt graceful")
	}
	again, err := g.RequestRelease(s.BootID, s.Revision, "same", "immediate")
	if err != nil || !reflect.DeepEqual(first, again) {
		t.Fatal("repeat changed operation", again, err)
	}
	if _, err := g.RequestRelease(s.BootID, s.Revision, "same", "graceful"); !errors.Is(err, ErrConflict) {
		t.Fatal("different payload", err)
	}
	op.Finish("failed", "termination_failed")
	again, err = g.RequestRelease(s.BootID, s.Revision, "same", "immediate")
	if err != nil || again.Release != "failed" || again.ReleaseOperation.ErrorCode != "termination_failed" {
		t.Fatal(again, err)
	}
	select {
	case <-tickets:
		t.Fatal("repeat launched termination")
	default:
	}
	for n := 1; n < ReleaseCapacity; n++ {
		if _, err := g.RequestRelease(s.BootID, s.Revision, fmt.Sprintf("id-%d", n), "immediate"); err != nil {
			t.Fatal(n, err)
		}
		receiveRelease(t, tickets).Finish("released", "")
	}
	if _, err := g.RequestRelease(s.BootID, s.Revision, "overflow", "immediate"); !errors.Is(err, ErrReleaseCapacity) {
		t.Fatal(err)
	}
	again, err = g.RequestRelease(s.BootID, s.Revision, "same", "immediate")
	if err != nil || again.Release != "failed" {
		t.Fatal("capacity evicted oldest ID", again, err)
	}
	resumed, err := g.Update(true, s.BootID, s.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.RequestRelease(s.BootID, s.Revision, "same", "immediate"); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	s = pauseRelease(t, g)
	receiveRelease(t, tickets)
	if s.Revision != resumed.Revision+1 {
		t.Fatal(s)
	}
	if _, err := g.RequestRelease(s.BootID, s.Revision, "same", "immediate"); !errors.Is(err, ErrConflict) {
		t.Fatal("ID reused in same boot", err)
	}
	if _, err := g.RequestRelease(s.BootID, s.Revision, "new-pause", "immediate"); !errors.Is(err, ErrReleaseCapacity) {
		t.Fatal("capacity reset by resume", err)
	}
}

func TestReleaseEffectLinearizesWithResumeAndNewPause(t *testing.T) {
	g, tickets := releaseGate(t)
	s := pauseRelease(t, g)
	old := receiveRelease(t, tickets)
	entered, finish := make(chan struct{}), make(chan struct{})
	effectDone := make(chan error, 1)
	go func() { effectDone <- old.Effect(func() error { close(entered); <-finish; return nil }) }()
	<-entered
	resumed := make(chan Snapshot, 1)
	go func() { next, _ := g.Update(true, s.BootID, s.Revision); resumed <- next }()
	deadline := time.Now().Add(3 * time.Second)
	for g.Snapshot().Revision == s.Revision {
		if time.Now().After(deadline) {
			t.Fatal("resume not accepted")
		}
		time.Sleep(time.Millisecond)
	}
	if g.Snapshot().AllowAI {
		t.Fatal("resume opened before actual effect boundary")
	}
	// A new pause can latch despite the busy effect and an in-progress resume.
	pending := g.Snapshot()
	pauseDone := make(chan struct{})
	go func() { _, _ = g.Update(false, pending.BootID, pending.Revision); close(pauseDone) }()
	for g.Snapshot().Revision == pending.Revision {
		if time.Now().After(deadline) {
			t.Fatal("pause waited for effect")
		}
		time.Sleep(time.Millisecond)
	}
	close(finish)
	if err := <-effectDone; err != nil {
		t.Fatal(err)
	}
	<-resumed
	<-pauseDone
	next := receiveRelease(t, tickets)
	if g.Snapshot().AllowAI {
		t.Fatal("new pause lost")
	}
	var kills atomic.Int32
	if err := old.Effect(func() error { kills.Add(1); return nil }); err == nil || kills.Load() != 0 {
		t.Fatal("stale release effect executed")
	}
	s = g.Snapshot()
	if _, err := g.Update(true, s.BootID, s.Revision); err != nil {
		t.Fatal(err)
	}
	permit, err := g.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer permit.Close()
	if err := next.Effect(func() error { kills.Add(1); return nil }); err == nil || kills.Load() != 0 {
		t.Fatal("old task killed new generation")
	}
	if err := permit.Spawn(func() error { return nil }); err != nil {
		t.Fatal("new generation blocked", err)
	}
}

func TestReleasePausePersistenceFailureStillLaunchesAndCancelsLoading(t *testing.T) {
	g, tickets := releaseGate(t)
	g.path = "synthetic-no-file-access"
	entered, finish := make(chan struct{}), make(chan struct{})
	g.persist = func(string, []byte) error { close(entered); <-finish; return errors.New("injected persistence error") }
	p, err := g.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx, cleanup := p.BindContext(context.Background())
	defer cleanup()
	done := make(chan error, 1)
	s := g.Snapshot()
	go func() { _, err := g.Update(false, s.BootID, s.Revision); done <- err }()
	<-entered
	auto := receiveRelease(t, tickets)
	if auto.Mode() != "graceful" || ctx.Err() != nil || g.Snapshot().AllowAI {
		t.Fatal("pause must drain, not cancel")
	}
	s = g.Snapshot()
	if _, err := g.RequestRelease(s.BootID, s.Revision, "interrupt", "immediate"); err != nil {
		t.Fatal(err)
	}
	immediate := receiveRelease(t, tickets)
	if err := immediate.CancelWork(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("loading not canceled")
	}
	close(finish)
	if err := <-done; err == nil {
		t.Fatal("persistence failure hidden")
	}
	if s := g.Snapshot(); s.AllowAI || s.Durable {
		t.Fatal(s)
	}
	// Cleanup must not re-enter the injected persistence barrier.
	g.persist = func(string, []byte) error { return nil }
}
