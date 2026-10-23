package server

import (
	"context"
	"encoding/json"
	"fmt"
	"llamactl/pkg/instance"
	"llamactl/pkg/manager"
	"testing"
)

func TestN1AcceptRetryAfterRegistrationAndParameterChanges(t *testing.T) {
	f := newN1Fixture(t)
	req, a := n1PreparedEvaluation(t, f)
	run := n1Call(t, f, "POST", "evaluation-runs", req, 202)
	id := run["run_id"].(string)
	registration := n1Envelope(f, "busy-register")
	registration["artifact_id"] = a["artifact_id"]
	n1Call(t, f, "POST", "inventory/register", registration, 409)
	n1Call(t, f, "POST", "evaluation-runs/"+id+"/cancel", n1Envelope(f, "cancel-before-change"), 200)
	n1Wait(t, f, "evaluation-runs/"+id, func(v map[string]any) bool { return v["frozen_at"] != nil })
	opts, err := f.instance.GetOptions().Clone()
	if err != nil {
		t.Fatal(err)
	}
	// A subsequent valid registration occupies the last slot. The old accepted
	// candidate must no longer be revalidated against this unrelated current state.
	original := opts.Control.Models["alpha"]
	opts.Control.Models = map[string]instance.ControlModel{}
	for k := 0; k < 64; k++ {
		opts.Control.Models[fmt.Sprintf("model%d", k)] = original
	}
	opts.ConfigRevision++
	opts.PresetIni = nil
	s := f.instance.ControlSnapshot()
	m, err := f.instance.BeginControl(s.BootID, s.Revision, true, false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := m.PrepareOptions(opts)
	if err != nil {
		m.Close()
		t.Fatal(err)
	}
	err = p.PersistControl(context.Background(), func(*instance.Instance) error { return nil })
	p.Close()
	m.Close()
	if err != nil {
		t.Fatal(err)
	}
	before := f.instance.ControlSnapshot()
	same := n1Call(t, f, "POST", "evaluation-runs", req, 202)
	if same["run_id"] != id {
		t.Fatal("lost acceptance replayed", same)
	}
	events, err := f.n1Store.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Type == "dispatch" {
			t.Fatal("retry caused dispatch")
		}
	}
	if f.instance.ControlSnapshot().Revision != before.Revision {
		t.Fatal("retry changed revision")
	}
}

func TestN1AcceptLookupFailureDoesNotBecomeMissing(t *testing.T) {
	f := newN1Fixture(t)
	req, _ := n1PreparedEvaluation(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	body, _ := json.Marshal(req)
	_, _, err := f.manager.(manager.N1Manager).N1Request(ctx, "router", "test", false, "POST", "evaluation-runs", nil, body)
	if err == nil {
		t.Fatal("cancelled storage lookup accepted")
	}
	if f.instance.IsRunning() {
		t.Fatal("lookup failure started process")
	}
}
