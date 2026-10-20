package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"llamactl/pkg/instance"
	"strings"
	"testing"
	"time"
)

func TestP3FailedStopCannotBeClearedByNoopUnload(t *testing.T) {
	im, inst, _, _ := p3Manager(t)
	ready := p3Submit(t, im, inst, "apply", "apply", "alpha", `{}`)
	if ready.State != "succeeded" {
		t.Fatal(ready)
	}
	im.control.stop = func(context.Context, *instance.Maintenance, *instance.Instance) error {
		return fmt.Errorf("synthetic stop failure")
	}
	failed := p3Submit(t, im, inst, "stop-failed", "stop", "", "")
	if failed.State != "failed" {
		t.Fatal(failed)
	}
	p3Submit(t, im, inst, "cleanup", "unload", "", "")
	if _, err := im.SubmitControl("p3", "scope", p3Payload(t, inst, "unsafe-load", "load", "beta", "")); p3Code(err) != "recovery_required" {
		t.Fatalf("unload erased stop failure: %v", err)
	}
}

func TestP3CallerDeadlineOnlyShortens(t *testing.T) {
	im, inst, backend, _ := p3Manager(t)
	if im.control.budget != 600*time.Second {
		t.Fatal("default budget changed")
	}
	base := p3Payload(t, inst, "deadline", "stop", "", "")
	for _, value := range []string{"0", "null", "false", "-1", "1.5", "601"} {
		body := strings.TrimSuffix(string(base), "}") + `,"timeout_seconds":` + value + `}`
		if _, err := im.SubmitControl("p3", "scope", []byte(body)); err == nil {
			t.Fatalf("accepted timeout %s", value)
		}
	}
	backend.holdLoad = true
	body := p3Payload(t, inst, "short", "apply", "alpha", `{}`)
	body = []byte(strings.TrimSuffix(string(body), "}") + `,"timeout_seconds":1}`)
	op, err := im.SubmitControl("p3", "scope", body)
	if err != nil {
		t.Fatal(err)
	}
	if op.Deadline.Sub(op.StartedAt) != time.Second {
		t.Fatal("caller deadline not applied")
	}
	result := p3Wait(t, im, op.ID)
	if result.State != "deadline_exceeded" || !inst.ControlSnapshot().RecoveryRequired {
		t.Fatal(result)
	}
}

func TestP3CanonicalPresenceAndZeroValues(t *testing.T) {
	prefix := `{"action":"apply","boot_id":"boot","expected_revision":0,"idempotency_key":"one","model":"alpha","patch":`
	seen := map[string]bool{}
	for _, patch := range []string{`{}`, `{"threads":null}`, `{"threads":0}`, `{"flash_attention":false}`} {
		req, err := parseControlRequest([]byte(prefix + patch + `}`))
		if err != nil {
			t.Fatal(err)
		}
		if seen[req.canonical] {
			t.Fatalf("presence collapsed: %s", patch)
		}
		seen[req.canonical] = true
	}
	a, err := parseControlRequest([]byte(prefix + `{"threads":0,"flash_attention":false}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := parseControlRequest([]byte(prefix + `{"flash_attention":false,"threads":0.0e2}}`))
	if err != nil {
		t.Fatal(err)
	}
	if a.canonical != b.canonical {
		t.Fatal("equivalent normalized integers/order differ")
	}
}

func TestP3LoadOtherModelDoesNotMutateConfiguration(t *testing.T) {
	im, inst, _, store := p3Manager(t)
	first := p3Submit(t, im, inst, "apply", "apply", "alpha", `{}`)
	if first.State != "succeeded" {
		t.Fatal(first)
	}
	before, _ := json.Marshal(inst.GetOptions())
	store.mu.Lock()
	saves := len(store.saved)
	store.mu.Unlock()
	if op := p3Submit(t, im, inst, "unload", "unload", "", ""); op.State != "succeeded" {
		t.Fatal(op)
	}
	second := p3Submit(t, im, inst, "load-beta", "load", "beta", "")
	if second.State != "succeeded" {
		t.Fatal(second)
	}
	after, _ := json.Marshal(inst.GetOptions())
	store.mu.Lock()
	defer store.mu.Unlock()
	if string(before) != string(after) || len(store.saved) != saves {
		t.Fatal("load silently mutated durable configuration")
	}
	s := inst.ControlSnapshot()
	if s.Target != "alpha" || s.ActualModel != "beta" || s.Revision != 1 || second.ReadyToken == first.ReadyToken {
		t.Fatal(s)
	}
}
