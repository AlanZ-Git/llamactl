package instance

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeploymentFixtureHTTPRequestPolicy(t *testing.T) {
	const text = "This is a synthetic deployment check. List three colors in a short sentence. Do not include any personal information."
	if DeploymentFixtureID != "synthetic-text-v1" || deploymentFixture != text || DeploymentFixtureDigest() != "897be3a804a6a40fba8d9ff5a1ba088c970137f51bb1d3e502c503a087f24d40" {
		t.Fatal("fixture text identity changed with request policy")
	}
	for _, tc := range []struct {
		name, stream, code string
	}{
		{"content", "data: {\"choices\":[{\"delta\":{\"content\":\"red, green, blue\"}}],\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":0}}\n\ndata: [DONE]\n\n", ""},
		{"reasoning_only", "data: {\"choices\":[{\"delta\":{\"content\":\"\",\"reasoning_content\":\"not an answer\"}}],\"usage\":{\"prompt_tokens\":32,\"completion_tokens\":128}}\n\ndata: [DONE]\n\n", "generation_empty_response"},
		{"backend_failure", "", "generation_backend_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			inst, _ := protocolFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.RequestURI() != "/v1/chat/completions" || r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("unexpected request: %s %s %v", r.Method, r.URL, r.Header)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				want := map[string]any{
					"model": "alpha", "messages": []any{map[string]any{"role": "user", "content": text}},
					"stream": true, "stream_options": map[string]any{"include_usage": true},
					"max_tokens": float64(128), "seed": float64(0), "temperature": float64(0), "cache_prompt": false,
					"chat_template_kwargs": map[string]any{"enable_thinking": false},
				}
				if !reflect.DeepEqual(body, want) {
					t.Errorf("fixed request policy: got %#v, want %#v", body, want)
				}
				if tc.name == "backend_failure" {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.stream)
			}))
			publishSyntheticReady(t, inst, "alpha")
			s := inst.ControlSnapshot()
			lease, err := inst.BeginControl(s.BootID, s.Revision, true, false)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			out, err := lease.GenerateDeploymentFixture(context.Background(), "alpha", 128, 0)
			if tc.code == "" {
				if err != nil || !out.Completed || out.TTFTMS == nil || out.ResponseBytes != len("red, green, blue") || out.PromptTokens == nil || *out.PromptTokens != 0 || out.CompletionTokens == nil || *out.CompletionTokens != 0 {
					t.Fatalf("content/zero usage: %+v, error=%v", out, err)
				}
			} else if _, code, ok := AdmissionHTTPError(err); !ok || code != tc.code || out.Completed || out.ResponseBytes != 0 || out.TTFTMS != nil {
				t.Fatalf("failure must not become an answer/TTFT: %+v, error=%v", out, err)
			}
			if tc.name == "reasoning_only" && (out.PromptTokens == nil || *out.PromptTokens != 32 || out.CompletionTokens == nil || *out.CompletionTokens != 128) {
				t.Fatalf("failed request usage lost: %+v", out)
			}
			if calls.Load() != 1 {
				t.Fatalf("request retried: %d calls", calls.Load())
			}
		})
	}
}

func TestDeploymentStreamTTFTIgnoresControlAndReasoning(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	go func() {
		defer writer.Close()
		_, _ = io.WriteString(writer, ": keepalive\n\nevent: message\ndata: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"\",\"reasoning_content\":\"not output\"}}]}\n\ndata: {\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":0}}\n\n")
		time.Sleep(80 * time.Millisecond)
		_, _ = io.WriteString(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"色\"}}],\n")
		_, _ = io.WriteString(writer, "data: \"timings\":{\"prompt_ms\":0,\"predicted_ms\":12.5}}\r\n\r\ndata: [DONE]\n\n")
	}()
	out := GenerationObservation{StartedAt: time.Now()}
	if err := readDeploymentStream(reader, &out); err != nil {
		t.Fatal(err)
	}
	if !out.Completed || out.TTFTMS == nil || *out.TTFTMS < 70 || out.ResponseBytes != 3 {
		t.Fatalf("content timing/UTF-8 length: %+v", out)
	}
	if out.PromptTokens == nil || *out.PromptTokens != 0 || out.CompletionTokens == nil || *out.CompletionTokens != 0 || out.PrefillMS == nil || *out.PrefillMS != 0 || out.DecodeMS == nil || *out.DecodeMS != 12.5 {
		t.Fatalf("exact zero usage/timing not preserved: %+v", out)
	}
}

func TestDeploymentStreamMissingAndMalformedBounded(t *testing.T) {
	content := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"
	cases := []struct {
		name, data string
		success    bool
	}{
		{"missing_usage", content + "data: [DONE]\n\n", true},
		{"null_content", "data: {\"choices\":[{\"delta\":{\"content\":null}}]}\n\n" + content + "data: [DONE]\n\n", true},
		{"no_done", content, false}, {"empty", "data: [DONE]\n\n", false},
		{"malformed", "data: {broken}\n\n", false},
		{"error_body", "data: {\"error\":{\"message\":\"/private/path secret\"}}\n\n", false},
		{"oversized_frame", "data: " + strings.Repeat("x", 65<<10) + "\n\n", false},
		{"oversized_total", strings.Repeat(": "+strings.Repeat("x", 1024)+"\n\n", 2100) + content + "data: [DONE]\n\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := GenerationObservation{StartedAt: time.Now()}
			err := readDeploymentStream(strings.NewReader(tc.data), &out)
			if (err == nil) != tc.success {
				t.Fatalf("error=%v completed=%v", err, out.Completed)
			}
			if out.PromptTokens != nil || out.CompletionTokens != nil || out.PrefillMS != nil || out.DecodeMS != nil {
				t.Fatal("missing values fabricated")
			}
			if err != nil && (strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "secret")) {
				t.Fatal("backend text leaked")
			}
		})
	}
	if DeploymentFixtureID != "synthetic-text-v1" || len(DeploymentFixtureDigest()) != 64 || len(deploymentFixture) > 512 {
		t.Fatal("fixture identity/bound drift")
	}
}
