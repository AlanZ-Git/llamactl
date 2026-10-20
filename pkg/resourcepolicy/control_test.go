package resourcepolicy

import (
	"context"
	"testing"
	"time"
)

func TestP3CleanupPermitCannotCrossResume(t *testing.T) {
	g, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	s := g.Snapshot()
	s, err = g.Update(false, s.BootID, s.Revision)
	if err != nil {
		t.Fatal(err)
	}
	old := g.AcquireCleanup()
	defer old.Close()
	ctx, cancel := old.BindContext(context.Background())
	defer cancel()
	if _, err = g.Update(true, s.BootID, s.Revision); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("cleanup permit not cancelled")
	}
	dispatched := false
	if err := old.CleanupEffect(func() error { dispatched = true; return nil }); err == nil || dispatched {
		t.Fatal("old cleanup signalled resumed generation")
	}
	current := g.AcquireCleanup()
	defer current.Close()
	if err := current.CleanupEffect(func() error { dispatched = true; return nil }); err != nil || !dispatched {
		t.Fatal("new generation cleanup blocked", err)
	}
}
