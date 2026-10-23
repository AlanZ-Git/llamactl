package models

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestN1InventoryHashesOnlyFirstOrChangedIdentity(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "model.gguf"), n1GGUF("llama"), 0600)
	inv, err := NewInventory(map[string]string{"r": root})
	if err != nil {
		t.Fatal(err)
	}
	defer inv.Close()
	calls := 0
	inv.hash = func(ctx context.Context, root, name, role string, full bool) (ArtifactMember, error) {
		if full {
			calls++
		}
		return hashMember(ctx, root, name, role, full)
	}
	s, _ := inv.Scan(context.Background(), "r")
	a, err := inv.Verify(context.Background(), s.ID, s.Candidates[0].ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = inv.Verify(context.Background(), s.ID, s.Candidates[0].ID, ""); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("unchanged weights rehashed", calls)
	}
	restored, _ := NewInventory(map[string]string{"r": root})
	defer restored.Close()
	restored.Remember(a)
	restored.hash = inv.hash
	s2, _ := restored.Scan(context.Background(), "r")
	if _, err = restored.Verify(context.Background(), s2.ID, s2.Candidates[0].ID, ""); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("persisted stat cache ignored")
	}
	os.WriteFile(filepath.Join(root, "model.gguf"), n1GGUF("qwen2"), 0600)
	s3, _ := inv.Scan(context.Background(), "r")
	if _, err = inv.Verify(context.Background(), s3.ID, s3.Candidates[0].ID, ""); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("changed weights not explicitly rehashed", calls)
	}
}
