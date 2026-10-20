//go:build linux || windows

package manager

import (
	"fmt"
	"io"
	"llamactl/pkg/config"
	"llamactl/pkg/instance"
	"llamactl/pkg/resourcepolicy"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOwnerManagerHelperProcess(t *testing.T) {
	marker := os.Getenv("OWNER_SYNTHETIC_READY")
	if marker == "" {
		return
	}
	go func() { time.Sleep(60 * time.Second); os.Exit(90) }()
	ln, err := net.Listen("tcp", "127.0.0.1:"+os.Getenv("OWNER_SYNTHETIC_PORT"))
	if err != nil {
		os.Exit(91)
	}
	if os.WriteFile(marker, []byte(ln.Addr().String()), 0600) != nil {
		os.Exit(92)
	}
	finish := make(chan struct{})
	var once sync.Once
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/completion" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: first\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-finish:
				fmt.Fprint(w, "data: [DONE]\n\n")
			case <-r.Context().Done():
			}
			return
		}
		if r.URL.Path == "/finish" {
			once.Do(func() { close(finish) })
		}
		w.WriteHeader(204)
	}))
	_, _ = io.Copy(io.Discard, os.Stdin)
	_ = ln.Close()
	os.Exit(0)
}

func ownerManagerFixture(t *testing.T, count int) (*instanceManager, []*instance.Instance, []string) {
	t.Helper()
	im, _ := presetManager(t)
	im.ports = newPortAllocator(1024, 65535)
	dir := t.TempDir()
	g, err := resourcepolicy.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	im.globalConfig.DataDir = dir
	im.globalConfig.ResourcePolicy = g
	g.SetReleaseRunner(im.releaseOwned)
	im.globalConfig.Backends.LlamaCpp.Command = os.Args[0]
	im.globalConfig.Backends.LlamaCpp.Args = []string{"-test.run=^TestOwnerManagerHelperProcess$", "--"}
	var instances []*instance.Instance
	var addresses []string
	for n := 0; n < count; n++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		opts := presetOptions(t, port)
		opts.EnvironmentPolicy = "isolated"
		marker := filepath.Join(t.TempDir(), "ready")
		opts.Environment = map[string]string{"OWNER_SYNTHETIC_READY": marker, "OWNER_SYNTHETIC_PORT": strconv.Itoa(port)}
		inst, err := im.CreateInstance(fmt.Sprintf("owner-%d", n), opts)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := im.StartInstance(inst.Name); err != nil {
			t.Fatal(err)
		}
		instances = append(instances, inst)
		deadline := time.Now().Add(5 * time.Second)
		for {
			b, err := os.ReadFile(marker)
			if err == nil {
				addresses = append(addresses, string(b))
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("helper not ready")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	t.Cleanup(func() {
		s := g.Snapshot()
		if s.AllowAI {
			s, _ = g.Update(false, s.BootID, s.Revision)
		}
		_, _ = g.RequestRelease(s.BootID, s.Revision, "cleanup", "immediate")
		waitManagerRelease(t, g, 12*time.Second)
	})
	return im, instances, addresses
}
func waitManagerRelease(t *testing.T, g *resourcepolicy.Gate, budget time.Duration) resourcepolicy.Snapshot {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		s := g.Snapshot()
		if s.ReleaseOperation != nil && s.ReleaseOperation.EndedAt != nil {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("release operation did not finish")
	return g.Snapshot()
}
func managerReachable(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func TestOwnerManagerActualProcessSSE(t *testing.T) {
	for _, mode := range []string{"graceful", "immediate"} {
		t.Run(mode, func(t *testing.T) {
			im, instances, addresses := ownerManagerFixture(t, 1)
			g := im.globalConfig.ResourcePolicy
			front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = instances[0].ServeHTTP(w, r) }))
			defer front.Close()
			client := &http.Client{Timeout: 5 * time.Second}
			resp, err := client.Post(front.URL+"/completion", "application/json", strings.NewReader(`{"stream":true}`))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			prefix := make([]byte, len("data: first\n\n"))
			if _, err := io.ReadFull(resp.Body, prefix); err != nil {
				t.Fatal(err)
			}
			s := g.Snapshot()
			s, err = g.Update(false, s.BootID, s.Revision)
			if err != nil || s.Release != "draining" || !managerReachable(addresses[0]) {
				t.Fatal("pause did not preserve active stream", s, err)
			}
			if mode == "graceful" {
				end, err := client.Get("http://" + addresses[0] + "/finish")
				if err != nil {
					t.Fatal(err)
				}
				end.Body.Close()
			} else if _, err := g.RequestRelease(s.BootID, s.Revision, "sse-immediate", mode); err != nil {
				t.Fatal(err)
			}
			rest, err := io.ReadAll(resp.Body)
			if mode == "graceful" {
				if err != nil || !strings.Contains(string(rest), "[DONE]") {
					t.Fatal("natural SSE completion lost", string(rest), err)
				}
			} else if err == nil || strings.Contains(string(rest), "[DONE]") {
				t.Fatal("interrupted SSE fabricated completion", string(rest), err)
			}
			s = waitManagerRelease(t, g, 5*time.Second)
			if s.Release != "released" || managerReachable(addresses[0]) {
				t.Fatal("SSE finished but owned process not released", s)
			}
		})
	}
}

func TestOwnerManagerGracefulNaturalFinishAllInstances(t *testing.T) {
	im, instances, addresses := ownerManagerFixture(t, 2)
	g := im.globalConfig.ResourcePolicy
	held, err := instances[0].BeginMaintenance()
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	s := g.Snapshot()
	start := time.Now()
	s, err = g.Update(false, s.BootID, s.Revision)
	if err != nil || s.Release != "draining" || time.Since(start) > time.Second {
		t.Fatal(s, err)
	}
	for _, addr := range addresses {
		if !managerReachable(addr) {
			t.Fatal("graceful killed before natural finish")
		}
	}
	held.Close()
	s = waitManagerRelease(t, g, 5*time.Second)
	if s.Release != "released" {
		t.Fatal(s)
	}
	for _, addr := range addresses {
		if managerReachable(addr) {
			t.Fatal("released with live process")
		}
	}
}

func TestOwnerManagerGracefulDeadlineNeverForces(t *testing.T) {
	im, _, addresses := ownerManagerFixture(t, 1)
	g := im.globalConfig.ResourcePolicy
	held, err := g.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	s := g.Snapshot()
	s, err = g.Update(false, s.BootID, s.Revision)
	if err != nil {
		t.Fatal(err)
	}
	s = waitManagerRelease(t, g, resourcepolicy.GracefulTimeout+3*time.Second)
	if s.AllowAI || s.Release != "unreleased_unknown" || s.ReleaseOperation.ErrorCode != "drain_timeout" {
		t.Fatal(s)
	}
	if !managerReachable(addresses[0]) {
		t.Fatal("drain deadline auto-escalated")
	}
	if _, err := g.RequestRelease(s.BootID, s.Revision, "owner-force", "immediate"); err != nil {
		t.Fatal(err)
	}
	s = waitManagerRelease(t, g, 5*time.Second)
	if s.Release != "released" || managerReachable(addresses[0]) {
		t.Fatal("explicit immediate did not release", s)
	}
}

func TestOwnerManagerNeverContactsRemoteInstances(t *testing.T) {
	im, _, _ := ownerManagerFixture(t, 1)
	g := im.globalConfig.ResourcePolicy
	var calls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(204) }))
	defer remote.Close()
	im.globalConfig.Nodes = map[string]config.NodeConfig{"synthetic-remote": {Address: remote.URL}}
	opts := presetOptions(t, 18005)
	opts.Nodes = map[string]struct{}{"synthetic-remote": {}}
	stub, err := instance.NewChecked("remote", im.globalConfig, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	stub.SetStatus(instance.Running)
	if err := im.registry.add(stub); err != nil {
		t.Fatal(err)
	}
	s := g.Snapshot()
	s, err = g.Update(false, s.BootID, s.Revision)
	if err != nil {
		t.Fatal(err)
	}
	_, err = g.RequestRelease(s.BootID, s.Revision, "local-only", "immediate")
	if err != nil {
		t.Fatal(err)
	}
	s = waitManagerRelease(t, g, 5*time.Second)
	if s.Release != "released" || calls.Load() != 0 || !stub.IsRunning() {
		t.Fatal("release touched remote instance", s, calls.Load())
	}
}

func TestOwnerManagerPersistenceFailureStillReleases(t *testing.T) {
	im, _, addresses := ownerManagerFixture(t, 1)
	g := im.globalConfig.ResourcePolicy
	if err := os.Mkdir(filepath.Join(im.globalConfig.DataDir, "resource-policy.json"), 0700); err != nil {
		t.Fatal(err)
	}
	s := g.Snapshot()
	s, err := g.Update(false, s.BootID, s.Revision)
	if err == nil || s.AllowAI || s.Durable {
		t.Fatal("pause persistence failure not failure-closed", s, err)
	}
	s = waitManagerRelease(t, g, 5*time.Second)
	if s.Release != "released" || managerReachable(addresses[0]) {
		t.Fatal("failed persistence skipped release", s)
	}
	restored, err := resourcepolicy.Open(im.globalConfig.DataDir)
	if err == nil || restored.Snapshot().AllowAI || restored.Snapshot().Release != "unreleased_unknown" || restored.Snapshot().ReleaseOperation != nil {
		t.Fatal("blocked boot replayed release")
	}
}

func TestOwnerManagerImmediatePreemptsHeldMaintenance(t *testing.T) {
	im, instances, addresses := ownerManagerFixture(t, 2)
	g := im.globalConfig.ResourcePolicy
	held, err := instances[0].BeginMaintenance()
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	s := g.Snapshot()
	s, err = g.Update(false, s.BootID, s.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.RequestRelease(s.BootID, s.Revision, "preempt", "immediate"); err != nil {
		t.Fatal(err)
	}
	s = waitManagerRelease(t, g, 5*time.Second)
	if s.Release != "released" {
		t.Fatal(s)
	}
	for _, addr := range addresses {
		if managerReachable(addr) {
			t.Fatal("immediate retained local process")
		}
	}
}
