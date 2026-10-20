package instance

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestP3AllInferencePathsRequireExactModelAndStripToken(t *testing.T) {
	paths := []string{"/completion", "/completions", "/v1/completions", "/chat/completions", "/v1/chat/completions", "/embedding", "/embeddings", "/v1/embeddings", "/rerank", "/reranking", "/v1/rerank", "/v1/reranking", "/infill", "/tokenize", "/detokenize", "/apply-template"}
	inst, _ := protocolFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.Header.Values("X-Ready-Token")) != 0 {
			t.Error("token leaked")
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"model":"alpha"`) {
			t.Error("model rewritten incorrectly")
		}
		w.WriteHeader(200)
	}))
	token := publishSyntheticReady(t, inst, "alpha")
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			for _, tc := range []struct {
				model, token string
				want         int
			}{{"beta", token, 409}, {"alpha", "", 409}, {"alpha", token, 200}} {
				req := httptest.NewRequest("POST", path, strings.NewReader(`{"model":"`+tc.model+`"}`))
				req.Header.Set("X-Ready-Token", tc.token)
				w := httptest.NewRecorder()
				_ = inst.ServeHTTP(w, req)
				if w.Code != tc.want {
					t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
				}
			}
		})
	}
}

func TestP3ReadyHeaderNeverForwarded(t *testing.T) {
	for _, path := range []string{"/health", "/models", "/v1/models"} {
		t.Run(path, func(t *testing.T) {
			seen := make(chan string, 1)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen <- r.Header.Get("X-Ready-Token")
				w.WriteHeader(200)
			}))
			defer backend.Close()
			cfg, opts := policyFixture(t, "isolated")
			opts.BackendOptions.SetPort(backend.Listener.Addr().(*net.TCPAddr).Port)
			inst := New("header", cfg, opts, nil)
			inst.SetStatus(Running)
			req := httptest.NewRequest("GET", path, nil)
			req.Header.Set("X-Ready-Token", "synthetic-private-capability")
			if err := inst.ServeHTTP(httptest.NewRecorder(), req); err != nil {
				t.Fatal(err)
			}
			if token := <-seen; token != "" {
				t.Fatalf("ready token leaked to backend: %q", token)
			}
		})
	}
}
