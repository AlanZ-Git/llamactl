package resourcepolicy

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPendingSignalBlocksResumeBeforePersistence(t *testing.T) {
	g, tickets := releaseGate(t)
	s := pauseRelease(t, g)
	ticket := receiveRelease(t, tickets)
	done := make(chan struct{})
	defer close(done)
	if err := ticket.Effect(func() error { g.TrackOSSignal(done); return nil }); err != nil {
		t.Fatal(err)
	}
	var writes atomic.Int32
	g.path = "synthetic-no-file-access"
	g.persist = func(string, []byte) error { writes.Add(1); return nil }
	before := time.Now()
	s, err := g.Update(true, s.BootID, s.Revision)
	if !errors.Is(err, ErrEffectPending) || s.AllowAI || s.Durable || writes.Load() != 0 {
		t.Fatalf("uncertain effect persisted/opened resume: %+v err=%v writes=%d", s, err, writes.Load())
	}
	if time.Since(before) > time.Second {
		t.Fatal("pending signal blocked the resume caller instead of failing closed")
	}
	// A new pause and immediate effect must work while old dispatch is uncertain.
	s, err = g.Update(false, s.BootID, s.Revision)
	if err != nil {
		t.Fatal(err)
	}
	receiveRelease(t, tickets)
	if _, err := g.RequestRelease(s.BootID, s.Revision, "pending-immediate", "immediate"); err != nil {
		t.Fatal(err)
	}
	immediate := receiveRelease(t, tickets)
	if err := immediate.Effect(func() error { return nil }); err != nil {
		t.Fatal("pending guard blocked explicit immediate", err)
	}
}

func TestPendingSignalPreservesBlockedFile(t *testing.T) {
	dir := t.TempDir()
	g, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	tickets := make(chan *ReleaseTicket, 1)
	g.SetReleaseRunner(func(ticket *ReleaseTicket) { tickets <- ticket })
	s := pauseRelease(t, g)
	ticket := receiveRelease(t, tickets)
	path := filepath.Join(dir, "resource-policy.json")
	blocked, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var once sync.Once
	complete := func() { once.Do(func() { close(done) }) }
	defer complete()
	if err := ticket.Effect(func() error { g.TrackOSSignal(done); return nil }); err != nil {
		t.Fatal(err)
	}
	s, err = g.Update(true, s.BootID, s.Revision)
	if !errors.Is(err, ErrEffectPending) || s.AllowAI || s.Durable {
		t.Fatal(s, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(blocked, after) {
		t.Fatal("failed resume changed the durable blocked file", err)
	}
	complete()
	s, err = g.Update(true, s.BootID, s.Revision)
	if err != nil || !s.AllowAI || !s.Durable {
		t.Fatal(s, err)
	}
	reopened, err := Open(dir)
	if err != nil || !reopened.Snapshot().AllowAI || reopened.Snapshot().Revision != s.Revision {
		t.Fatal("fresh confirmed resume was not persisted", err)
	}
}

func TestPendingSignalRegisteredInsideInFlightEffect(t *testing.T) {
	g, tickets := releaseGate(t)
	s := pauseRelease(t, g)
	ticket := receiveRelease(t, tickets)
	entered, register := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	defer close(done)
	var once sync.Once
	unblock := func() { once.Do(func() { close(register) }) }
	defer unblock()
	var writes atomic.Int32
	g.path = "synthetic-no-file-access"
	g.persist = func(string, []byte) error { writes.Add(1); return nil }
	effectDone := make(chan error, 1)
	go func() {
		effectDone <- ticket.Effect(func() error {
			close(entered)
			<-register
			g.TrackOSSignal(done)
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("effect did not enter")
	}
	resumeDone := make(chan error, 1)
	go func() { _, err := g.Update(true, s.BootID, s.Revision); resumeDone <- err }()
	deadline := time.Now().Add(3 * time.Second)
	for g.Snapshot().Revision == s.Revision {
		if time.Now().After(deadline) {
			t.Fatal("resume did not latch")
		}
		time.Sleep(time.Millisecond)
	}
	if writes.Load() != 0 {
		t.Error("resume wrote allowed before joining the in-flight effect")
	}
	unblock()
	select {
	case err := <-effectDone:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("effect did not return")
	}
	select {
	case err := <-resumeDone:
		if !errors.Is(err, ErrEffectPending) {
			t.Error(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("resume did not fail closed within its boundary")
	}
	if writes.Load() != 0 || g.Snapshot().AllowAI {
		t.Fatal("late guard registration escaped pre-persistence validation")
	}
}

func TestPendingSignalCompletionRequiresFreshResume(t *testing.T) {
	for _, completion := range []string{"canceled-before-claim", "syscall-returned", "dispatcher-exited"} {
		t.Run(completion, func(t *testing.T) {
			g, tickets := releaseGate(t)
			s := pauseRelease(t, g)
			ticket := receiveRelease(t, tickets)
			done := make(chan struct{})
			if err := ticket.Effect(func() error { g.TrackOSSignal(done); return nil }); err != nil {
				t.Fatal(err)
			}
			s, err := g.Update(true, s.BootID, s.Revision)
			if !errors.Is(err, ErrEffectPending) {
				t.Fatal(err)
			}
			close(done) // The OS-specific implementation supplies this evidence.
			if g.Snapshot().AllowAI {
				t.Fatal("evidence completion automatically resumed admission")
			}
			if err := ticket.Effect(func() error { t.Error("stale dispatch ran"); return nil }); err == nil {
				t.Fatal("old ticket became valid after guard completion")
			}
			s, err = g.Update(true, s.BootID, s.Revision)
			if err != nil || !s.AllowAI || s.Release != "not_requested" {
				t.Fatal(s, err)
			}
			g.mu.Lock()
			remaining := len(g.pendingEffects)
			g.mu.Unlock()
			if remaining != 0 {
				t.Fatal("completed signal guard leaked")
			}
		})
	}
}

func TestPendingSignalRegistrationDuringResumePersistence(t *testing.T) {
	g, tickets := releaseGate(t)
	s := pauseRelease(t, g)
	receiveRelease(t, tickets)
	entered, finish := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(finish) }) }
	t.Cleanup(unblock)
	var writes atomic.Int32
	g.path = "synthetic-no-file-access"
	g.persist = func(string, []byte) error {
		if writes.Add(1) == 1 {
			close(entered)
			<-finish
		}
		return nil
	}
	resumed := make(chan error, 1)
	go func() { _, err := g.Update(true, s.BootID, s.Revision); resumed <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("resume did not reach persistence")
	}
	// Ordinary Stop must not add an uncertain signal in the gap between the
	// pre-persistence guard check and reopening. It returns, without dispatch.
	if err := g.StopEffect(func() error { t.Error("Stop dispatched during resume persistence"); return nil }); !errors.Is(err, ErrPaused) {
		t.Error(err)
	}
	// A new owner pause still latches without waiting for that file write, and
	// its authorized immediate syscall does not wait for persistence either.
	pending := g.Snapshot()
	pauseDone := make(chan error, 1)
	go func() { _, err := g.Update(false, pending.BootID, pending.Revision); pauseDone <- err }()
	newPause := receiveRelease(t, tickets)
	guard := make(chan struct{})
	if err := newPause.Effect(func() error { g.TrackOSSignal(guard); return nil }); err != nil {
		t.Error(err)
	}
	defer close(guard)
	unblock()
	select {
	case err := <-resumed:
		if !errors.Is(err, ErrConflict) {
			t.Error("old resume ignored newer pause", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("old resume did not settle")
	}
	select {
	case err := <-pauseDone:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("new pause did not settle")
	}
	if g.Snapshot().AllowAI {
		t.Fatal("new pause or unresolved effect lost to old resume")
	}
}
