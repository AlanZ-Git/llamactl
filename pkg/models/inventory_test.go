package models

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func n1GGUF(arch string) []byte {
	var b bytes.Buffer
	put := func(v any) {
		if err := binary.Write(&b, binary.LittleEndian, v); err != nil {
			panic(err)
		}
	}
	str := func(s string) { put(uint64(len(s))); b.WriteString(s) }
	b.WriteString("GGUF")
	put(uint32(3))
	put(uint64(1))
	put(uint64(1))
	str("general.architecture")
	put(uint32(8))
	str(arch)
	str("weight")
	put(uint32(1))
	put(uint64(1))
	put(uint32(0))
	put(uint64(0))
	for b.Len()%32 != 0 {
		b.WriteByte(0)
	}
	put(float32(1))
	return b.Bytes()
}
func TestN1InventoryFrozenUnionVerifyAndDrift(t *testing.T) {
	root := t.TempDir()
	for name, body := range map[string][]byte{"good.gguf": n1GGUF("llama"), "bad.gguf": []byte("GGUF"), "unknown.gguf": n1GGUF("future"), "mmproj.gguf": n1GGUF("clip"), "copy.gguf.part": []byte("partial")} {
		if err := os.WriteFile(filepath.Join(root, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	inv, err := NewInventory(map[string]string{"approved": root})
	if err != nil {
		t.Fatal(err)
	}
	s, err := inv.Scan(context.Background(), "approved")
	if err != nil {
		t.Fatal(err)
	}
	if s.Partial {
		t.Fatal("small scan partial")
	}
	seen := map[string]string{}
	for offset := 0; ; {
		p, err := inv.Page(s.ID, offset, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range p.Candidates {
			if _, ok := seen[c.Name]; ok {
				t.Fatal("duplicate")
			}
			seen[c.Name] = c.State
		}
		if p.Next == nil {
			break
		}
		offset = *p.Next
	}
	if len(seen) != 5 || seen["bad.gguf"] != "invalid" || seen["unknown.gguf"] != "unknown_metadata" || seen["mmproj.gguf"] != "needs_association" || seen["copy.gguf.part"] != "copying" {
		t.Fatalf("union: %v", seen)
	}
	var id string
	for _, c := range s.Candidates {
		if c.Name == "good.gguf" {
			id = c.ID
		}
	}
	a, err := inv.Verify(context.Background(), s.ID, id, "")
	if err != nil || a.IdentityLevel != "verified" {
		t.Fatalf("verify: %+v %v", a, err)
	}
	if err = inv.Check(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "good.gguf"), n1GGUF("qwen2"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = inv.Check(context.Background(), a); err == nil {
		t.Fatal("drift accepted")
	}
	p, _ := inv.Page(s.ID, 0, 100)
	if p.Candidates[0].State != s.Candidates[0].State {
		t.Fatal("snapshot mutated")
	}
	if _, err = inv.Scan(context.Background(), "../approved"); err == nil {
		t.Fatal("unapproved root")
	}
}
func TestN1InventoryRejectsLinksTruncationAndPartial(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "real.gguf"), n1GGUF("llama"), 0600)
	if err := os.Symlink("real.gguf", filepath.Join(root, "link.gguf")); err != nil {
		t.Skip(err)
	}
	b := n1GGUF("llama")
	os.WriteFile(filepath.Join(root, "truncated.gguf"), b[:24], 0600) // Metadata, not weights, is truncated.
	inv, err := NewInventory(map[string]string{"r": root})
	if err != nil {
		t.Fatal(err)
	}
	s, err := inv.Scan(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, c := range s.Candidates {
		states[c.Name] = c.State
	}
	if states["link.gguf"] != "blocked_link" || states["truncated.gguf"] != "invalid" {
		t.Fatal(states)
	}
	if _, err = safeInventoryOpen(root, "../outside"); err == nil {
		t.Fatal("traversal accepted")
	}
	inv.entriesPerPass = 1
	partial, err := inv.Scan(context.Background(), "r")
	if err != nil || !partial.Partial || partial.Continuation == "" {
		t.Fatalf("partial: %+v %v", partial, err)
	}
	continued, err := inv.Continue(context.Background(), partial.ID, partial.Continuation)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := inv.Page(partial.ID, 0, 100)
	if len(old.Candidates) != len(partial.Candidates) {
		t.Fatal("old page changed")
	}
	if continued.ID == partial.ID {
		t.Fatal("continuation changed frozen snapshot")
	}
}

func TestN1InventoryTargetedScan(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for n := 0; n < 25; n++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("other-%02d.gguf", n)), []byte("malformed sibling"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	body := n1GGUF("llama")
	if err := os.WriteFile(filepath.Join(root, "target.gguf"), body, 0600); err != nil {
		t.Fatal(err)
	}
	inv, err := NewInventory(map[string]string{"r": root})
	if err != nil {
		t.Fatal(err)
	}
	defer inv.Close()
	// A filtered full-root scan cannot complete in this one-entry budget.
	inv.entriesPerPass = 1
	s, err := inv.Scan(ctx, "r", "target.gguf")
	if err != nil || s.Partial || s.Continuation != "" || s.Filename != "target.gguf" || len(s.Candidates) != 1 {
		t.Fatalf("target snapshot: %+v %v", s, err)
	}
	c := s.Candidates[0]
	if c.Name != "target.gguf" || c.State != "discovered" || len(c.Members) != 1 || c.Members[0] != c.Name {
		t.Fatalf("target: %+v", c)
	}
	a, err := inv.Verify(ctx, s.ID, c.ID, "")
	if err != nil || a.IdentityLevel != "verified" || len(a.Members) != 1 || a.Main != c.Name {
		t.Fatalf("target verify: %+v %v", a, err)
	}
	digest := sha256.Sum256(body)
	if a.Members[0].SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatal("wrong identity")
	}
	page, err := inv.Page(s.ID, 0, 1)
	if err != nil || page.Next != nil || page.Filename != s.Filename || len(page.Candidates) != 1 || page.Candidates[0].ID != c.ID {
		t.Fatalf("target page: %+v %v", page, err)
	}
	inv.entriesPerPass = 1024
	for _, selection := range [][]string{nil, {""}} {
		full, err := inv.Scan(ctx, "r", selection...)
		if err != nil || full.Partial || full.Filename != "" || len(full.Candidates) != 26 {
			t.Fatalf("full-root scan changed: %+v %v", full, err)
		}
	}
	if err = os.WriteFile(filepath.Join(root, "target.gguf"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	frozen, err := inv.Page(s.ID, 0, 1)
	if err != nil || frozen.Candidates[0].State != "discovered" {
		t.Fatal("snapshot not frozen", err)
	}
	if _, _, err = inv.Resolve(ctx, a); err == nil {
		t.Fatal("target identity drift accepted")
	}
}

func TestN1InventoryTargetedNegatives(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	files := map[string][]byte{
		"valid.gguf":               n1GGUF("llama"),
		"broken.gguf":              []byte("GGUF"),
		"future.gguf":              n1GGUF("future"),
		"copy.gguf.part":           []byte("copying"),
		"plain.txt":                []byte("not GGUF"),
		"pair-00001-of-00002.gguf": splitGGUF("llama", "pair", "a", 0, 2, 1, 0, nil),
		"pair-00002-of-00002.gguf": splitGGUF("llama", "pair", "b", 1, 2, 1, 0, nil),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "directory.gguf"), 0700); err != nil {
		t.Fatal(err)
	}
	inv, err := NewInventory(map[string]string{"r": root})
	if err != nil {
		t.Fatal(err)
	}
	defer inv.Close()
	for _, name := range []string{".", "..", "../valid.gguf", "dir/valid.gguf", `dir\valid.gguf`, "/valid.gguf", `C:\valid.gguf`, "C:valid.gguf", "valid.gguf:stream", ".hidden.gguf", "valid.gguf.", "valid.gguf ", "nul\x00.gguf", "directory.gguf"} {
		if _, err := inv.Scan(ctx, "r", name); err == nil || err.Error() != "invalid_filename" {
			t.Errorf("unsafe selection %q: %v", name, err)
		}
	}
	states := map[string]string{"absent.gguf": "unreadable", "broken.gguf": "invalid", "future.gguf": "unknown_metadata", "copy.gguf.part": "copying", "plain.txt": "unsupported", "pair-00001-of-00002.gguf": "incomplete"}
	for name, state := range states {
		s, err := inv.Scan(ctx, "r", name)
		if err != nil || s.Partial || len(s.Candidates) != 1 {
			t.Fatalf("%s: %+v %v", name, s, err)
		}
		c := s.Candidates[0]
		if c.Name != name || c.State != state {
			t.Fatalf("%s: %+v", name, c)
		}
		if name == "absent.gguf" && c.Reason != "stat_failed" {
			t.Fatal("missing target hidden", c)
		}
		if _, err = inv.Verify(ctx, s.ID, c.ID, ""); err == nil {
			t.Fatal("unsafe target verified", c)
		}
	}
	// An available sibling must NOT trigger implicit split discovery.
	full, err := inv.Scan(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range full.Candidates {
		if c.Name == "pair-00001-of-00002.gguf" {
			found = c.State == "discovered" && len(c.Members) == 2
		}
	}
	if !found {
		t.Fatal("full-root split discovery regressed")
	}
	t.Run("links", func(t *testing.T) {
		for _, name := range []string{"valid.gguf", "absent.gguf", "directory.gguf"} {
			link := "link-" + name
			if err := os.Symlink(name, filepath.Join(root, link)); err != nil {
				t.Skip(err)
			}
			s, err := inv.Scan(ctx, "r", link)
			if err != nil || len(s.Candidates) != 1 || s.Candidates[0].State != "blocked_link" {
				t.Fatalf("link: %+v %v", s, err)
			}
			if _, err = inv.Verify(ctx, s.ID, s.Candidates[0].ID, ""); err == nil {
				t.Fatal("link verified")
			}
		}
	})
}

func TestN1InventoryTargetedContinuationSelection(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for _, name := range []string{"one.gguf", "two.gguf"} {
		if err := os.WriteFile(filepath.Join(root, name), n1GGUF("llama"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	inv, err := NewInventory(map[string]string{"r": root})
	if err != nil {
		t.Fatal(err)
	}
	defer inv.Close()
	for _, selection := range []string{"", "one.gguf"} {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		start, err := inv.Scan(cancelled, "r", selection)
		if err != nil || !start.Partial || len(start.Candidates) != 0 {
			t.Fatalf("partial: %+v %v", start, err)
		}
		if _, err = inv.Continue(ctx, start.ID, start.Continuation, "two.gguf"); err == nil || err.Error() != "continuation_selection_conflict" {
			t.Fatal("changed continuation selection", err)
		}
		// Rejection must not consume the cursor. Empty keeps its original selection.
		next, err := inv.Continue(ctx, start.ID, start.Continuation, "")
		want := 2
		if selection != "" {
			want = 1
		}
		if err != nil || next.Partial || next.Filename != selection || len(next.Candidates) != want {
			t.Fatalf("continue: %+v %v", next, err)
		}
		if selection != "" && next.Candidates[0].Name != selection {
			t.Fatal("target changed")
		}
		old, err := inv.Page(start.ID, 0, 100)
		if err != nil || len(old.Candidates) != 0 || !old.Partial {
			t.Fatal("old partial changed", err)
		}
		if _, err = inv.Continue(ctx, start.ID, start.Continuation); err == nil {
			t.Fatal("cursor replayed")
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	start, err := inv.Scan(cancelled, "r", "one.gguf")
	if err != nil {
		t.Fatal(err)
	}
	next, err := inv.Continue(ctx, start.ID, start.Continuation, "one.gguf")
	if err != nil || len(next.Candidates) != 1 || next.Candidates[0].Name != "one.gguf" {
		t.Fatalf("same selection: %+v %v", next, err)
	}
}

func TestN1InventoryMalformedMetadataBounds(t *testing.T) {
	base := n1GGUF("llama")
	for _, variant := range []string{"magic", "version", "tensor_count", "kv_count", "string_limit", "duplicate_key", "truncated_descriptor", "truncated_offset"} {
		t.Run(variant, func(t *testing.T) {
			body := append([]byte(nil), base...)
			state := "invalid"
			tensorStart := bytes.Index(body, []byte("weight")) - 8
			switch variant {
			case "magic":
				body[0] = 'X'
			case "version":
				binary.LittleEndian.PutUint32(body[4:8], 99)
				state = "unknown_metadata"
			case "tensor_count":
				binary.LittleEndian.PutUint64(body[8:16], 100001)
			case "kv_count":
				binary.LittleEndian.PutUint64(body[16:24], 65537)
			case "string_limit":
				binary.LittleEndian.PutUint64(body[24:32], 1<<20+1)
			case "duplicate_key":
				entry := append([]byte(nil), body[24:tensorStart]...)
				body = append(append(append([]byte(nil), body[:tensorStart]...), entry...), body[tensorStart:]...)
				binary.LittleEndian.PutUint64(body[16:24], 2)
			case "truncated_descriptor":
				body = body[:tensorStart+8+2]
			case "truncated_offset":
				body = body[:tensorStart+8+6+4+8+4+7]
			}
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "bad.gguf"), body, 0600); err != nil {
				t.Fatal(err)
			}
			inv, err := NewInventory(map[string]string{"r": root})
			if err != nil {
				t.Fatal(err)
			}
			defer inv.Close()
			s, err := inv.Scan(context.Background(), "r", "bad.gguf")
			if err != nil || len(s.Candidates) != 1 || s.Candidates[0].State != state {
				t.Fatalf("metadata: %+v %v", s, err)
			}
			if _, err = inv.Verify(context.Background(), s.ID, s.Candidates[0].ID, ""); err == nil {
				t.Fatal("malformed metadata verified")
			}
		})
	}
}

type n1IQ2Tensor struct {
	name   string
	dims   []uint64
	typ    uint32
	offset uint64
}

// Synthetic bounded descriptors and payload lengths; no engine or geometry table.
func n1IQ2GGUF(tensors []n1IQ2Tensor, dataSize int) []byte {
	var b bytes.Buffer
	put := func(v any) {
		if err := binary.Write(&b, binary.LittleEndian, v); err != nil {
			panic(err)
		}
	}
	str := func(s string) { put(uint64(len(s))); b.WriteString(s) }
	b.WriteString("GGUF")
	put(uint32(3))
	put(uint64(len(tensors)))
	put(uint64(1))
	str("general.architecture")
	put(uint32(8))
	str("llama")
	for _, tensor := range tensors {
		str(tensor.name)
		put(uint32(len(tensor.dims)))
		for _, dim := range tensor.dims {
			put(dim)
		}
		put(tensor.typ)
		put(tensor.offset)
	}
	for b.Len()%32 != 0 {
		b.WriteByte(0)
	}
	b.Write(make([]byte, dataSize))
	return b.Bytes()
}

func TestN1InventoryIdentityOnly(t *testing.T) {
	for _, tc := range []struct {
		name    string
		typ     uint32
		payload int
	}{
		{"unknown_quantization", 0xffffffff, 4},
		{"truncated_weight_tail", 0, 3},
		{"unknown_without_weights", 0xffffffff, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			body := n1IQ2GGUF([]n1IQ2Tensor{{"weight", []uint64{1}, tc.typ, 0}}, tc.payload)
			path := filepath.Join(root, "identity.gguf")
			if err := os.WriteFile(path, body, 0600); err != nil {
				t.Fatal(err)
			}
			inv, err := NewInventory(map[string]string{"r": root})
			if err != nil {
				t.Fatal(err)
			}
			defer inv.Close()
			hashes := 0
			inv.hash = func(ctx context.Context, root, name, role string, full bool) (ArtifactMember, error) {
				if full {
					hashes++
				}
				return hashMember(ctx, root, name, role, full)
			}
			scan, err := inv.Scan(ctx, "r")
			if err != nil || len(scan.Candidates) != 1 {
				t.Fatalf("scan: %+v %v", scan, err)
			}
			c := scan.Candidates[0]
			if c.State != "discovered" {
				t.Fatalf("identity-only candidate: %+v", c)
			}
			a, err := inv.Verify(ctx, scan.ID, c.ID, "")
			if err != nil || a.IdentityLevel != "verified" || a.State != "complete" {
				t.Fatalf("identity: %+v %v", a, err)
			}
			digest := sha256.Sum256(body)
			if len(a.Members) != 1 || a.Members[0].SHA256 != hex.EncodeToString(digest[:]) || a.Members[0].Size != int64(len(body)) {
				t.Fatalf("member: %+v", a.Members)
			}
			cached, err := inv.Verify(ctx, scan.ID, c.ID, "")
			if err != nil || cached.ID != a.ID || cached.ManifestDigest != a.ManifestDigest || hashes != 1 {
				t.Fatalf("cache: %+v %v hashes=%d", cached, err, hashes)
			}
			main, projector, err := inv.Resolve(ctx, cached)
			if err != nil || main != path || projector != "" {
				t.Fatalf("resolve: %q %q %v", main, projector, err)
			}
			if err = os.WriteFile(path, append(body, 0), 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err = inv.Resolve(ctx, cached); err == nil {
				t.Fatal("changed payload identity resolved")
			}
		})
	}
}

func TestN1InventoryPayloadGeometryIsEngineOwned(t *testing.T) {
	// All bounded descriptors below are identity-readable. Neither payload length,
	// quantization support nor offsets establish engine loadability at inventory time.
	iq2 := n1IQ2Tensor{"quantized", []uint64{512, 2}, 22, 0}
	mixed := []n1IQ2Tensor{iq2, {"float", []uint64{1}, 0, 352}, {"q4", []uint64{64}, 2, 384}}
	for _, tc := range []struct {
		name        string
		tensors     []n1IQ2Tensor
		dataSize    int
		parseError  string
		unsupported bool
		state       string
	}{
		{"complete_multiblock", []n1IQ2Tensor{iq2}, 328, "", false, "discovered"},
		{"one_byte_truncated", []n1IQ2Tensor{iq2}, 327, "", false, "discovered"},
		{"one_block_truncated", []n1IQ2Tensor{iq2}, 246, "", false, "discovered"},
		{"non_block_dimension", []n1IQ2Tensor{{"quantized", []uint64{128, 8}, 22, 0}}, 328, "", false, "discovered"},
		{"mixed_legal_types", mixed, 420, "", false, "discovered"},
		{"mixed_tail_truncated", mixed, 419, "", false, "discovered"},
		{"unknown_type", []n1IQ2Tensor{{"unknown", []uint64{256}, 23, 0}}, 82, "", false, "discovered"},
		{"mixed_unknown_type", []n1IQ2Tensor{iq2, {"unknown", []uint64{256}, 23, 352}}, 434, "", false, "discovered"},
		{"unaligned_offset", []n1IQ2Tensor{{"quantized", []uint64{512, 2}, 22, 1}}, 329, "", false, "discovered"},
		{"offset_out_of_bounds", []n1IQ2Tensor{{"quantized", []uint64{512, 2}, 22, ^uint64(0) - 31}}, 328, "", false, "discovered"},
		{"zero_dimension", []n1IQ2Tensor{{"weight", []uint64{0}, 0, 0}}, 4, "tensor size", false, "invalid"},
		{"dimension_limit", []n1IQ2Tensor{{"weight", []uint64{1<<40 + 1}, 0, 0}}, 4, "tensor size", false, "invalid"},
		{"dimension_product_limit", []n1IQ2Tensor{{"weight", []uint64{1 << 40, 1 << 40}, 0, 0}}, 4, "tensor size", false, "invalid"},
		{"no_dimensions", []n1IQ2Tensor{{"weight", nil, 0, 0}}, 4, "tensor dimensions", false, "invalid"},
		{"too_many_dimensions", []n1IQ2Tensor{{"weight", []uint64{1, 1, 1, 1, 1}, 0, 0}}, 4, "tensor dimensions", false, "invalid"},
		{"empty_tensor_name", []n1IQ2Tensor{{"", []uint64{1}, 0, 0}}, 4, "tensor name", false, "invalid"},
		{"duplicate_tensor_name", []n1IQ2Tensor{iq2, iq2}, 328, "tensor name", false, "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			body := n1IQ2GGUF(tc.tensors, tc.dataSize)
			path := filepath.Join(root, "synthetic.gguf")
			if err := os.WriteFile(path, body, 0600); err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			meta, err := parseGGUF(f)
			f.Close()
			if errors.Is(err, errGGUFUnsupported) != tc.unsupported {
				t.Errorf("unsupported = %v, want %v: %v", errors.Is(err, errGGUFUnsupported), tc.unsupported, err)
			}
			if tc.parseError == "" {
				if err != nil || meta.Architecture != "llama" || meta.Tensors != uint64(len(tc.tensors)) {
					t.Errorf("parse: %+v %v", meta, err)
				}
			} else if err == nil || err.Error() != tc.parseError {
				t.Errorf("parse error = %v, want %q", err, tc.parseError)
			}

			inv, err := NewInventory(map[string]string{"r": root})
			if err != nil {
				t.Fatal(err)
			}
			defer inv.Close()
			fullHashes := 0
			inv.hash = func(ctx context.Context, root, name, role string, full bool) (ArtifactMember, error) {
				if full {
					fullHashes++
				}
				return hashMember(ctx, root, name, role, full)
			}
			scan, err := inv.Scan(ctx, "r")
			if err != nil || scan.Partial || len(scan.Candidates) != 1 {
				t.Fatalf("scan: %+v %v", scan, err)
			}
			candidate := scan.Candidates[0]
			if candidate.State != tc.state {
				t.Errorf("candidate state = %q, want %q", candidate.State, tc.state)
			}
			if tc.state == "invalid" && candidate.Reason != "gguf_structure_invalid" {
				t.Errorf("invalid candidate reason = %q", candidate.Reason)
			}
			a, err := inv.Verify(ctx, scan.ID, candidate.ID, "")
			if tc.state != "discovered" {
				if err == nil || err.Error() != "candidate_not_verifiable" || a.IdentityLevel == "verified" || fullHashes != 0 {
					t.Fatalf("rejected candidate verified/hashed: %+v %v hashes=%d", a, err, fullHashes)
				}
				return
			}
			if err != nil || a.State != "complete" || a.IdentityLevel != "verified" || len(a.Members) != 1 {
				t.Fatalf("verify: %+v %v", a, err)
			}
			digest := sha256.Sum256(body)
			if a.Members[0].SHA256 != hex.EncodeToString(digest[:]) || a.Members[0].Size != int64(len(body)) {
				t.Fatalf("synthetic member identity: %+v", a.Members[0])
			}
			cached, err := inv.Verify(ctx, scan.ID, candidate.ID, "")
			if err != nil || cached.ID != a.ID || cached.ManifestDigest != a.ManifestDigest || cached.IdentityLevel != "verified" || fullHashes != 1 {
				t.Fatalf("cache: %+v %v hashes=%d", cached, err, fullHashes)
			}
			if err = inv.Check(ctx, cached); err != nil {
				t.Fatal(err)
			}
			cached.Members = append([]ArtifactMember(nil), cached.Members...)
			cached.Members[0].HeaderDigest = "changed"
			if err = inv.Check(ctx, cached); err == nil || err.Error() != "changed" {
				t.Fatalf("identity mismatch not refused: %v", err)
			}
		})
	}
}
