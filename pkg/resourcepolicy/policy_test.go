package resourcepolicy

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestPolicyDanglingLinkFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "absent"), filepath.Join(dir, "resource-policy.json")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	g, err := Open(dir)
	if err == nil || g.Snapshot().AllowAI {
		t.Fatal("dangling policy link treated as fresh missing file")
	}
}

func TestNewPauseInvalidatesPendingResume(t *testing.T) {
	g, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := g.Snapshot()
	s, err = g.Update(false, s.BootID, s.Revision)
	if err != nil {
		t.Fatal(err)
	}
	entered, finish := make(chan struct{}), make(chan struct{})
	original := g.persist
	var once sync.Once
	g.persist = func(path string, data []byte) error {
		once.Do(func() { close(entered); <-finish })
		return original(path, data)
	}
	resume := make(chan error, 1)
	go func() { _, err := g.Update(true, s.BootID, s.Revision); resume <- err }()
	<-entered
	pending := g.Snapshot()
	// The write mutex is held by resume. Pause must still linearize now.
	pause := make(chan error, 1)
	go func() { _, err := g.Update(false, pending.BootID, pending.Revision); pause <- err }()
	// Wait for the accepted pause without timing sleeps, then release the I/O.
	for g.Snapshot().Revision == pending.Revision {
		runtime.Gosched()
	}
	if g.Snapshot().AllowAI {
		t.Fatal("pause waited for writer")
	}
	// Only the first write blocks; access remains serialized by writeMu.
	close(finish)
	if err := <-resume; !errors.Is(err, ErrConflict) {
		t.Fatalf("superseded resume: %v", err)
	}
	if err := <-pause; err != nil {
		t.Fatal(err)
	}
	if g.Snapshot().AllowAI || !g.Snapshot().Durable {
		t.Fatalf("new pause lost: %+v", g.Snapshot())
	}
}

func TestPauseWithHeldPermits(t *testing.T) {
	g, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	shared, err := g.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()
	maintenance, err := g.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close()
	before := g.Snapshot()
	done := make(chan error, 1)
	go func() { _, err := g.Update(false, before.BootID, before.Revision); done <- err }()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	s := g.Snapshot()
	if s.AllowAI || s.Admission != "blocked" || s.Release != "unreleased_unknown" || !s.Durable {
		t.Fatalf("pause: %+v", s)
	}
	if _, err := g.Acquire(); !errors.Is(err, ErrPaused) {
		t.Fatalf("admit: %v", err)
	}
	if err := shared.Check(); !errors.Is(err, ErrPaused) {
		t.Fatalf("old permit: %v", err)
	}
	if _, err := g.Update(true, s.BootID, s.Revision); err != nil {
		t.Fatal(err)
	}
	if err := maintenance.Check(); !errors.Is(err, ErrPaused) {
		t.Fatalf("old epoch revived: %v", err)
	}
	shared.Close()
	maintenance.Close()
	if g.Active() != 0 {
		t.Fatal("leaked permits")
	}
}

func TestPolicyPersistence(t *testing.T) {
	dir := t.TempDir()
	g, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	initial := g.Snapshot()
	if !initial.AllowAI || initial.Durable {
		t.Fatalf("fresh: %+v", initial)
	}
	s, err := g.Update(false, initial.BootID, initial.Revision)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	next := loaded.Snapshot()
	if next.AllowAI || !next.Durable || next.Revision != s.Revision || next.BootID == s.BootID {
		t.Fatalf("reload: %+v", next)
	}
	if _, err := loaded.Update(true, s.BootID, s.Revision); !errors.Is(err, ErrConflict) {
		t.Fatalf("boot: %v", err)
	}
	if _, err := loaded.Update(true, next.BootID, next.Revision+1); !errors.Is(err, ErrConflict) {
		t.Fatalf("revision: %v", err)
	}
	if loaded.Snapshot() != next {
		t.Fatal("stale write changed state")
	}
}

func TestInvalidPolicyFailsClosed(t *testing.T) {
	for _, data := range []string{"null", "{}", `{"version":1,"allow_ai":null,"revision":0}`, `{"version":1,"allow_ai":false}`, `{"version":1,"allow_ai":true,"revision":-1}`, `{"version":1,"allow_ai":true,"revision":0,"extra":1}`, `{"version":1,"allow_ai":true,"revision":0} {}`, `{"version":2,"allow_ai":true,"revision":0}`, `{"version":1,"allow_ai":false,"allow_ai":true,"revision":0}`, `{"version":1,"allow_ai":1,"revision":0}`} {
		t.Run(data, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "resource-policy.json")
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			g, err := Open(dir)
			if err == nil || g.Snapshot().AllowAI || g.Snapshot().Durable {
				t.Fatalf("invalid accepted: %+v %v", g.Snapshot(), err)
			}
			unchanged, _ := os.ReadFile(path)
			if string(unchanged) != data {
				t.Fatal("corrupt file overwritten")
			}
		})
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "resource-policy.json"), 0700); err != nil {
		t.Fatal(err)
	}
	g, err := Open(dir)
	if err == nil || g.Snapshot().AllowAI {
		t.Fatal("read error must block")
	}
}

func TestPersistenceFailureAndMemory(t *testing.T) {
	dir := t.TempDir()
	g, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A directory at the target causes replacement failure on every OS, even as root.
	if err := os.Mkdir(filepath.Join(dir, "resource-policy.json"), 0700); err != nil {
		t.Fatal(err)
	}
	s := g.Snapshot()
	s, err = g.Update(false, s.BootID, s.Revision)
	if err == nil || s.AllowAI || s.Durable {
		t.Fatalf("pause failure: %+v %v", s, err)
	}
	s, err = g.Update(true, s.BootID, s.Revision)
	if err == nil || s.AllowAI || s.Durable {
		t.Fatalf("resume failure: %+v %v", s, err)
	}
	memory, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	s = memory.Snapshot()
	s, err = memory.Update(false, s.BootID, s.Revision)
	if err != nil || s.Durable || s.AllowAI {
		t.Fatalf("memory: %+v %v", s, err)
	}
}
