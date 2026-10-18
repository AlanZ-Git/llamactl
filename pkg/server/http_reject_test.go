package server

import (
	"encoding/json"
	"io"
	"llamactl/pkg/resourcepolicy"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type rejectBody struct {
	io.Reader
	read, closed int
}

func (b *rejectBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}
func (b *rejectBody) Close() error { b.closed++; return nil }

type rejectDeadlineWriter struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (w *rejectDeadlineWriter) SetReadDeadline(d time.Time) error {
	w.deadlines = append(w.deadlines, d)
	return nil
}

func TestControlRejectionOwnerPauseAndAuth(t *testing.T) {
	var calls atomic.Int32
	router, _, inst, _ := admissionServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{}`)
	}))
	ownerPut(t, router, false, ownerGet(t, router), 200)
	payload, err := json.Marshal(inst.GetOptions())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []struct{ method, path, body string }{
		{"POST", "/api/v1/llama-cpp/admitted/models/synthetic/load", `{"model":"synthetic"}`},
		{"POST", "/api/v1/instances/admitted/proxy/models/load", `{"model":"synthetic"}`},
		{"POST", "/api/v1/instances/admitted/start", `{}`},
		{"POST", "/api/v1/instances/admitted/restart", `{}`},
		{"PUT", "/api/v1/instances/admitted", string(payload) + strings.Repeat(" ", 1024)},
		{"POST", "/api/v1/instances/new", string(payload) + strings.Repeat(" ", 1024)},
	} {
		t.Run(a.method+a.path, func(t *testing.T) {
			r := admittedRequest(a.method, a.path, a.body)
			body := &rejectBody{Reader: strings.NewReader(a.body)}
			r.Body = body
			w := &rejectDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
			router.ServeHTTP(w, r)
			if w.Code != 409 || !strings.Contains(w.Body.String(), `"error":"ai_paused"`) || !strings.Contains(w.Body.String(), `"retryable":false`) {
				t.Fatalf("pause changed: %d %s", w.Code, w.Body.String())
			}
			if body.read != len(a.body) {
				t.Fatalf("pause consumed %d of %d", body.read, len(a.body))
			}
			r = admittedRequest(a.method, a.path, a.body)
			r.Header.Del("Authorization")
			body = &rejectBody{Reader: strings.NewReader(a.body)}
			r.Body = body
			w = &rejectDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
			router.ServeHTTP(w, r)
			if w.Code != 401 || body.read != 0 || len(w.deadlines) != 0 {
				t.Fatal("auth rejection changed or body settled before auth")
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("paused load reached backend")
	}
	for _, a := range []struct {
		path   string
		status int
	}{
		{"/api/v1/instances/admitted/proxy/completion", 503},
		{"/api/v1/instances/admitted/proxy/props", 409},
		{"/llama-cpp/admitted/completion", 503},
	} {
		r := admittedRequest("POST", a.path, `{"model":"synthetic"}`)
		body := &rejectBody{Reader: strings.NewReader(`{"model":"synthetic"}`)}
		r.Body = body
		w := &rejectDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
		router.ServeHTTP(w, r)
		if w.Code != a.status || body.read != 0 || len(w.deadlines) != 0 {
			t.Fatalf("non-target changed: %s: %d reads=%d", a.path, w.Code, body.read)
		}
	}
	// Owner pause must still allow unload, not turn it into a load-like action.
	admissionResponse(t, router, "POST", "/api/v1/llama-cpp/admitted/models/synthetic/unload", `{"model":"synthetic"}`, 200, "")
	if calls.Load() != 1 {
		t.Fatal("paused unload did not preserve forwarding")
	}
}

type inspectRejectReader struct{ read func([]byte) (int, error) }

func (r inspectRejectReader) Read(p []byte) (int, error) { return r.read(p) }

func TestControlRejectionReleasesPermitsBeforeSettlement(t *testing.T) {
	var policy *resourcepolicy.Gate
	router, _, inst, _ := admissionServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("busy request reached backend") }), func(h *Handler) { policy = h.cfg.ResourcePolicy })
	lease, err := inst.BeginMaintenance()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	const payload = `{"model":"synthetic"}`
	r := admittedRequest("POST", "/api/v1/llama-cpp/admitted/models/synthetic/load", payload)
	reader := strings.NewReader(payload)
	checked := false
	r.Body = &rejectBody{Reader: inspectRejectReader{read: func(p []byte) (int, error) {
		if !checked {
			checked = true
			if policy.Active() != 1 {
				t.Errorf("rejected request retained owner permit during settlement: %d", policy.Active())
			}
			lease.Close()
			probe, err := inst.BeginMaintenance()
			if err != nil {
				t.Errorf("rejection retained instance lease: %v", err)
			} else {
				probe.Close()
			}
		}
		return reader.Read(p)
	}}}
	w := &rejectDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	router.ServeHTTP(w, r)
	if !checked || w.Code != 409 || policy.Active() != 0 {
		t.Fatalf("settlement/release: checked=%v status=%d active=%d", checked, w.Code, policy.Active())
	}
}

func TestControlRejectionConsumesSmallBody(t *testing.T) {
	var calls atomic.Int32
	router, _, inst, preset := admissionServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	lease, err := inst.BeginMaintenance()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	oldOptions := inst.GetOptions()
	oldPreset, err := os.ReadFile(preset)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []struct{ method, path string }{
		{"POST", "/api/v1/llama-cpp/admitted/models/synthetic/load"},
		{"POST", "/api/v1/llama-cpp/admitted/models/synthetic/unload"},
		{"POST", "/api/v1/instances/admitted/proxy/models/load"},
		{"POST", "/api/v1/instances/admitted/proxy/models/unload"},
		{"POST", "/api/v1/instances/admitted/start"},
		{"POST", "/api/v1/instances/admitted/stop"},
		{"POST", "/api/v1/instances/admitted/restart"},
		{"DELETE", "/api/v1/instances/admitted"},
	} {
		t.Run(action.path, func(t *testing.T) {
			const payload = `{"model":"synthetic-test"}`
			r := admittedRequest(action.method, action.path, payload)
			r.Close = true
			body := &rejectBody{Reader: strings.NewReader(payload)}
			r.Body = body
			w := &rejectDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
			router.ServeHTTP(w, r)
			if w.Code != 409 || !strings.Contains(w.Body.String(), `"error":"busy"`) {
				t.Fatalf("response: %d %s", w.Code, w.Body.String())
			}
			if body.read != len(payload) {
				t.Errorf("rejection consumed %d bytes; want exactly %d", body.read, len(payload))
			}
			if body.closed != 0 {
				t.Errorf("cleanup called potentially blocking Body.Close")
			}
			if len(w.deadlines) != 2 || w.deadlines[0].IsZero() || !w.deadlines[1].IsZero() {
				t.Errorf("read deadline must be installed and cleared: %v", w.deadlines)
			}
		})
	}
	current, err := os.ReadFile(preset)
	if err != nil || string(current) != string(oldPreset) || inst.GetOptions() != oldOptions || calls.Load() != 0 {
		t.Fatal("rejection mutated config/preset or called backend")
	}
}
