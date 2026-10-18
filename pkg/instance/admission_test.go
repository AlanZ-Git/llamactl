package instance

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestControlledSSEBlocksOptionsPreparation(t *testing.T) {
	entered, finish := make(chan struct{}), make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		close(entered)
		<-finish
	}))
	defer backend.Close()
	cfg, opts := policyFixture(t, "isolated")
	if err := json.Unmarshal([]byte(`{"controlled":true,"environment_policy":"isolated","backend_type":"llama_cpp","backend_options":{"no_models_autoload":true,"models_max":1}}`), opts); err != nil {
		t.Fatal(err)
	}
	port := backend.Listener.Addr().(*net.TCPAddr).Port
	opts.BackendOptions.SetPort(port)
	inst := New("stream", cfg, opts, nil)
	inst.SetStatus(Running)
	done := make(chan error, 1)
	go func() {
		done <- inst.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"synthetic","stream":true}`)))
	}()
	<-entered
	prepared, err := inst.PrepareOptions(opts)
	if prepared != nil {
		prepared.Close()
	}
	close(finish)
	if proxyErr := <-done; proxyErr != nil {
		t.Fatal(proxyErr)
	}
	if err == nil {
		t.Fatal("options preparation admitted while SSE still holds inference use")
	}
	lease, err := inst.BeginMaintenance()
	if err != nil {
		t.Fatalf("permit leaked after SSE EOF: %v", err)
	}
	lease.Close()
}
