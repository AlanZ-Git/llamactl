package instance

import (
	"io"
	"os"
	"testing"
)

// The child exits before the Go test flag parser sees native engine argv. It is
// a synthetic executable, not a shell or model; only this test sets its marker.
func init() {
	if os.Getenv("LLAMACTL_CONTROLLED_TEST_CHILD") == "stdin-exit" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
}

func TestControlledSyntheticLifecycle(t *testing.T) {
	cfg, opts := controlledFixture(t)
	cfg.Backends.LlamaCpp.Command = os.Args[0]
	opts.BackendOptions.SetPort(0)
	opts.Environment = map[string]string{"LLAMACTL_CONTROLLED_TEST_CHILD": "stdin-exit"}
	inst, err := NewChecked("synthetic-lifecycle", cfg, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := inst.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if inst.IsRunning() {
			_ = inst.Stop()
		}
	}()
	if !inst.IsRunning() {
		t.Fatal("synthetic child not running")
	}
	if inst.process.shouldAutoRestart() {
		t.Fatal("controlled auto-restart enabled")
	}
	if err := inst.Restart(); err != nil {
		t.Fatal(err)
	}
	if !inst.IsRunning() {
		t.Fatal("restart did not start synthetic child")
	}
	if err := inst.Stop(); err != nil {
		t.Fatal(err)
	}
	if inst.GetStatus() != Stopped {
		t.Fatal("synthetic child not stopped")
	}
	inst.process.mu.Lock()
	monitor := inst.process.monitorDone
	inst.process.mu.Unlock()
	if monitor != nil {
		t.Fatal("stop returned before generation cleanup")
	}
	candidate, _ := inst.GetOptions().Clone()
	candidate.BackendOptions.LlamaServerOptions.ModelsMax = 2
	if err := inst.SetOptions(candidate); err == nil {
		t.Fatal("invalid direct controlled options committed")
	}
	lease, err := inst.BeginMaintenance()
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Retire(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Start(); err == nil {
		t.Fatal("retired lease can start")
	}
	lease.Close()
}
