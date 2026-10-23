package instance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestN1ReservationExcludesSentenceGapsAndOldControl(t *testing.T) {
	inst, _ := protocolFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Service-Use") != "" {
			t.Error("use leaked")
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"synthetic"}}]}`))
	}))
	token := publishSyntheticReady(t, inst, "alpha")
	s := inst.ControlSnapshot()
	reservation, err := inst.ReserveN1("use", "scope", "service_use", s.BootID, s.Revision, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	defer reservation.CloseConfirmed()
	if _, err = inst.BeginControl(s.BootID, s.Revision, true, false); err == nil {
		t.Fatal("legacy control bypassed reservation")
	}
	if _, _, err = inst.acquireUse(false); err == nil {
		t.Fatal("legacy inference bypassed reservation")
	}
	if err = reservation.ActivateService(RecipeRef{ID: "recipe", Version: 1}, "alpha", RequestContract{TemplateVersion: "v1", Parameters: map[string]json.RawMessage{"max_tokens": json.RawMessage(`4`)}, SystemPrefix: "synthetic"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	request := func(scope, use string) int {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"alpha","messages":[{"role":"user","content":"test"}]}`))
		r.Header.Set("X-Ready-Token", token)
		r.Header.Set("X-Service-Use", use)
		r.Header.Set("X-Prompt-Template-Version", "v1")
		r.Header.Set("X-Prompt-Digest", reservation.contract.PromptDigest())
		r = WithServiceScope(r, scope)
		w := httptest.NewRecorder()
		_ = inst.ServeHTTP(w, r)
		return w.Code
	}
	if status := request("other", "use"); status != 409 {
		t.Fatalf("foreign use: %d", status)
	}
	if status := request("scope", "use"); status != 200 {
		t.Fatalf("own use: %d", status)
	}
	if _, err = inst.BeginControl(s.BootID, s.Revision, true, false); err == nil {
		t.Fatal("sentence gap released reservation")
	}
	reservation.BeginClosing()
	if status := request("scope", "use"); status != 409 {
		t.Fatalf("closing admitted: %d", status)
	}
	if err = reservation.CloseConfirmed(); err != nil {
		t.Fatal(err)
	}
	m, err := inst.BeginControl(s.BootID, s.Revision, true, false)
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
}
func TestN1FullRecipeReplacementAndLegacyProvenance(t *testing.T) {
	inst, _ := protocolFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	old, _ := inst.GetOptions().Clone()
	m := old.Control.Models["alpha"]
	m.Parameters = map[string]json.RawMessage{"threads": json.RawMessage(`8`), "gpu_layers": json.RawMessage(`9`)}
	old.Control.Models["alpha"] = m
	old.ActiveRecipe = &RecipeRef{ID: "old", Version: 1}
	old.PresetIni = nil
	inst.options.set(old)
	next, err := inst.RecipeCandidate("alpha", map[string]json.RawMessage{"gpu_layers": json.RawMessage(`0`), "flash_attention": json.RawMessage(`false`), "context": json.RawMessage(`null`)}, RecipeRef{ID: "new", Version: 2}, "digest")
	if err != nil {
		t.Fatal(err)
	}
	p := next.Control.Models["alpha"].Parameters
	if _, ok := p["threads"]; ok {
		t.Fatal("old override inherited")
	}
	if string(p["gpu_layers"]) != "0" || string(p["flash_attention"]) != "false" {
		t.Fatal("zero/false lost")
	}
	if _, ok := p["context"]; ok {
		t.Fatal("null not reset")
	}
	if next.ActiveRecipe.ID != "new" {
		t.Fatal("provenance missing")
	}
	legacy, err := inst.ControlCandidate("alpha", map[string]json.RawMessage{"threads": json.RawMessage(`2`)})
	if err != nil {
		t.Fatal(err)
	}
	if legacy.ActiveRecipe != nil {
		t.Fatal("legacy retained provenance")
	}
	_ = context.Background()
}
