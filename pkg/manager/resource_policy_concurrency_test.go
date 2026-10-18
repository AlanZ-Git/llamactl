package manager

import (
	"errors"
	"llamactl/pkg/resourcepolicy"
	"sync"
	"testing"
)

func TestOwnerPolicyPauseWhileMaintenancePersists(t *testing.T) {
	im, _ := presetManager(t)
	g, err := resourcepolicy.Open("")
	if err != nil {
		t.Fatal(err)
	}
	im.globalConfig.ResourcePolicy = g
	store := &admissionBlockingStore{}
	im.db = store
	inst, err := im.CreateInstance("held-policy", controlledOptions(t, 18001))
	if err != nil {
		t.Fatal(err)
	}
	store.entered, store.finish = make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(store.finish) })
	done := make(chan error, 1)
	go func() { _, err := im.UpdateInstance(inst.Name, controlledOptions(t, 18002)); done <- err }()
	<-store.entered
	s := g.Snapshot()
	s, err = g.Update(false, s.BootID, s.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if s.AllowAI || s.Release != "unreleased_unknown" {
		t.Fatalf("pause %+v", s)
	}
	if _, err := im.StartInstance(inst.Name); !errors.Is(err, resourcepolicy.ErrPaused) {
		t.Fatalf("pause did not outrank held maintenance: %v", err)
	}
	once.Do(func() { close(store.finish) })
	if err := <-done; err != nil {
		t.Fatal("already committed work must finish", err)
	}
	if g.Active() != 0 {
		t.Fatal("permit leaked")
	}
}
