package instance

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A reader can already be in a blocking Read when a logger generation closes.
// Releasing it later must neither reopen that file nor write to a newer one.
type heldLogReader struct {
	entered chan struct{}
	release chan struct{}
	closed  chan struct{}
	once    sync.Once
	source  io.Reader
}

func (r *heldLogReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.entered); <-r.release })
	return r.source.Read(p)
}
func (r *heldLogReader) Close() error { close(r.closed); return nil }

func TestLoggerGenerationCleanupDoesNotCloseNewOutput(t *testing.T) {
	l := newLogger("generation", t.TempDir(), &LogRotationConfig{})
	if err := l.create(); err != nil {
		t.Fatal(err)
	}
	oldOut, oldWrite := io.Pipe()
	defer oldWrite.Close()
	oldErr, oldErrWrite := io.Pipe()
	defer oldErrWrite.Close()
	finishOld := l.startOutput(oldOut, oldErr)
	defer finishOld()
	l.logDir = t.TempDir()
	if err := l.create(); err != nil {
		t.Fatal(err)
	}
	newLog := l.logFile
	newOut, newWrite := io.Pipe()
	defer newWrite.Close()
	finishNew := l.startOutput(newOut)
	defer finishNew()
	oldDone := make(chan struct{})
	go func() { defer close(oldDone); finishOld() }()
	select {
	case <-oldDone:
	case <-time.After(3 * time.Second):
		t.Fatal("generation cleanup failed to close/join readers")
	}
	finishOld() // The monitor/stop cleanup may observe the same completed generation.
	l.mu.RLock()
	same := l.logFile == newLog
	l.mu.RUnlock()
	if !same {
		t.Fatal("old generation cleanup cleared the new logger")
	}
	if _, err := oldWrite.Write([]byte("stale\n")); err == nil {
		t.Fatal("old pipe remained open")
	}
	if _, err := oldErrWrite.Write([]byte("stale-error\n")); err == nil {
		t.Fatal("old stderr remained open")
	}
	if _, err := newWrite.Write([]byte("new-generation-output\n")); err != nil {
		t.Fatal("new pipe was closed", err)
	}
	finishNew()
	data, err := os.ReadFile(filepath.Join(l.logDir, "generation.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "new-generation-output") || strings.Contains(string(data), "stale") {
		t.Fatal("generation output isolation failed", string(data))
	}
	l.mu.RLock()
	open := l.logFile != nil
	l.mu.RUnlock()
	if open {
		t.Fatal("finished output retained log handle")
	}
}

func TestLoggerOldReaderCannotWriteNewGeneration(t *testing.T) {
	l := newLogger("generation", t.TempDir(), &LogRotationConfig{})
	if err := l.create(); err != nil {
		t.Fatal(err)
	}
	defer l.close()
	r := &heldLogReader{entered: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{}), source: strings.NewReader("old-generation-tail\n")}
	done := make(chan struct{})
	go func() { defer close(done); l.readOutput(r) }()
	select {
	case <-r.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("reader did not start")
	}
	l.close()
	// A distinct directory makes a wrong-generation write directly observable.
	l.logDir = t.TempDir()
	if err := l.create(); err != nil {
		t.Fatal(err)
	}
	close(r.release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("old reader did not finish")
	}
	data, err := os.ReadFile(filepath.Join(l.logDir, "generation.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "old-generation-tail") {
		t.Fatal("old output was written into the new generation's log")
	}
	select {
	case <-r.closed:
	default:
		t.Fatal("old reader pipe was not closed")
	}
}
