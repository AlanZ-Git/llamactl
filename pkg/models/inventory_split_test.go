package models

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func splitGGUF(arch, name, tensor string, no, count uint64, dim uint64, typ uint32, extra map[string]uint64) []byte {
	var b bytes.Buffer
	put := func(v any) {
		if err := binary.Write(&b, binary.LittleEndian, v); err != nil {
			panic(err)
		}
	}
	str := func(v string) { put(uint64(len(v))); b.WriteString(v) }
	b.WriteString("GGUF")
	put(uint32(3))
	put(uint64(1))
	kv := uint64(2 + len(extra))
	if count > 0 {
		kv += 3
	}
	put(kv)
	for _, pair := range [][2]string{{"general.architecture", arch}, {"general.name", name}} {
		str(pair[0])
		put(uint32(8))
		str(pair[1])
	}
	if count > 0 {
		for _, pair := range []struct {
			k string
			v uint64
		}{{"split.no", no}, {"split.count", count}, {"split.tensors.count", count}} {
			str(pair.k)
			put(uint32(10))
			put(pair.v)
		}
	}
	for k, v := range extra {
		str(k)
		put(uint32(10))
		put(v)
	}
	str(tensor)
	put(uint32(1))
	put(dim)
	put(typ)
	put(uint64(0))
	for b.Len()%32 != 0 {
		b.WriteByte(0)
	}
	b.Write(make([]byte, 64))
	return b.Bytes()
}
func TestN1SplitGroupsRejectInvalidMembers(t *testing.T) {
	for _, variant := range []string{"complete", "unknown_type", "truncated_payload", "missing", "mixed_arch", "mixed_name", "zero_dimension", "truncated_metadata", "duplicate_tensor", "wrong_index"} {
		t.Run(variant, func(t *testing.T) {
			root := t.TempDir()
			first := splitGGUF("llama", "pair", "a", 0, 2, 1, 0, nil)
			second := splitGGUF("llama", "pair", "b", 1, 2, 1, 0, nil)
			switch variant {
			case "mixed_arch":
				second = splitGGUF("qwen2", "pair", "b", 1, 2, 1, 0, nil)
			case "mixed_name":
				second = splitGGUF("llama", "other", "b", 1, 2, 1, 0, nil)
			case "unknown_type":
				second = splitGGUF("llama", "pair", "b", 1, 2, 1, 0xffffffff, nil)
			case "truncated_payload":
				second = second[:len(second)-64]
			case "zero_dimension":
				second = splitGGUF("llama", "pair", "b", 1, 2, 0, 0, nil)
			case "truncated_metadata":
				second = second[:24]
			case "duplicate_tensor":
				second = splitGGUF("llama", "pair", "a", 1, 2, 1, 0, nil)
			case "wrong_index":
				second = splitGGUF("llama", "pair", "b", 0, 2, 1, 0, nil)
			}
			if err := os.WriteFile(filepath.Join(root, "pair-00001-of-00002.gguf"), first, 0600); err != nil {
				t.Fatal(err)
			}
			if variant != "missing" {
				if err := os.WriteFile(filepath.Join(root, "pair-00002-of-00002.gguf"), second, 0600); err != nil {
					t.Fatal(err)
				}
			}
			inv, err := NewInventory(map[string]string{"r": root})
			if err != nil {
				t.Fatal(err)
			}
			defer inv.Close()
			scan, err := inv.Scan(context.Background(), "r")
			if err != nil {
				t.Fatal(err)
			}
			complete := variant == "complete" || variant == "unknown_type" || variant == "truncated_payload"
			logical := 0
			for _, c := range scan.Candidates {
				if c.State == "discovered" {
					logical++
				}
				a, e := inv.Verify(context.Background(), scan.ID, c.ID, "")
				if complete && c.Name == "pair-00001-of-00002.gguf" {
					if e != nil || len(a.Members) != 2 || a.Main != c.Name {
						t.Fatalf("complete group: %+v %v", a, e)
					}
				} else if e == nil {
					t.Fatalf("invalid/member candidate verified: %+v", c)
				}
			}
			if complete && logical != 1 {
				t.Fatal("not one logical model", logical)
			}
			if !complete && logical != 0 {
				t.Fatalf("invalid group exposed as runnable: %+v", scan.Candidates)
			}
		})
	}
}
func TestN1SplitChangeAfterScanRefused(t *testing.T) {
	root := t.TempDir()
	for k := 0; k < 2; k++ {
		name := fmt.Sprintf("pair-%05d-of-00002.gguf", k+1)
		if err := os.WriteFile(filepath.Join(root, name), splitGGUF("llama", "pair", fmt.Sprint(k), uint64(k), 2, 1, 0, nil), 0600); err != nil {
			t.Fatal(err)
		}
	}
	inv, err := NewInventory(map[string]string{"r": root})
	if err != nil {
		t.Fatal(err)
	}
	defer inv.Close()
	scan, err := inv.Scan(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "pair-00002-of-00002.gguf"), splitGGUF("qwen2", "pair", "1", 1, 2, 1, 0, nil), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = inv.Verify(context.Background(), scan.ID, scan.Candidates[0].ID, ""); err == nil {
		t.Fatal("changed mixed split verified from stale scan")
	}
}
func TestN1ProjectorAssociationMetadata(t *testing.T) {
	for _, variant := range []string{"wrong_type", "incompatible", "unknown"} {
		t.Run(variant, func(t *testing.T) {
			root := t.TempDir()
			files := map[string][]byte{"model.gguf": splitGGUF("llama", "m", "a", 0, 0, 1, 0, map[string]uint64{"llama.embedding_length": 32})}
			arch := "clip"
			extra := map[string]uint64{}
			if variant == "wrong_type" {
				arch = "llama"
			}
			if variant == "incompatible" {
				extra["clip.vision.projection_dim"] = 64
			}
			files["proj.gguf"] = splitGGUF(arch, "p", "b", 0, 0, 1, 0, extra)
			for name, b := range files {
				if err := os.WriteFile(filepath.Join(root, name), b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			inv, err := NewInventory(map[string]string{"r": root})
			if err != nil {
				t.Fatal(err)
			}
			defer inv.Close()
			s, err := inv.Scan(context.Background(), "r")
			if err != nil {
				t.Fatal(err)
			}
			ids := map[string]string{}
			for _, c := range s.Candidates {
				ids[c.Name] = c.ID
			}
			a, err := inv.Verify(context.Background(), s.ID, ids["model.gguf"], ids["proj.gguf"])
			if variant != "unknown" {
				if err == nil {
					t.Fatalf("bad projector verified: %+v", a)
				}
			} else if err != nil {
				t.Fatal(err)
			} else {
				// Content identity is not semantic engine compatibility.
				if a.ProjectorCompatibility != "unverified" {
					t.Fatal("unknown projector compatibility claimed proven", a)
				}
				if _, _, err = inv.Resolve(context.Background(), a); err == nil {
					t.Fatal("unverified projector association executable")
				}
			}
		})
	}
}
