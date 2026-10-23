package manager

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"llamactl/pkg/database"
	"llamactl/pkg/instance"
	"llamactl/pkg/resourcepolicy"
	"testing"
	"time"
)

func n1WorkerFixture(t *testing.T) (*instanceManager, *n1Coordinator, *n1Live) {
	t.Helper()
	im, n, l, _ := n1WorkerRunFixture(t, "switch", time.Minute)
	return im, n, l
}
func n1WorkerRunFixture(t *testing.T, kind string, lifetime time.Duration) (*instanceManager, *n1Coordinator, *n1Live, *sql.DB) {
	t.Helper()
	im, inst, _, _ := p3Manager(t)
	if r := p3Submit(t, im, inst, "worker-ready", "apply", "alpha", `{}`); r.State != "succeeded" {
		t.Fatal(r)
	}
	db, err := database.Open(&database.Config{Path: ":memory:", MaxOpenConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err = database.RunMigrations(db); err != nil {
		t.Fatal(err)
	}
	s := inst.ControlSnapshot()
	deadline := time.Now().Add(lifetime)
	reservationKind := "service_use"
	if kind == "evaluation" {
		reservationKind = kind
	}
	r, err := inst.ReserveN1("worker", "scope", reservationKind, s.BootID, s.Revision, deadline)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.CloseConfirmed() })
	run, _, err := db.N1Store().Accept(context.Background(), database.ExecutionRun{ID: "worker", Kind: kind, Scope: "scope", Instance: inst.Name, Boot: s.BootID, Key: "worker", Payload: json.RawMessage(`{}`), Manifest: json.RawMessage(`{}`), Deadline: deadline.UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	n := &n1Coordinator{store: db.N1Store(), ctx: context.Background(), deliveries: map[string]n1Delivery{}}
	s.ReadyToken = ""
	l := &n1Live{run: run, reservation: r, inst: inst, owned: true, identity: s, lastSeen: time.Now()}
	n.active = l
	return im, n, l, db.DB
}
func TestN1CleanupFailureKeepsRecoveryBarrier(t *testing.T) {
	im, n, l := n1WorkerFixture(t)
	calls := 0
	im.control.stop = func(context.Context, *instance.Maintenance, *instance.Instance) error {
		calls++
		return errors.New("synthetic stop blocked")
	}
	n.wg.Add(1)
	im.n1Finish(n, l)
	n.wg.Wait()
	r, err := n.store.Get(context.Background(), l.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Cleanup != "unknown" || r.State != "partial" || !l.blocked || n.active != l || calls != 1 {
		t.Fatalf("failure washed green: %+v blocked=%v calls=%d", r, l.blocked, calls)
	}
	s := l.inst.ControlSnapshot()
	if _, err = l.inst.ReserveN1("next", "scope", "evaluation", s.BootID, s.Revision, time.Now().Add(time.Minute)); err == nil {
		t.Fatal("unknown cleanup admitted new work")
	}
}
func TestN1OwnerDelegationPreservesTestedIdentityAndReceipt(t *testing.T) {
	im, n, l := n1WorkerFixture(t)
	// The tested identity differs from a later snapshot. A delegate must use the
	// saved tested identity, not current mutable state after an owner transition.
	original := l.identity
	original.ProcessGeneration = 17
	original.LoadGeneration = 23
	l.identity = original
	g := im.globalConfig.ResourcePolicy
	g.SetReleaseRunner(func(ticket *resourcepolicy.ReleaseTicket) {
		ticket.Terminating()
		ticket.Finish("failed", "synthetic_owner_stop_failure")
	})
	s := g.Snapshot()
	if _, err := g.Update(false, s.BootID, s.Revision); err != nil {
		t.Fatal(err)
	}
	if got := im.n1DelegateOwner(n, l); got != "unknown" {
		t.Fatal("failed owner receipt became released", got)
	}
	events, err := n.store.Events(context.Background(), l.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	delegates, refs := 0, 0
	releaseID := ""
	for _, e := range events {
		var payload map[string]json.RawMessage
		json.Unmarshal(e.Payload, &payload)
		switch e.Type {
		case "delegated_owner":
			delegates++
			json.Unmarshal(payload["release_operation_id"], &releaseID)
			var identity instance.ControlSnapshot
			json.Unmarshal(payload["identity"], &identity)
			if identity.ProcessGeneration != original.ProcessGeneration || identity.LoadGeneration != original.LoadGeneration || identity.BootID != original.BootID || identity.ReadyToken != "" {
				t.Fatal("delegation overwrote tested identity", identity)
			}
		case "owner_release_ref":
			refs++
			var ref string
			json.Unmarshal(payload["release_operation_id"], &ref)
			if ref != releaseID || len(payload["owner_receipt"]) == 0 {
				t.Fatal("missing authoritative receipt", e)
			}
		}
	}
	if delegates != 1 || refs != 1 {
		t.Fatal("delegation/receipt count", delegates, refs)
	}
}
func TestN1BlockedCleanupCannotCrossResumedGeneration(t *testing.T) {
	im, n, l := n1WorkerFixture(t)
	entered, proceed := make(chan struct{}), make(chan struct{})
	effects := 0
	im.control.stop = func(ctx context.Context, m *instance.Maintenance, i *instance.Instance) error {
		close(entered)
		<-proceed
		if err := m.CheckControl(ctx, false); err != nil {
			return err
		}
		effects++
		return nil
	}
	m, err := l.reservation.BeginControl(false)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() { state, _ := im.n1Cleanup(n, l, m, 5); m.Close(); done <- state }()
	<-entered
	g := im.globalConfig.ResourcePolicy
	g.SetReleaseRunner(func(ticket *resourcepolicy.ReleaseTicket) { <-ticket.Context().Done() })
	s := g.Snapshot()
	s, err = g.Update(false, s.BootID, s.Revision)
	if err != nil {
		t.Fatal(err)
	}
	s, err = g.Update(true, s.BootID, s.Revision)
	if err != nil {
		t.Fatal(err)
	}
	// Old maintenance still holds the admission barrier even after resume.
	snap := l.inst.ControlSnapshot()
	if _, err = l.inst.ReserveN1("too-early", "scope", "evaluation", snap.BootID, snap.Revision, time.Now().Add(time.Minute)); err == nil {
		t.Fatal("blocked cleanup lost barrier")
	}
	close(proceed)
	if state := <-done; state != "unknown" || effects != 0 {
		t.Fatal("old cleanup crossed resumed epoch", state, effects)
	}
	// Simulate a separately confirmed recovery in this synthetic instance. New
	// work uses a new permit; reusing the old reservation remains forbidden.
	if err = l.reservation.CloseConfirmed(); err != nil {
		t.Fatal(err)
	}
	next, err := l.inst.ReserveN1("new-generation", "scope", "evaluation", snap.BootID, snap.Revision, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	defer next.CloseConfirmed()
	if _, err = l.reservation.BeginControl(false); err == nil {
		t.Fatal("old cleanup acquired new generation")
	}
	if effects != 0 {
		t.Fatal("old effect touched new generation")
	}
}
