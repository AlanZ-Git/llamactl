package database

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestN1RecipeVersionsAndExplicitCapabilities(t *testing.T) {
	db := newTestDB(t)
	s := db.N1Store()
	ctx := context.Background()
	if _, err := db.Exec(`INSERT INTO instances(id,name,status,created_at,updated_at,options_json) VALUES(1,'i','stopped',0,0,'{}'); INSERT INTO api_keys(id,key_hash,name,user_id,permission_mode,created_at,updated_at) VALUES(1,'synthetic','test','test','per_instance',0,0)`); err != nil {
		t.Fatal(err)
	}
	allowed, err := s.HasCapability(ctx, 1, 1, "evaluate")
	if err != nil || allowed {
		t.Fatalf("legacy capability: %v %v", allowed, err)
	}
	if err = s.SetCapabilities(ctx, 1, 1, []string{"discover", "evaluate"}); err != nil {
		t.Fatal(err)
	}
	allowed, err = s.HasCapability(ctx, 1, 1, "evaluate")
	if err != nil || !allowed {
		t.Fatalf("explicit capability: %v %v", allowed, err)
	}
	if err = s.SetCapabilities(ctx, 1, 1, []string{"unknown"}); err == nil {
		t.Fatal("unknown capability accepted")
	}
	allowed, _ = s.HasCapability(ctx, 1, 1, "evaluate")
	if !allowed {
		t.Fatal("invalid grant partially changed rights")
	}
	r := RecipeRecord{Instance: "i", ID: "recipe", Version: 1, Document: json.RawMessage(`{"description":"synthetic","deployment":{"gpu_layers":0,"flash_attention":false,"context":null}}`), ArtifactDigest: "a", CreatedBy: "s"}
	first, replay, err := s.PutRecipe(ctx, r)
	if err != nil || replay {
		t.Fatal(err)
	}
	if _, replay, err = s.PutRecipe(ctx, r); err != nil || !replay {
		t.Fatal("same version not idempotent", err)
	}
	r.Document = json.RawMessage(`{"description":"changed"}`)
	if _, _, err = s.PutRecipe(ctx, r); !errors.Is(err, ErrN1Conflict) {
		t.Fatal("immutable version overwritten", err)
	}
	r.Version = 2
	second, _, err := s.PutRecipe(ctx, r)
	if err != nil || second.Digest == first.Digest {
		t.Fatal(err)
	}
	got, err := s.GetRecipe(ctx, "i", "recipe", 1)
	if err != nil || string(got.Document) != string(first.Document) {
		t.Fatal("old version changed", err)
	}
	if err = s.SetCapabilities(ctx, 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	allowed, _ = s.HasCapability(ctx, 1, 1, "evaluate")
	if allowed {
		t.Fatal("revocation failed")
	}
}
