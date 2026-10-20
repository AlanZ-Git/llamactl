package manager

import (
	"context"
	"llamactl/pkg/instance"
	"llamactl/pkg/resourcepolicy"
	"llamactl/pkg/resources"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestResourceManagerCacheAndShutdown(t *testing.T) {
	im, _ := presetManager(t)
	im.lifecycle = newLifecycleManager(im.registry, im, time.Hour, true)
	im.lifecycle.start()
	var calls atomic.Int32
	entered := make(chan struct{})
	im.resources = resources.NewSampler(func(ctx context.Context) resources.Reading {
		calls.Add(1)
		close(entered)
		<-ctx.Done()
		return resources.Reading{}
	}, im.resourcesIdle)
	im.resources.Start()
	<-entered
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 100 {
				_, ok := im.ResourceSnapshot()
				if !ok {
					t.Error("missing shared sampler")
				}
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("reads started collectors")
	}
	done := make(chan struct{})
	go func() { im.Shutdown(); im.Shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("manager shutdown leaked sampler")
	}
}

func TestResourceManagerPauseWinsCadence(t *testing.T) {
	im, _ := presetManager(t)
	gate, err := resourcepolicy.Open("")
	if err != nil {
		t.Fatal(err)
	}
	im.globalConfig.ResourcePolicy = gate
	if !im.resourcesIdle() {
		t.Fatal("empty manager should sample slowly")
	}
	local, err := instance.NewDescriptor("synthetic", im.globalConfig, presetOptions(t, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := im.registry.add(local); err != nil {
		t.Fatal(err)
	}
	local.SetStatus(instance.Running)
	if im.resourcesIdle() {
		t.Fatal("active local instance should sample faster")
	}
	s := gate.Snapshot()
	if _, err := gate.Update(false, s.BootID, s.Revision); err != nil {
		t.Fatal(err)
	}
	if !im.resourcesIdle() {
		t.Fatal("manual pause must win cadence")
	}
	local.SetStatus(instance.Stopped)
	r, coverage := im.collectOwnedResources(context.Background())
	if len(r) != 1 || r[0].Reason != "no_owned_generation" || coverage != "manager_owned_local_only" {
		t.Fatal(r, coverage)
	}
}
