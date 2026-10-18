package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"llamactl/pkg/instance"
)

func TestControlledOriginalJSONProductionBoundary(t *testing.T) {
	var calls atomic.Int32
	router, _, _, _ := admissionServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{}`)
	}))
	for _, path := range []string{"/v1/chat/completions", "/llama-cpp/admitted/completion", "/api/v1/instances/admitted/proxy/completion"} {
		for name, body := range map[string]string{
			"duplicate": `{"model":"admitted/synthetic","temperature":0,"temperature":1}`,
			"nested":    `{"model":"admitted/synthetic","chat_template_kwargs":{"enable_thinking":false,"enable_thinking":true}}`,
			"model":     `{"model":"other","model":"admitted/synthetic"}`,
			"oversize":  `{"model":"admitted/synthetic"}` + strings.Repeat(" ", 16<<20),
			"override":  `{"model":"admitted/synthetic","extra":{"autoload":null}}`,
		} {
			t.Run(path+"/"+name, func(t *testing.T) {
				before := calls.Load()
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, admittedRequest("POST", path, body))
				if recorder.Code != 400 || !strings.Contains(recorder.Body.String(), `"error":"unsupported_controlled_request"`) || calls.Load() != before {
					t.Fatalf("status=%d body=%s backend_calls=%d; want 400 unsupported_controlled_request, zero calls", recorder.Code, recorder.Body.String(), calls.Load()-before)
				}
			})
		}
	}
	for _, policy := range []string{"query", "encoded"} {
		t.Run(policy, func(t *testing.T) {
			before := calls.Load()
			path := "/v1/chat/completions"
			if policy == "query" {
				path += "?autoload=false"
			}
			req := admittedRequest("POST", path, `{"model":"admitted/synthetic"}`)
			if policy == "encoded" {
				req.Header.Set("Content-Encoding", "gzip")
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)
			if recorder.Code != 400 || !strings.Contains(recorder.Body.String(), `"error":"unsupported_controlled_request"`) || calls.Load() != before {
				t.Fatalf("policy bypass: %d %s calls=%d", recorder.Code, recorder.Body.String(), calls.Load()-before)
			}
		})
	}
}

func TestControlledOpenAIModelRewriteAndLegacy(t *testing.T) {
	var calls atomic.Int32
	var forwarded map[string]any
	router, _, inst, _ := admissionServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if err := json.NewDecoder(r.Body).Decode(&forwarded); err != nil {
			t.Error(err)
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	for _, single := range []bool{false, true} {
		model, want := "admitted/synthetic", "synthetic"
		if single {
			inst.SetStatus(instance.Stopped)
			candidate, err := inst.GetOptions().Clone()
			if err != nil {
				t.Fatal(err)
			}
			candidate.BackendOptions.LlamaServerOptions.Model = "single.gguf"
			if err := inst.SetOptions(candidate); err != nil {
				t.Fatal(err)
			}
			inst.SetStatus(instance.Running)
			model, want = "admitted", "single.gguf"
		}
		body := `{"model":"` + model + `","temperature":0,"stream":false,"chat_template_kwargs":{"enable_thinking":false}}`
		// Exactly 16 MiB, including trailing whitespace, remains valid.
		body += strings.Repeat(" ", (16<<20)-len(body))
		before := calls.Load()
		admissionResponse(t, router, "POST", "/v1/chat/completions", body, 200, "")
		if calls.Load() != before+1 || forwarded["model"] != want || forwarded["temperature"] != float64(0) || forwarded["stream"] != false || forwarded["chat_template_kwargs"].(map[string]any)["enable_thinking"] != false {
			t.Fatalf("model/zero/false forwarding changed: %#v", forwarded)
		}
	}
	inst.SetStatus(instance.Stopped)
	candidate, err := inst.GetOptions().Clone()
	if err != nil {
		t.Fatal(err)
	}
	candidate.Controlled = false
	if err := inst.SetOptions(candidate); err != nil {
		t.Fatal(err)
	}
	inst.SetStatus(instance.Running)
	body := `{"model":"admitted/synthetic","temperature":0,"temperature":1,"autoload":false}` + strings.Repeat(" ", 16<<20)
	before := calls.Load()
	admissionResponse(t, router, "POST", "/v1/chat/completions?legacy=true", body, 200, "")
	if calls.Load() != before+1 || forwarded["temperature"] != float64(1) || forwarded["autoload"] != false {
		t.Fatal("legacy normalization semantics changed")
	}
}

func TestControlledStartupPresetUpdatePreservesState(t *testing.T) {
	_, im, inst, preset := admissionServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("update reached backend")
	}))
	old := inst.GetOptions()
	before, err := os.ReadFile(preset)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := old.Clone()
	if err != nil {
		t.Fatal(err)
	}
	changed := string(before) + "load_on_startup=true\n"
	candidate.PresetIni = &changed
	candidate.BackendOptions.SetPort(old.BackendOptions.GetPort() + 1)
	_, err = im.UpdateInstance(inst.Name, candidate)
	after, readErr := os.ReadFile(preset)
	if err == nil || !strings.Contains(err.Error(), "controlled") || readErr != nil || string(after) != string(before) || inst.GetOptions() != old || inst.GetPort() != old.BackendOptions.GetPort() || inst.GetStatus() != instance.Running {
		t.Fatalf("invalid startup candidate was not rejected before old state/preset/port changes: %v", err)
	}
}
