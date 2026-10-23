package database

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestN1DurableAcceptanceDispatchAndFinalize(t *testing.T) {
	db := newTestDB(t)
	s := db.N1Store()
	ctx := context.Background()
	r := ExecutionRun{ID: "run", Kind: "evaluation", Scope: "key:1", Instance: "i", Boot: "boot", Key: "accept", Payload: json.RawMessage(`{"x":0}`), Manifest: json.RawMessage(`{"candidates":["a"]}`), Deadline: time.Now().Add(time.Minute).UnixMilli(), Budget: RunBudget{Requests: 1, Tokens: 10, Bytes: 100}}
	got, replay, err := s.Accept(ctx, r)
	if err != nil || replay || got.State != "accepted" {
		t.Fatalf("accept: %+v %v %v", got, replay, err)
	}
	if _, replay, err = s.Accept(ctx, r); err != nil || !replay {
		t.Fatalf("lost accept replay: %v %v", replay, err)
	}
	r.Payload = json.RawMessage(`{"x":1}`)
	if _, _, err = s.Accept(ctx, r); !errors.Is(err, ErrN1Conflict) {
		t.Fatalf("conflict: %v", err)
	}
	payload := json.RawMessage(`{"candidate":"a"}`)
	e, replay, err := s.Dispatch(ctx, "run", "attempt", payload, 10, 100)
	if err != nil || replay || e.Type != "dispatch" {
		t.Fatalf("dispatch: %+v %v", e, err)
	}
	if _, replay, err = s.Dispatch(ctx, "run", "attempt", payload, 10, 100); err != nil || !replay {
		t.Fatalf("dispatch replay: %v %v", replay, err)
	}
	if _, _, err = s.Dispatch(ctx, "run", "second", payload, 1, 1); !errors.Is(err, ErrN1Budget) {
		t.Fatalf("budget: %v", err)
	}
	if _, err = s.Append(ctx, "run", "receipt", "attempt", json.RawMessage(`{"state":"succeeded"}`)); err != nil {
		t.Fatal(err)
	}
	if err = s.Freeze(ctx, "run", "succeeded", "released", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	final := json.RawMessage(`{"schema_version":1}`)
	f, replay, err := s.Finalize(ctx, "run", 1, final)
	if err != nil || replay {
		t.Fatalf("finalize: %v", err)
	}
	f2, replay, err := s.Finalize(ctx, "run", 1, final)
	if err != nil || !replay || f.Digest != f2.Digest {
		t.Fatalf("finalize replay: %v", err)
	}
	if _, _, err = s.Finalize(ctx, "run", 1, json.RawMessage(`{"schema_version":2}`)); !errors.Is(err, ErrN1Conflict) {
		t.Fatal(err)
	}
	if _, err = s.Append(ctx, "run", "receipt", "late", final); err == nil {
		t.Fatal("receipt set not frozen")
	}
}

func TestN1RestartNeverReplaysAndFailureReservesNothing(t *testing.T) {
	db := newTestDB(t)
	s := db.N1Store()
	ctx := context.Background()
	r := ExecutionRun{ID: "r", Kind: "evaluation", Scope: "s", Instance: "i", Boot: "old", Key: "k", Payload: json.RawMessage(`{}`), Manifest: json.RawMessage(`{}`), Deadline: time.Now().Add(time.Minute).UnixMilli(), Budget: RunBudget{Requests: 2, Tokens: 20, Bytes: 200}}
	if _, _, err := s.Accept(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_dispatch BEFORE INSERT ON execution_events WHEN NEW.event_type='dispatch' BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Dispatch(ctx, "r", "a", json.RawMessage(`{}`), 10, 100); err == nil {
		t.Fatal("injected write succeeded")
	}
	got, err := s.Get(ctx, "r")
	if err != nil || got.Budget.ReservedRequests != 0 {
		t.Fatalf("transaction leaked: %+v %v", got, err)
	}
	if err := s.Interrupt(ctx, "new"); err != nil {
		t.Fatal(err)
	}
	got, err = s.Get(ctx, "r")
	if err != nil || got.State != "interrupted" || got.Cleanup != "unknown" {
		t.Fatalf("restart: %+v %v", got, err)
	}
	if _, _, err = s.Dispatch(ctx, "r", "a", json.RawMessage(`{}`), 10, 100); err == nil {
		t.Fatal("replayed interrupted run")
	}
}
