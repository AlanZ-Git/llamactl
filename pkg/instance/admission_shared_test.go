package instance

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestControlledOverlappingSharedUses(t *testing.T) {
	entered := make(chan struct{}, 2)
	finish := make(chan struct{})
	var once sync.Once
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-finish
		_, _ = io.WriteString(w, `{}`)
	}))
	defer engine.Close()
	defer once.Do(func() { close(finish) })
	cfg, opts := controlledFixture(t)
	opts.BackendOptions.SetPort(engine.Listener.Addr().(*net.TCPAddr).Port)
	inst := New("shared", cfg, opts, nil)
	inst.SetStatus(Running)
	done := make(chan error, 2)
	for range 2 {
		go func() {
			done <- inst.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/completion", strings.NewReader(`{}`)))
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("shared permits serialized inference")
		}
	}
	if _, err := inst.BeginMaintenance(); err == nil {
		t.Fatal("maintenance admitted with two shared uses")
	}
	once.Do(func() { close(finish) })
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	lease, err := inst.BeginMaintenance()
	if err != nil {
		t.Fatal(err)
	}
	lease.Close()
}

func TestControlledLegacyMaintenanceCompatibility(t *testing.T) {
	cfg, opts := policyFixture(t, "isolated")
	inst := New("legacy-maintenance", cfg, opts, nil)
	first, err := inst.BeginMaintenance()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := inst.BeginMaintenance()
	if err != nil {
		t.Fatalf("legacy maintenance unexpectedly rejected: %v", err)
	}
	defer second.Close()
	candidate, _ := opts.Clone()
	candidate.Controlled = true
	if err := first.CheckModeChange(candidate); err == nil {
		t.Fatal("transition ignored another legacy operation")
	}
	second.Close()
	if err := first.CheckModeChange(candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := inst.BeginMaintenance(); err == nil {
		t.Fatal("legacy operation slipped into exclusive transition")
	}
}

func TestControlledBooleanContract(t *testing.T) {
	for _, field := range []string{`"controlled":null,`, `"controlled":"true",`, `"controlled":1,`} {
		var opts Options
		if err := json.Unmarshal([]byte(`{`+field+`"backend_type":"llama_cpp","backend_options":{}}`), &opts); err == nil {
			t.Fatalf("accepted non-boolean: %s", field)
		}
	}
	for _, field := range []string{"", `"controlled":false,`} {
		var opts Options
		if err := json.Unmarshal([]byte(`{`+field+`"backend_type":"llama_cpp","backend_options":{}}`), &opts); err != nil || opts.Controlled {
			t.Fatalf("legacy contract: %s %v", field, err)
		}
	}
}
