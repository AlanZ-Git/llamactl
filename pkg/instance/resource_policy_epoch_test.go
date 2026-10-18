package instance

import (
	"context"
	"errors"
	"llamactl/pkg/resourcepolicy"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestOwnerPolicyDelayedRestartEpoch(t *testing.T) {
	cfg, opts := policyFixture(t, "isolated")
	g, err := resourcepolicy.Open("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.ResourcePolicy = g
	inst, err := NewChecked("delayed", cfg, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := inst.BeginWorkMaintenance()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	timer := make(chan time.Time)
	done := make(chan struct{})
	go func() { defer close(done); inst.process.waitAutomaticRestart(context.Background(), timer, lease) }()
	s := g.Snapshot()
	s, err = g.Update(false, s.BootID, s.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Update(true, s.BootID, s.Revision); err != nil {
		t.Fatal(err)
	}
	close(timer)
	<-done
	if inst.process.cmd != nil {
		t.Fatal("old scheduled restart reached command construction")
	}
}

func TestOwnerPolicyPauseWhileProcessLockHeld(t *testing.T) {
	cfg, opts := policyFixture(t, "isolated")
	g, err := resourcepolicy.Open("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.ResourcePolicy = g
	inst, err := NewChecked("held", cfg, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	inst.process.mu.Lock()
	result := make(chan error, 1)
	go func() { result <- inst.Start() }()
	// Wait for the start reservation before pausing; no process/gate lock is shared.
	for g.Active() == 0 {
		runtime.Gosched()
	}
	s := g.Snapshot()
	_, err = g.Update(false, s.BootID, s.Revision)
	inst.process.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, resourcepolicy.ErrPaused) {
		t.Fatalf("late process start: %v", err)
	}
	if inst.process.cmd != nil {
		t.Fatal("paused start constructed command")
	}
}

func TestOwnerPolicyOldProxyRequestCannotRevive(t *testing.T) {
	cfg, opts := policyFixture(t, "isolated")
	g, err := resourcepolicy.Open("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.ResourcePolicy = g
	inst, err := NewChecked("old-request", cfg, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/completion", strings.NewReader(`{}`))
	release, err := inst.PrepareInferenceRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	s := g.Snapshot()
	s, err = g.Update(false, s.BootID, s.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Update(true, s.BootID, s.Revision); err != nil {
		t.Fatal(err)
	}
	if err := inst.CheckRequestPolicy(r); !errors.Is(err, resourcepolicy.ErrPaused) {
		t.Fatalf("old request admission: %v", err)
	}
	if err := inst.ServeHTTP(httptest.NewRecorder(), r); !errors.Is(err, resourcepolicy.ErrPaused) {
		t.Fatalf("old request forwarded: %v", err)
	}
}
