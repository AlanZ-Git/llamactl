// Package resourcepolicy implements an owner-controlled new-work latch, not a
// drain, process terminator, or proof of resource release.
package resourcepolicy

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

var ErrPaused = errors.New("ai_paused: owner must resume AI; retryable=false")
var ErrConflict = errors.New("resource policy boot_id or revision conflict")

// Snapshot describes policy only. Even with no permits, blocked never means released.
type Snapshot struct {
	AllowAI   bool   `json:"allow_ai"`
	Admission string `json:"admission"`
	Durable   bool   `json:"durable"`
	Release   string `json:"release"`
	Revision  uint64 `json:"revision"`
	BootID    string `json:"boot_id"`
}

type Gate struct {
	mu      sync.Mutex
	writeMu sync.Mutex
	state   Snapshot
	epoch   uint64
	active  uint64
	path    string
	// Immutable after construction; separated for deterministic I/O failure tests.
	persist func(string, []byte) error
}

// Open loads policy before any instance construction. Empty dataDir explicitly
// selects nondurable embedded mode; it never reads or writes the working directory.
// Missing files default allowed but are not claimed durable until an explicit write.
// Every read/parse error returns a usable BLOCKED gate as well as the error.
func Open(dataDir string) (*Gate, error) {
	g := &Gate{persist: replaceFile}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, fmt.Errorf("policy boot identity: %w", err)
	}
	g.state = Snapshot{AllowAI: true, BootID: hex.EncodeToString(id[:])}
	if dataDir == "" {
		return g, nil
	}
	g.path = filepath.Join(dataDir, "resource-policy.json")
	if info, err := os.Lstat(g.path); err == nil && !info.Mode().IsRegular() {
		g.state.AllowAI = false
		return g, fmt.Errorf("resource policy target is not a regular file")
	} else if err != nil && !os.IsNotExist(err) {
		g.state.AllowAI = false
		return g, fmt.Errorf("read resource policy target: %w", err)
	}
	data, err := os.ReadFile(g.path)
	if os.IsNotExist(err) {
		// A missing configured directory is not a fresh configured data directory.
		info, dirErr := os.Stat(dataDir)
		if dirErr == nil && info.IsDir() {
			return g, nil
		}
		if dirErr != nil {
			err = dirErr
		}
	}
	if err == nil {
		var fields map[string]json.RawMessage
		fields, err = DecodeObject(data, "version", "allow_ai", "revision")
		if err == nil {
			var version uint64
			err = json.Unmarshal(fields["version"], &version)
			if err == nil && version != 1 {
				err = fmt.Errorf("unsupported resource policy version")
			}
			if err == nil {
				err = json.Unmarshal(fields["allow_ai"], &g.state.AllowAI)
			}
			if err == nil {
				err = json.Unmarshal(fields["revision"], &g.state.Revision)
			}
		}
	}
	if err != nil {
		g.state.AllowAI = false
		return g, fmt.Errorf("read resource policy: %w", err)
	}
	g.state.Durable = true
	return g, nil
}

// DecodeObject accepts exactly the named, non-null fields once each. Callers
// validate field types; duplicate keys and trailing JSON are never last-wins.
func DecodeObject(data []byte, names ...string) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("expected JSON object")
	}
	fields := make(map[string]json.RawMessage, len(names))
	allowed := make(map[string]bool, len(names))
	for _, name := range names {
		allowed[name] = true
	}
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, ok := token.(string)
		if !ok || !allowed[name] {
			return nil, fmt.Errorf("unknown field %v", token)
		}
		if _, exists := fields[name]; exists {
			return nil, fmt.Errorf("duplicate field %s", name)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		if string(value) == "null" {
			return nil, fmt.Errorf("null field %s", name)
		}
		fields[name] = value
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing JSON")
	}
	if len(fields) != len(names) {
		return nil, fmt.Errorf("missing required field")
	}
	return fields, nil
}

func (g *Gate) snapshotLocked() Snapshot {
	s := g.state
	s.Admission, s.Release = "allowed", "not_requested"
	if !s.AllowAI {
		s.Admission, s.Release = "blocked", "unreleased_unknown"
	}
	return s
}
func (g *Gate) Snapshot() Snapshot { g.mu.Lock(); defer g.mu.Unlock(); return g.snapshotLocked() }
func (g *Gate) Active() uint64     { g.mu.Lock(); defer g.mu.Unlock(); return g.active }

// Update validates the optimistic precondition and closes admission before any
// I/O or writer wait. Resume also remains blocked until its write succeeds.
// A newer accepted pause invalidates a pending resume, including its reopen step.
func (g *Gate) Update(allow bool, boot string, revision uint64) (Snapshot, error) {
	g.mu.Lock()
	if boot != g.state.BootID || revision != g.state.Revision || revision == ^uint64(0) {
		s := g.snapshotLocked()
		g.mu.Unlock()
		return s, ErrConflict
	}
	g.state.Revision++
	current := g.state.Revision
	g.state.AllowAI, g.state.Durable = false, false
	g.epoch++
	g.mu.Unlock()

	g.writeMu.Lock()
	defer g.writeMu.Unlock()
	g.mu.Lock()
	if current != g.state.Revision {
		s := g.snapshotLocked()
		g.mu.Unlock()
		return s, ErrConflict
	}
	g.mu.Unlock()
	data, err := json.Marshal(struct {
		Version  uint64 `json:"version"`
		AllowAI  bool   `json:"allow_ai"`
		Revision uint64 `json:"revision"`
	}{1, allow, current})
	if err == nil && g.path != "" {
		err = g.persist(g.path, append(data, '\n'))
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if current != g.state.Revision {
		return g.snapshotLocked(), errors.Join(ErrConflict, err)
	}
	if err != nil {
		return g.snapshotLocked(), fmt.Errorf("persist resource policy: %w", err)
	}
	g.state.AllowAI, g.state.Durable = allow, g.path != ""
	return g.snapshotLocked(), nil
}

// Permit is a temporary activity reservation. Check at each late start, commit,
// or forward boundary; a pause invalidates old permits even after owner resume.
// No gate mutex is retained during caller work or I/O.
type Permit struct {
	gate   *Gate
	epoch  uint64
	closed bool
}

func (g *Gate) Acquire() (*Permit, error) {
	if g == nil {
		return nil, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.state.AllowAI {
		return nil, ErrPaused
	}
	g.active++
	return &Permit{gate: g, epoch: g.epoch}, nil
}
func (p *Permit) Check() error {
	if p == nil {
		return nil
	}
	g := p.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	if p.closed || !g.state.AllowAI || p.epoch != g.epoch {
		return ErrPaused
	}
	return nil
}
func (p *Permit) Close() {
	if p == nil {
		return
	}
	g := p.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	if !p.closed {
		p.closed = true
		g.active--
	}
}

func replaceFile(path string, data []byte) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("policy target is not a regular file")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".resource-policy-*.tmp")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if _, err := f.Write(data); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Go's Windows implementation uses replacement semantics. Sharing violations
	// are returned; never remove the old destination to force replacement.
	return os.Rename(temp, path)
}
