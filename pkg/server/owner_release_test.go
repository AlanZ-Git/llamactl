package server

import (
	"encoding/json"
	"fmt"
	"io"
	"llamactl/pkg/config"
	"llamactl/pkg/resourcepolicy"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func releaseBody(s resourcepolicy.Snapshot, id, mode string) string {
	b, _ := json.Marshal(map[string]any{"boot_id": s.BootID, "expected_revision": s.Revision, "request_id": id, "mode": mode})
	return string(b)
}
func TestOwnerReleaseEndpointAccepted(t *testing.T) {
	router, _, _, _ := admissionServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	s := ownerPut(t, router, false, ownerGet(t, router), 200)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, admittedRequest("POST", "/api/v1/resource-policy/release", releaseBody(s, "synthetic-release-1", "immediate")))
	if w.Code != http.StatusAccepted {
		t.Fatalf("release endpoint: got %d %s; want 202 policy snapshot", w.Code, w.Body.String())
	}
}

func TestOwnerReleaseStrictHTTP(t *testing.T) {
	g, err := resourcepolicy.Open("")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.AppConfig{ResourcePolicy: g}
	cfg.Auth.ManagementKeys = []string{"synthetic-management-key"}
	router := SetupRouter(NewHandler(nil, nil, cfg, nil, nil))
	s := ownerPut(t, router, false, ownerGet(t, router), 200)
	valid := releaseBody(s, "strict", "immediate")
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`null`, 400}, {`[]`, 400}, {`{}`, 400}, {valid + ` {}`, 400}, {valid + strings.Repeat(" ", 4097), 400},
		{strings.Replace(valid, `"immediate"`, `null`, 1), 400}, {strings.Replace(valid, `"immediate"`, `true`, 1), 400},
		{strings.Replace(valid, `"immediate"`, `"force"`, 1), 400},
		{strings.Replace(valid, `"strict"`, `""`, 1), 400}, {strings.Replace(valid, `"strict"`, `"space id"`, 1), 400},
		{strings.Replace(valid, `"strict"`, `"`+strings.Repeat("x", 129)+`"`, 1), 400},
		{strings.Replace(valid, `"strict"`, `123`, 1), 400},
		{strings.Replace(valid, `"expected_revision":1`, `"expected_revision":null`, 1), 400},
		{strings.Replace(valid, `"expected_revision":1`, `"expected_revision":-1`, 1), 400},
		{strings.Replace(valid, `"expected_revision":1`, `"expected_revision":1.0`, 1), 400},
		{strings.Replace(valid, `"expected_revision":1`, `"expected_revision":1e0`, 1), 400},
		{strings.Replace(valid, `"expected_revision":1`, `"expected_revision":18446744073709551616`, 1), 400},
		{strings.Replace(valid, `"expected_revision":1`, `"expected_revision":"1"`, 1), 400},
		{strings.Replace(valid, `"mode":"immediate"`, `"mode":"immediate","mode":"immediate"`, 1), 400},
		{strings.Replace(valid, `"mode":"immediate"`, `"mode":"immediate","extra":false`, 1), 422},
		{strings.Replace(valid, s.BootID, "stale-boot", 1), 409},
		{strings.Replace(valid, `"expected_revision":1`, `"expected_revision":0`, 1), 409},
	} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, admittedRequest("POST", "/api/v1/resource-policy/release", tc.body))
		if w.Code != tc.status {
			t.Errorf("%s: %d %s; want %d", tc.body, w.Code, w.Body.String(), tc.status)
		}
		if !reflect.DeepEqual(s, g.Snapshot()) {
			t.Fatal("rejected release changed policy")
		}
	}
	for _, key := range []string{"", "synthetic-inference-key"} {
		r := httptest.NewRequest("POST", "/api/v1/resource-policy/release", strings.NewReader(valid))
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal("owner authorization bypass", w.Code)
		}
	}
	for n := 0; n < 2; n++ {
		admissionResponse(t, router, "POST", "/api/v1/resource-policy/release", valid, 202, "")
	}
	if g.Snapshot().Revision != s.Revision {
		t.Fatal("release changed policy revision")
	}
	admissionResponse(t, router, "POST", "/api/v1/resource-policy/release", releaseBody(s, "strict", "graceful"), 409, "")
}

func TestOwnerImmediateCancelsRealSSEAndNativeLoading(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/llama-cpp/admitted/completion", "/api/v1/instances/admitted/proxy/completion", "/api/v1/llama-cpp/admitted/models/synthetic/load", "/api/v1/instances/admitted/proxy/models/load"} {
		t.Run(path, func(t *testing.T) {
			entered, canceled := make(chan struct{}), make(chan struct{})
			router, _, _, _ := admissionServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: first\n\n")
				w.(http.Flusher).Flush()
				close(entered)
				<-r.Context().Done()
				close(canceled)
			}))
			front := httptest.NewServer(router)
			defer front.Close()
			client := &http.Client{Timeout: 5 * time.Second}
			req, _ := http.NewRequest("POST", front.URL+path, strings.NewReader(`{"model":"admitted/synthetic","stream":true}`))
			if strings.Contains(path, "/models/") {
				req, _ = http.NewRequest("POST", front.URL+path, strings.NewReader(`{"model":"synthetic"}`))
			}
			req.Header.Set("Authorization", "Bearer synthetic-management-key")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			prefix := make([]byte, len("data: first\n\n"))
			if _, err := io.ReadFull(resp.Body, prefix); err != nil {
				t.Fatal(err)
			}
			awaitAdmission(t, entered)
			start := time.Now()
			s := ownerPut(t, router, false, ownerGet(t, router), 200)
			if time.Since(start) > time.Second || s.Release != "draining" {
				t.Fatal("pause waited for stream/loading", s)
			}
			select {
			case <-canceled:
				t.Fatal("graceful pause canceled existing work")
			default:
			}
			start = time.Now()
			admissionResponse(t, router, "POST", "/api/v1/resource-policy/release", releaseBody(s, "interrupt", "immediate"), 202, "")
			if time.Since(start) > time.Second {
				t.Fatal("immediate HTTP waited for ordinary maintenance")
			}
			awaitAdmission(t, canceled)
			rest, readErr := io.ReadAll(resp.Body)
			if strings.Contains(string(rest), "[DONE]") || readErr == nil {
				t.Fatalf("interrupted SSE looked naturally complete: %q %v", rest, readErr)
			}
		})
	}
}
