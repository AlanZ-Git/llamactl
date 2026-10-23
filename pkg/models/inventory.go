package models

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type InventoryCandidate struct {
	ID       string       `json:"candidate_id"`
	Name     string       `json:"name"`
	State    string       `json:"file_state"`
	Reason   string       `json:"reason,omitempty"`
	Metadata ggufMetadata `json:"metadata"`
	Members  []string     `json:"members"`
}
type InventoryScan struct {
	ID           string               `json:"scan_id"`
	RootID       string               `json:"root_id"`
	Filename     string               `json:"filename,omitempty"`
	Candidates   []InventoryCandidate `json:"candidates"`
	Partial      bool                 `json:"partial"`
	Continuation string               `json:"continuation,omitempty"`
	Next         *int                 `json:"next,omitempty"`
}
type ArtifactMember struct {
	Name         string `json:"name"`
	Role         string `json:"role"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
	FileID       string `json:"file_id"`
	MTime        int64  `json:"mtime"`
	HeaderDigest string `json:"header_digest"`
}
type Artifact struct {
	ID                     string           `json:"artifact_id"`
	RootID                 string           `json:"root_id"`
	Main                   string           `json:"relative_main"`
	Members                []ArtifactMember `json:"ordered_members"`
	Metadata               ggufMetadata     `json:"metadata"`
	State                  string           `json:"file_state"`
	ManifestDigest         string           `json:"manifest_digest"`
	IdentityLevel          string           `json:"identity_level"`
	ScanID                 string           `json:"last_scan_id"`
	ProjectorCompatibility string           `json:"projector_compatibility,omitempty"`
}
type scanFrame struct {
	file  *os.File
	name  string
	depth int
}
type scanCursor struct {
	root       string
	filename   string // Empty selects the existing recursive root scan.
	frames     []scanFrame
	candidates []InventoryCandidate
	created    time.Time
}
type Inventory struct {
	mu             sync.Mutex
	roots          map[string]string
	scans          map[string]InventoryScan
	cursors        map[string]*scanCursor
	busy           map[string]bool
	artifacts      map[string]Artifact
	entriesPerPass int
	hash           func(context.Context, string, string, string, bool) (ArtifactMember, error)
}

func inventoryID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func NewInventory(roots map[string]string) (*Inventory, error) {
	i := &Inventory{roots: map[string]string{}, scans: map[string]InventoryScan{}, cursors: map[string]*scanCursor{}, busy: map[string]bool{}, artifacts: map[string]Artifact{}, entriesPerPass: 1024, hash: hashMember}
	for id, root := range roots {
		if id == "" || strings.ContainsAny(id, "/\\.") || !filepath.IsAbs(root) {
			return nil, fmt.Errorf("invalid approved root")
		}
		clean := filepath.Clean(root)
		real, err := filepath.EvalSymlinks(clean)
		if err != nil || real != clean {
			return nil, fmt.Errorf("approved root must be canonical, without links")
		}
		info, err := os.Lstat(clean)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("invalid approved root")
		}
		i.roots[id] = clean
	}
	return i, nil
}
func (i *Inventory) Roots() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := []string{}
	for id := range i.roots {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// inventoryFilename accepts a single portable basename, never a path or stream.
// An empty selection preserves the full-root scan contract.
func inventoryFilename(selection []string) (string, error) {
	if len(selection) > 1 {
		return "", fmt.Errorf("invalid_filename")
	}
	if len(selection) == 0 || selection[0] == "" {
		return "", nil
	}
	name := selection[0]
	if strings.HasPrefix(name, ".") || strings.ContainsAny(name, "/\\:\x00") ||
		strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") || filepath.IsAbs(name) || filepath.Base(name) != name {
		return "", fmt.Errorf("invalid_filename")
	}
	return name, nil
}

func (i *Inventory) Scan(ctx context.Context, root string, selection ...string) (InventoryScan, error) {
	filename, err := inventoryFilename(selection)
	if err != nil {
		return InventoryScan{}, err
	}
	i.mu.Lock()
	path, ok := i.roots[root]
	if !ok {
		i.mu.Unlock()
		return InventoryScan{}, fmt.Errorf("unapproved_root")
	}
	if i.busy[root] {
		i.mu.Unlock()
		return InventoryScan{}, fmt.Errorf("scan_busy")
	}
	if len(i.scans) >= 128 {
		i.mu.Unlock()
		return InventoryScan{}, fmt.Errorf("scan_capacity")
	}
	i.busy[root] = true
	i.mu.Unlock()
	defer func() { i.mu.Lock(); delete(i.busy, root); i.mu.Unlock() }()
	f, err := safeInventoryOpen(path, ".")
	if err != nil {
		return InventoryScan{}, err
	}
	return i.scanPass(ctx, &scanCursor{root: root, filename: filename, frames: []scanFrame{{file: f, name: ".", depth: 0}}, created: time.Now()})
}
func (i *Inventory) Continue(ctx context.Context, id, token string, selection ...string) (InventoryScan, error) {
	filename, err := inventoryFilename(selection)
	if err != nil {
		return InventoryScan{}, err
	}
	i.mu.Lock()
	scan, ok := i.scans[id]
	c := i.cursors[token]
	if !ok || scan.Continuation != token || c == nil {
		i.mu.Unlock()
		return InventoryScan{}, fmt.Errorf("continuation_invalid")
	}
	if filename != "" && filename != c.filename {
		i.mu.Unlock()
		return InventoryScan{}, fmt.Errorf("continuation_selection_conflict")
	}
	if i.busy[c.root] {
		i.mu.Unlock()
		return InventoryScan{}, fmt.Errorf("scan_busy")
	}
	if len(i.scans) >= 128 {
		i.mu.Unlock()
		return InventoryScan{}, fmt.Errorf("scan_capacity")
	}
	delete(i.cursors, token)
	i.busy[c.root] = true
	i.mu.Unlock()
	defer func() { i.mu.Lock(); delete(i.busy, c.root); i.mu.Unlock() }()
	return i.scanPass(ctx, c)
}
func (i *Inventory) scanPass(ctx context.Context, c *scanCursor) (InventoryScan, error) {
	deadline := time.Now().Add(2 * time.Second)
	count := 0
	for len(c.frames) > 0 && count < i.entriesPerPass && time.Now().Before(deadline) && ctx.Err() == nil {
		top := c.frames[len(c.frames)-1]
		var names []string
		var err error
		if c.filename != "" {
			// Consume exactly the selected basename, without enumerating siblings.
			// The same safe-open, metadata, snapshot and Verify path handles it below.
			names = []string{c.filename}
			top.file.Close()
			c.frames = c.frames[:len(c.frames)-1]
		} else {
			names, err = top.file.Readdirnames(1)
		}
		if len(names) == 0 {
			top.file.Close()
			c.frames = c.frames[:len(c.frames)-1]
			if err != nil && err != io.EOF {
				c.candidates = append(c.candidates, InventoryCandidate{ID: inventoryID(), Name: top.name, State: "unreadable", Reason: "directory_read_failed"})
			}
			continue
		}
		count++
		name := filepath.Join(top.name, names[0])
		candidate := InventoryCandidate{ID: inventoryID(), Name: filepath.ToSlash(name), Members: []string{filepath.ToSlash(name)}}
		info, e := os.Lstat(filepath.Join(i.roots[c.root], name))
		if e != nil {
			candidate.State = "unreadable"
			candidate.Reason = "stat_failed"
			c.candidates = append(c.candidates, candidate)
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			candidate.State = "blocked_link"
			c.candidates = append(c.candidates, candidate)
			continue
		}
		f, e := safeInventoryOpen(i.roots[c.root], name)
		if e != nil {
			candidate.State = "blocked_link"
			candidate.Reason = "safe_open_failed"
			c.candidates = append(c.candidates, candidate)
			continue
		}
		if info.IsDir() {
			if c.filename != "" {
				f.Close()
				return InventoryScan{}, fmt.Errorf("invalid_filename")
			}
			if top.depth >= 16 {
				f.Close()
				candidate.State = "unsupported"
				candidate.Reason = "depth_limit"
				c.candidates = append(c.candidates, candidate)
				continue
			}
			c.frames = append(c.frames, scanFrame{file: f, name: name, depth: top.depth + 1})
			continue
		}
		if !info.Mode().IsRegular() {
			f.Close()
			candidate.State = "unsupported"
			candidate.Reason = "not_regular"
			c.candidates = append(c.candidates, candidate)
			continue
		}
		lower := strings.ToLower(name)
		switch {
		case strings.HasSuffix(lower, ".part") || strings.HasSuffix(lower, ".tmp") || strings.HasSuffix(lower, ".download"):
			candidate.State = "copying"
		case !strings.HasSuffix(lower, ".gguf"):
			candidate.State = "unsupported"
			candidate.Reason = "not_gguf"
		default:
			candidate.Metadata, e = parseGGUF(f)
			switch {
			case errors.Is(e, errGGUFUnsupported):
				candidate.State = "unknown_metadata"
			case e != nil:
				candidate.State = "invalid"
				candidate.Reason = "gguf_structure_invalid"
			case candidate.Metadata.Architecture == "clip":
				candidate.State = "needs_association"
			case !knownArchitecture(candidate.Metadata.Architecture):
				candidate.State = "unknown_metadata"
			default:
				candidate.State = "discovered"
			}
		}
		f.Close()
		c.candidates = append(c.candidates, candidate)
		if len(c.candidates) >= 100000 {
			for _, f := range c.frames {
				f.file.Close()
			}
			c.frames = nil
			return InventoryScan{}, fmt.Errorf("scan_capacity")
		}
	}
	snapshot := InventoryScan{ID: inventoryID(), RootID: c.root, Filename: c.filename, Candidates: append([]InventoryCandidate(nil), c.candidates...), Partial: len(c.frames) > 0}
	if snapshot.Partial {
		snapshot.Continuation = inventoryID()
	}
	groupInventorySplits(snapshot.Candidates, snapshot.Partial)
	sort.Slice(snapshot.Candidates, func(a, b int) bool { return snapshot.Candidates[a].Name < snapshot.Candidates[b].Name })
	i.mu.Lock()
	defer i.mu.Unlock()
	i.scans[snapshot.ID] = snapshot
	if snapshot.Partial {
		i.cursors[snapshot.Continuation] = c
	}
	return cloneScan(snapshot), nil
}
func knownArchitecture(s string) bool {
	switch s {
	case "llama", "qwen2", "qwen3", "qwen3moe", "qwen35", "qwen35moe", "gemma", "gemma2", "gemma3", "mistral", "phi3", "deepseek2":
		return true
	}
	return false
}

var inventorySplit = regexp.MustCompile(`^(.*)-(\d{5})-of-(\d{5})\.gguf$`)

func groupInventorySplits(cs []InventoryCandidate, partial bool) {
	// Validate against the unmodified snapshot, not state written for earlier members.
	original := append([]InventoryCandidate(nil), cs...)
	byName := map[string]int{}
	for n, c := range cs {
		byName[c.Name] = n
	}
	for n := range cs {
		c := &cs[n]
		if c.State != "discovered" {
			continue
		}
		match := inventorySplit.FindStringSubmatch(c.Name)
		if c.Metadata.SplitCount == 0 {
			if match != nil {
				c.State = "ambiguous"
				c.Reason = "filename_metadata_disagree"
			}
			continue
		}
		if match == nil {
			c.State = "ambiguous"
			c.Reason = "split_filename_required"
			continue
		}
		index, _ := strconv.Atoi(match[2])
		total, _ := strconv.Atoi(match[3])
		if uint64(index) != c.Metadata.SplitNo+1 || uint64(total) != c.Metadata.SplitCount {
			c.State = "ambiguous"
			continue
		}
		members := []string{}
		tensorNames := map[string]bool{}
		sum := uint64(0)
		for k := 1; k <= total; k++ {
			name := fmt.Sprintf("%s-%05d-of-%05d.gguf", match[1], k, total)
			other, ok := byName[name]
			if !ok {
				c.State = "incomplete"
				break
			}
			m := original[other].Metadata
			if original[other].State != "discovered" || m.EmbeddingLength != c.Metadata.EmbeddingLength || m.Architecture != c.Metadata.Architecture || m.Name != c.Metadata.Name || m.SplitCount != c.Metadata.SplitCount || m.SplitNo != uint64(k-1) || m.SplitTensors != c.Metadata.SplitTensors {
				c.State = "ambiguous"
				break
			}
			for _, name := range m.TensorNames {
				if tensorNames[name] {
					c.State = "ambiguous"
					c.Reason = "duplicate_split_tensor"
				}
				tensorNames[name] = true
			}
			members = append(members, name)
			sum += m.Tensors
		}
		if c.State != "discovered" {
			continue
		}
		if sum != c.Metadata.SplitTensors {
			c.State = "ambiguous"
			continue
		}
		if partial {
			c.Reason = "scan_partial"
		}
		c.Members = members
		if index != 1 {
			c.State = "split_member"
		}
	}
}
func cloneScan(s InventoryScan) InventoryScan {
	b, _ := json.Marshal(s)
	var out InventoryScan
	_ = json.Unmarshal(b, &out)
	return out
}
func (i *Inventory) Page(id string, offset, limit int) (InventoryScan, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	s, ok := i.scans[id]
	if !ok {
		return s, fmt.Errorf("scan_not_found")
	}
	if offset < 0 || offset > len(s.Candidates) || limit < 1 || limit > 1000 {
		return s, fmt.Errorf("invalid_page")
	}
	end := min(offset+limit, len(s.Candidates))
	s.Candidates = s.Candidates[offset:end]
	if end < len(i.scans[id].Candidates) {
		s.Next = &end
	}
	return cloneScan(s), nil
}
func hashMember(ctx context.Context, root, name, role string, full bool) (ArtifactMember, error) {
	m := ArtifactMember{Name: name, Role: role}
	f, err := safeInventoryOpen(root, filepath.FromSlash(name))
	if err != nil {
		return m, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() {
		return m, fmt.Errorf("not_regular")
	}
	m.Size = before.Size()
	m.MTime = before.ModTime().UnixNano()
	m.FileID = inventoryFileID(f, before)
	if m.FileID == "unknown" || m.FileID == "unsupported" {
		return m, fmt.Errorf("file_identity_unavailable")
	}
	h := sha256.New()
	if _, err = io.Copy(h, io.LimitReader(f, 4096)); err != nil {
		return m, err
	}
	m.HeaderDigest = hex.EncodeToString(h.Sum(nil))
	if full {
		if _, err = parseGGUF(f); err != nil {
			return m, err
		}
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			return m, err
		}
		h.Reset()
		buf := make([]byte, 1<<20)
		for {
			if err = ctx.Err(); err != nil {
				return m, err
			}
			n, e := f.Read(buf)
			if n > 0 {
				h.Write(buf[:n])
			}
			if e == io.EOF {
				break
			}
			if e != nil {
				return m, e
			}
		}
		m.SHA256 = hex.EncodeToString(h.Sum(nil))
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return m, fmt.Errorf("copying")
	}
	path, err := safeInventoryOpen(root, filepath.FromSlash(name))
	if err != nil {
		return m, err
	}
	defer path.Close()
	current, err := path.Stat()
	if err != nil || !os.SameFile(before, current) {
		return m, fmt.Errorf("changed")
	}
	return m, nil
}
func (i *Inventory) Verify(ctx context.Context, scanID, candidateID, projectorID string) (Artifact, error) {
	i.mu.Lock()
	s, ok := i.scans[scanID]
	var c, p InventoryCandidate
	for _, v := range s.Candidates {
		if v.ID == candidateID {
			c = v
		}
		if v.ID == projectorID {
			p = v
		}
	}
	root := i.roots[s.RootID]
	old := i.artifacts[s.RootID+"/"+c.Name]
	i.mu.Unlock()
	if !ok || c.ID == "" || c.State != "discovered" {
		return Artifact{}, fmt.Errorf("candidate_not_verifiable")
	}
	if projectorID != "" && (p.ID == "" || p.State != "needs_association") {
		return Artifact{}, fmt.Errorf("projector_association_invalid")
	}
	if projectorID != "" && (p.Metadata.SplitCount != 0 || (c.Metadata.EmbeddingLength != 0 && p.Metadata.ProjectionDim != 0 && c.Metadata.EmbeddingLength != p.Metadata.ProjectionDim)) {
		return Artifact{}, fmt.Errorf("projector_metadata_incompatible")
	}
	a := Artifact{ID: old.ID, RootID: s.RootID, Main: c.Name, Metadata: c.Metadata, State: "complete", IdentityLevel: "verified", ScanID: scanID}
	if projectorID != "" {
		// Hash identity is verified; metadata consistency is not a fixed-engine
		// multimodal compatibility probe. Do not publish or execute this association.
		a.ProjectorCompatibility = "unverified"
	}
	if a.ID == "" {
		a.ID = inventoryID()
	}
	members := append([]string(nil), c.Members...)
	if projectorID != "" {
		members = append(members, p.Name)
	}
	same := old.IdentityLevel == "verified" && len(old.Members) == len(members)
	if same {
		for idx, name := range members {
			if old.Members[idx].Name != name {
				same = false
				break
			}
		}
	}
	if same && i.Check(ctx, old) == nil {
		old.ScanID = scanID
		old.ProjectorCompatibility = a.ProjectorCompatibility
		i.Remember(old)
		return old, nil
	}
	for n, name := range members {
		role := "model"
		if n >= len(c.Members) {
			role = "projector"
		}
		m, err := i.hash(ctx, root, name, role, true)
		if err != nil {
			return Artifact{}, err
		}
		a.Members = append(a.Members, m)
		f, err := safeInventoryOpen(root, filepath.FromSlash(name))
		if err != nil {
			return Artifact{}, err
		}
		current, err := parseGGUF(f)
		f.Close()
		if err != nil {
			return Artifact{}, err
		}
		matched := false
		for _, frozen := range s.Candidates {
			if frozen.Name == name {
				matched = reflect.DeepEqual(current, frozen.Metadata)
				break
			}
		}
		if !matched {
			return Artifact{}, fmt.Errorf("scan_metadata_changed")
		}
	}
	if err := i.Check(ctx, a); err != nil {
		return Artifact{}, err
	}
	type identity struct {
		Name string `json:"name"`
		Role string `json:"role"`
		Size int64  `json:"size"`
		SHA  string `json:"sha256"`
	}
	ids := []identity{}
	for _, m := range a.Members {
		ids = append(ids, identity{m.Name, m.Role, m.Size, m.SHA256})
	}
	b, _ := json.Marshal(ids)
	digest := sha256.Sum256(b)
	a.ManifestDigest = hex.EncodeToString(digest[:])
	i.mu.Lock()
	i.artifacts[a.RootID+"/"+a.Main] = a
	i.mu.Unlock()
	return a, nil
}
func (i *Inventory) Check(ctx context.Context, a Artifact) error {
	i.mu.Lock()
	root, ok := i.roots[a.RootID]
	i.mu.Unlock()
	if !ok || a.IdentityLevel != "verified" || len(a.Members) == 0 {
		return fmt.Errorf("unresolved")
	}
	for _, old := range a.Members {
		m, err := hashMember(ctx, root, old.Name, old.Role, false)
		if err != nil {
			return err
		}
		if m.FileID != old.FileID || m.MTime != old.MTime || m.Size != old.Size || m.HeaderDigest != old.HeaderDigest {
			return fmt.Errorf("changed")
		}
	}
	return nil
}
func (i *Inventory) Resolve(ctx context.Context, a Artifact) (string, string, error) {
	for _, m := range a.Members {
		if m.Role == "projector" {
			return "", "", fmt.Errorf("projector_engine_compatibility_unverified")
		}
	}
	if err := i.Check(ctx, a); err != nil {
		return "", "", err
	}
	i.mu.Lock()
	root := i.roots[a.RootID]
	i.mu.Unlock()
	projector := ""
	for _, m := range a.Members {
		if m.Role == "projector" {
			projector = filepath.Join(root, filepath.FromSlash(m.Name))
		}
	}
	return filepath.Join(root, filepath.FromSlash(a.Main)), projector, nil
}

// Remember hydrates a cache from SQLite; it grants nothing without Check.
func (i *Inventory) Remember(a Artifact) {
	i.mu.Lock()
	defer i.mu.Unlock()
	a.Members = append([]ArtifactMember(nil), a.Members...)
	i.artifacts[a.RootID+"/"+a.Main] = a
}
func (i *Inventory) Close() {
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, c := range i.cursors {
		for _, f := range c.frames {
			f.file.Close()
		}
	}
	i.cursors = map[string]*scanCursor{}
}
