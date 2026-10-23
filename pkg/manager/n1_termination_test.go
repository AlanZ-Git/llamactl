package manager

import (
	"context"
	"encoding/json"
	"errors"

	"llamactl/pkg/instance"
	"llamactl/pkg/resourcepolicy"
	"testing"
	"time"
)

func n1SafetyForTest(t *testing.T, im *instanceManager, n *n1Coordinator, l *n1Live, action, key string) {
	t.Helper()
	s := l.inst.ControlSnapshot()
	body := n1JSON(n1Envelope{Boot: s.BootID, Revision: &s.Revision, Key: key})
	status, _, err := im.n1Safety(context.Background(), n, l.inst, l.run, action, body)
	if err != nil || status != 200 {
		t.Errorf("%s rejected: status=%d err=%v", action, status, err)
	}
}

func n1AssertTerminal(t *testing.T, n *n1Coordinator, l *n1Live, state, execution, cleanup string) {
	t.Helper()
	n.wg.Wait()
	r, err := n.store.Get(context.Background(), l.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var terminal struct {
		Execution string `json:"execution_state"`
		Cleanup   string `json:"cleanup_state"`
	}
	if err = json.Unmarshal(r.Terminal, &terminal); err != nil {
		t.Fatal(err)
	}
	if r.FrozenAt == nil || r.State != state || r.Cleanup != cleanup || terminal.Execution != execution || terminal.Cleanup != cleanup {
		t.Fatalf("terminal: state=%s execution=%s cleanup=%s/%s frozen=%v; want %s/%s/%s", r.State, terminal.Execution, r.Cleanup, terminal.Cleanup, r.FrozenAt, state, execution, cleanup)
	}
	n.mu.Lock()
	active, blocked := n.active, l.blocked
	n.mu.Unlock()
	if cleanup == "released" {
		if active != nil || blocked {
			t.Fatal("confirmed release retained live barrier")
		}
	} else {
		if active != l || !blocked {
			t.Fatal("unknown cleanup lost recovery barrier")
		}
		s := l.inst.ControlSnapshot()
		next, err := l.inst.ReserveN1("after-unknown", "scope", "evaluation", s.BootID, s.Revision, time.Now().Add(time.Minute))
		if err == nil {
			next.CloseConfirmed()
			t.Fatal("unknown cleanup admitted new work")
		}
	}
}

func TestN1SafetyCauseAndEvaluationResultSurviveCleanup(t *testing.T) {
	for _, tc := range []struct {
		name, kind, action, receipt, state, execution string
		cleanupFails                                  bool
	}{
		{name: "release", kind: "switch", action: "release", state: "succeeded", execution: "succeeded"},
		{name: "release_cleanup_unknown", kind: "switch", action: "release", cleanupFails: true, state: "partial", execution: "succeeded"},
		{name: "cancel", kind: "switch", action: "cancel", state: "cancelled", execution: "cancelled"},
		{name: "cancel_cleanup_unknown", kind: "switch", action: "cancel", cleanupFails: true, state: "partial", execution: "cancelled"},
		{name: "finish_complete", kind: "evaluation", action: "finish", receipt: "succeeded", state: "succeeded", execution: "succeeded"},
		{name: "finish_missing_receipt", kind: "evaluation", action: "finish", state: "partial", execution: "partial"},
		{name: "finish_failed_receipt", kind: "evaluation", action: "finish", receipt: "failed", state: "partial", execution: "partial"},
		{name: "finish_cleanup_unknown", kind: "evaluation", action: "finish", receipt: "succeeded", cleanupFails: true, state: "partial", execution: "succeeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			im, n, l, _ := n1WorkerRunFixture(t, tc.kind, time.Minute)
			if tc.kind == "evaluation" {
				l.eval.Candidates = []N1Candidate{{ID: "candidate"}}
				if tc.receipt != "" {
					_, err := n.store.Append(context.Background(), l.run.ID, "receipt", "1", n1JSON(n1Receipt{Attempt: 1, Candidate: "candidate", State: tc.receipt, Cleanup: "released"}))
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			ticked := false
			im.control.stop = func(ctx context.Context, _ *instance.Maintenance, _ *instance.Instance) error {
				ticked = true
				if ctx.Err() != nil || l.reservation.Context().Err() != context.Canceled {
					t.Error("cleanup did not have an independent live context")
				}
				im.n1Tick(n)
				// A late client cancellation and heartbeat expiry cannot rewrite the
				// already accepted release/finish/cancel cause.
				n1SafetyForTest(t, im, n, l, "cancel", "late-cancel")
				n1SafetyForTest(t, im, n, l, "release", "late-release")
				n.mu.Lock()
				l.lastSeen = time.Now().Add(-61 * time.Second)
				n.mu.Unlock()
				im.n1Tick(n)
				if tc.cleanupFails {
					return errors.New("synthetic cleanup failure")
				}
				return nil
			}
			n1SafetyForTest(t, im, n, l, tc.action, "first-safety")
			cleanup := "released"
			if tc.cleanupFails {
				cleanup = "unknown"
			}
			n1AssertTerminal(t, n, l, tc.state, tc.execution, cleanup)
			if !ticked {
				t.Fatal("cleanup interleaving was not exercised")
			}
		})
	}
}

func TestN1RealExpiryRemainsDeadlineExceeded(t *testing.T) {
	for _, mode := range []string{"absolute", "absolute_before_release", "idle", "heartbeat", "heartbeat_cleanup_unknown"} {
		t.Run(mode, func(t *testing.T) {
			kind, lifetime := "switch", time.Minute
			if mode == "absolute" || mode == "absolute_before_release" {
				lifetime = time.Second
			}
			if mode == "heartbeat" || mode == "heartbeat_cleanup_unknown" {
				kind = "evaluation"
			}
			im, n, l, _ := n1WorkerRunFixture(t, kind, lifetime)
			switch mode {
			case "absolute", "absolute_before_release":
				select {
				case <-l.reservation.Context().Done():
				case <-time.After(5 * time.Second):
					t.Fatal("absolute deadline did not fire")
				}
				if l.reservation.Context().Err() != context.DeadlineExceeded || !l.reservation.Expired() {
					t.Fatal("probe did not reach a real absolute deadline")
				}
			case "idle":
				contract := instance.RequestContract{TemplateVersion: "v1", Parameters: map[string]json.RawMessage{"max_tokens": json.RawMessage(`4`)}}
				if err := l.reservation.ActivateService(instance.RecipeRef{ID: "recipe", Version: 1}, "alpha", contract, time.Millisecond); err != nil {
					t.Fatal(err)
				}
				// This timer starts after activation, so its firing establishes idle
				// expiry without relying on a scheduler race or cancelling context.
				<-time.After(time.Millisecond)
				if !l.reservation.Expired() || l.reservation.Context().Err() != nil {
					t.Fatal("probe did not reach idle-only expiry")
				}
			default:
				l.lastSeen = time.Now().Add(-61 * time.Second)
				if l.reservation.Expired() || l.reservation.Context().Err() != nil {
					t.Fatal("heartbeat probe also expired reservation")
				}
			}
			ticked := false
			im.control.stop = func(ctx context.Context, _ *instance.Maintenance, _ *instance.Instance) error {
				ticked = true
				if ctx.Err() != nil {
					t.Error("execution deadline cancelled cleanup")
				}
				n1SafetyForTest(t, im, n, l, "cancel", "cancel-after-expiry")
				im.n1Tick(n)
				if mode == "heartbeat_cleanup_unknown" {
					return errors.New("synthetic cleanup failure")
				}
				return nil
			}
			if mode == "absolute_before_release" {
				n1SafetyForTest(t, im, n, l, "release", "release-after-expiry")
			} else {
				im.n1Tick(n)
			}
			state, cleanup := "deadline_exceeded", "released"
			if mode == "heartbeat_cleanup_unknown" {
				state, cleanup = "partial", "unknown"
			}
			n1AssertTerminal(t, n, l, state, "deadline_exceeded", cleanup)
			if !ticked {
				t.Fatal("expiry did not dispatch cleanup")
			}
		})
	}
}

func TestN1ReleaseCauseSurvivesDeadlineDuringCleanup(t *testing.T) {
	im, n, l, _ := n1WorkerRunFixture(t, "switch", time.Second)
	deadline, _ := l.reservation.Context().Deadline()
	im.control.stop = func(ctx context.Context, _ *instance.Maintenance, _ *instance.Instance) error {
		<-time.After(time.Until(deadline))
		if !l.reservation.Expired() || ctx.Err() != nil {
			t.Error("probe did not cross execution deadline with cleanup still live")
		}
		im.n1Tick(n)
		return nil
	}
	n1SafetyForTest(t, im, n, l, "release", "release-before-expiry")
	n1AssertTerminal(t, n, l, "succeeded", "succeeded", "released")
}

func TestN1OwnerRevocationIsCancellationWithIndependentReceipt(t *testing.T) {
	for _, name := range []string{"released", "failed"} {
		t.Run(name, func(t *testing.T) {
			released := name == "released"
			im, n, l := n1WorkerFixture(t)
			g := im.globalConfig.ResourcePolicy
			g.SetReleaseRunner(func(ticket *resourcepolicy.ReleaseTicket) {
				ticket.Terminating()
				if released {
					ticket.Finish("released", "")
				} else {
					ticket.Finish("failed", "synthetic_owner_stop_failure")
				}
			})
			s := g.Snapshot()
			if _, err := g.Update(false, s.BootID, s.Revision); err != nil {
				t.Fatal(err)
			}
			select {
			case <-l.reservation.Context().Done():
			case <-time.After(time.Second):
				t.Fatal("owner did not cancel reservation")
			}
			if l.reservation.Expired() {
				t.Fatal("owner revocation became expiry")
			}
			im.n1Tick(n)
			state, cleanup := "cancelled", "released"
			if !released {
				state, cleanup = "partial", "unknown"
			}
			n1AssertTerminal(t, n, l, state, "cancelled", cleanup)
			events, err := n.store.Events(context.Background(), l.run.ID)
			if err != nil {
				t.Fatal(err)
			}
			delegates, receipts := 0, 0
			for _, event := range events {
				if event.Type == "delegated_owner" {
					delegates++
				}
				if event.Type == "owner_release_ref" {
					receipts++
				}
			}
			if delegates != 1 || receipts != 1 {
				t.Fatalf("owner receipt was lost or duplicated: delegates=%d receipts=%d", delegates, receipts)
			}
		})
	}
}

func TestN1CapabilityRevocationPreservesFirstCause(t *testing.T) {
	for _, first := range []string{"revocation", "release", "deadline"} {
		t.Run(first, func(t *testing.T) {
			kind, capability := "switch", "service_use"
			if first == "deadline" {
				kind, capability = "evaluation", "evaluate"
			}
			im, n, l, db := n1WorkerRunFixture(t, kind, time.Minute)
			if _, err := db.Exec(`INSERT INTO api_keys(id,key_hash,name,user_id,created_at,updated_at) VALUES(17,'synthetic','synthetic','synthetic',1,1)`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO instances(id,name,created_at,updated_at,options_json) VALUES(?,?,1,1,'{}')`, l.inst.ID, l.inst.Name); err != nil {
				t.Fatal(err)
			}
			l.run.Scope = "key:17"
			if err := n.store.SetCapabilities(context.Background(), 17, l.inst.ID, []string{capability}); err != nil {
				t.Fatal(err)
			}
			im.n1Tick(n)
			if l.closing {
				t.Fatal("valid grant closed the live run")
			}
			revoke := func() {
				if err := n.store.SetCapabilities(context.Background(), 17, l.inst.ID, nil); err != nil {
					t.Error(err)
				}
				im.n1Tick(n)
			}
			im.control.stop = func(context.Context, *instance.Maintenance, *instance.Instance) error {
				revoke()
				return nil
			}
			state := "cancelled"
			switch first {
			case "revocation":
				revoke()
			case "release":
				state = "succeeded"
				n1SafetyForTest(t, im, n, l, "release", "release-before-revoke")
			case "deadline":
				state = "deadline_exceeded"
				l.lastSeen = time.Now().Add(-61 * time.Second)
				im.n1Tick(n)
			}
			n1AssertTerminal(t, n, l, state, state, "released")
		})
	}
}

func TestN1ShutdownPreservesFirstCauseDuringCleanup(t *testing.T) {
	for _, first := range []string{"shutdown", "release", "deadline"} {
		t.Run(first, func(t *testing.T) {
			kind := "switch"
			if first == "deadline" {
				kind = "evaluation"
			}
			im, n, l, _ := n1WorkerRunFixture(t, kind, time.Minute)
			n.ctx, n.cancel = context.WithCancel(context.Background())
			defer n.cancel()
			im.n1Once.Do(func() {})
			im.n1 = n
			entered, proceed := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-proceed:
				default:
					close(proceed)
				}
			}()
			im.control.stop = func(ctx context.Context, _ *instance.Maintenance, _ *instance.Instance) error {
				close(entered)
				select {
				case <-proceed:
					im.n1Tick(n)
					return ctx.Err()
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			state := "cancelled"
			if first == "release" {
				state = "succeeded"
				n1SafetyForTest(t, im, n, l, "release", "release-before-shutdown")
			} else if first == "deadline" {
				state = "deadline_exceeded"
				l.lastSeen = time.Now().Add(-61 * time.Second)
				im.n1Tick(n)
			}
			if first != "shutdown" {
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("cleanup did not start")
				}
			}
			done := make(chan struct{})
			go func() { im.shutdownN1(); close(done) }()
			<-n.ctx.Done()
			close(proceed)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("shutdown did not join cleanup")
			}
			n1AssertTerminal(t, n, l, state, state, "released")
		})
	}
}
