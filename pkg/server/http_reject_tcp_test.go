package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"llamactl/pkg/instance"
	"llamactl/pkg/resourcepolicy"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type rejectionObservation struct {
	path    string
	bytes   int
	elapsed time.Duration
}

func rejectionTCPServer(t *testing.T, router http.Handler) (*httptest.Server, <-chan rejectionObservation) {
	t.Helper()
	observed := make(chan rejectionObservation, 64)
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, start := r.URL.Path, time.Now()
		body := &rejectBody{Reader: r.Body}
		// Preserve Close behavior in the fixture; the product cleanup must not
		// use it, but ordinary accepted decoders/proxies are free to do so.
		r.Body = &observedRequestBody{rejectBody: body, original: r.Body}
		router.ServeHTTP(w, r)
		observed <- rejectionObservation{path, body.read, time.Since(start)}
	}))
	// Test safety only: assertions below require the product's 100 ms bound,
	// not these longer host timeouts, to end an incomplete request.
	s.Config.ReadTimeout = 4 * time.Second
	s.Config.WriteTimeout = 4 * time.Second
	s.Start()
	t.Cleanup(s.Close)
	return s, observed
}

type observedRequestBody struct {
	*rejectBody
	original io.ReadCloser
}

func (b *observedRequestBody) Close() error { return b.original.Close() }

func controlConn(t *testing.T, s *httptest.Server) (*net.TCPConn, *bufio.Reader) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", s.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return conn.(*net.TCPConn), bufio.NewReader(conn)
}

func controlWire(t *testing.T, conn net.Conn, method, path, framing, body string) {
	t.Helper()
	_, err := fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: fixture\r\nAuthorization: Bearer synthetic-management-key\r\nContent-Type: application/json\r\n%s\r\n%s", method, path, framing, body)
	if err != nil {
		t.Fatal(err)
	}
}

func controlReply(t *testing.T, reader *bufio.Reader, status int, code string) *http.Response {
	t.Helper()
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != status || (code != "" && !strings.Contains(string(body), `"error":"`+code+`"`)) {
		t.Fatalf("response: %d %s; want %d %s", response.StatusCode, body, status, code)
	}
	return response
}

func nextRejectionObservation(t *testing.T, observations <-chan rejectionObservation) rejectionObservation {
	t.Helper()
	select {
	case o := <-observations:
		return o
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not finish within test bound")
		return rejectionObservation{}
	}
}

func TestControlRejectionTCPDuringSSENaturalEOF(t *testing.T) {
	entered, finish := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(finish) })
	var mutations atomic.Int32
	forwarded := make(chan string, 8)
	var policy *resourcepolicy.Gate
	router, _, inst, preset := admissionServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models/load" || r.URL.Path == "/models/unload" {
			body, _ := io.ReadAll(r.Body)
			forwarded <- string(body)
			mutations.Add(1)
			_, _ = io.WriteString(w, `{}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		close(entered)
		select {
		case <-finish:
		case <-r.Context().Done():
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}), func(h *Handler) { policy = h.cfg.ResourcePolicy })
	s, observed := rejectionTCPServer(t, router)
	client := s.Client()
	client.Timeout = 5 * time.Second
	r, _ := http.NewRequest("POST", s.URL+"/api/v1/instances/admitted/proxy/completion", strings.NewReader(`{"stream":true}`))
	r.Header.Set("Authorization", "Bearer synthetic-management-key")
	stream, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != 200 {
		t.Fatalf("SSE status: %d", stream.StatusCode)
	}
	streamReader := bufio.NewReader(stream.Body)
	line, err := streamReader.ReadString('\n')
	if err != nil || line != "data: first\n" {
		t.Fatalf("SSE first frame: %q %v", line, err)
	}
	awaitAdmission(t, entered)
	baseline := policy.Active()
	oldOptions := inst.GetOptions()
	oldPreset, err := os.ReadFile(preset)
	if err != nil {
		t.Fatal(err)
	}
	optionsBody, err := json.Marshal(oldOptions)
	if err != nil {
		t.Fatal(err)
	}
	controlOptions := string(optionsBody) + strings.Repeat(" ", 2048-len(optionsBody))
	const modelBody = " {\"model\" : \"synthetic\"} \n"
	for _, a := range []struct{ method, path, body string }{
		{"POST", "/api/v1/llama-cpp/admitted/models/synthetic/load", modelBody},
		{"POST", "/api/v1/llama-cpp/admitted/models/synthetic/unload", modelBody},
		{"POST", "/api/v1/instances/admitted/proxy/models/load", modelBody},
		{"POST", "/api/v1/instances/admitted/proxy/models/unload", modelBody},
		{"POST", "/api/v1/instances/admitted/start", modelBody},
		{"POST", "/api/v1/instances/admitted/stop", modelBody},
		{"POST", "/api/v1/instances/admitted/restart", modelBody},
		{"DELETE", "/api/v1/instances/admitted", modelBody},
		{"PUT", "/api/v1/instances/admitted", controlOptions},
		{"POST", "/api/v1/instances/admitted", controlOptions},
	} {
		t.Run(a.method+a.path, func(t *testing.T) {
			conn, br := controlConn(t, s)
			controlWire(t, conn, a.method, a.path, fmt.Sprintf("Content-Length: %d\r\nConnection: close\r\n", len(a.body)), a.body)
			controlReply(t, br, 409, "busy")
			o := nextRejectionObservation(t, observed)
			if o.bytes != len(a.body) {
				t.Errorf("consumed %d of %d bytes", o.bytes, len(a.body))
			}
			if policy.Active() != baseline {
				t.Errorf("rejected control leaked policy permit")
			}
		})
	}
	current, err := os.ReadFile(preset)
	if err != nil || string(current) != string(oldPreset) || inst.GetOptions() != oldOptions || inst.GetStatus() != instance.Running || mutations.Load() != 0 {
		t.Fatal("SSE rejection changed instance/backend state")
	}
	// An accepted stream outlives the control cleanup budget unchanged.
	time.Sleep(150 * time.Millisecond)
	once.Do(func() { close(finish) })
	rest, err := io.ReadAll(streamReader)
	if err != nil || !strings.Contains(string(rest), "data: [DONE]\n\n") {
		t.Fatalf("natural SSE EOF: %q %v", rest, err)
	}
	nextRejectionObservation(t, observed)
	if policy.Active() != 0 {
		t.Fatalf("permit retained after natural EOF: %d", policy.Active())
	}
	lease, err := inst.BeginMaintenance()
	if err != nil {
		t.Fatal(err)
	}
	lease.Close()
	for _, path := range []string{"/api/v1/llama-cpp/admitted/models/synthetic/load", "/api/v1/llama-cpp/admitted/models/synthetic/unload", "/api/v1/instances/admitted/proxy/models/load", "/api/v1/instances/admitted/proxy/models/unload"} {
		conn, br := controlConn(t, s)
		controlWire(t, conn, "POST", path, fmt.Sprintf("Content-Length: %d\r\nConnection: close\r\n", len(modelBody)), modelBody)
		controlReply(t, br, 200, "")
		nextRejectionObservation(t, observed)
		select {
		case got := <-forwarded:
			if got != modelBody {
				t.Fatalf("accepted body rewritten: %q", got)
			}
		case <-time.After(time.Second):
			t.Fatal("not forwarded")
		}
	}
	// Management decoding still works, including zero/false configuration data.
	inst.SetStatus(instance.Stopped)
	payload, _ := json.Marshal(inst.GetOptions())
	conn, br := controlConn(t, s)
	controlWire(t, conn, "PUT", "/api/v1/instances/admitted", fmt.Sprintf("Content-Length: %d\r\nConnection: close\r\n", len(payload)), string(payload))
	controlReply(t, br, 200, "")
	nextRejectionObservation(t, observed)
}

func TestControlRejectionTCPKeepAliveAndPipeline(t *testing.T) {
	router, _, inst, _ := admissionServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{}`) }))
	lease, err := inst.BeginMaintenance()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	s, observed := rejectionTCPServer(t, router)
	conn, br := controlConn(t, s)
	path := "/api/v1/llama-cpp/admitted/models/synthetic/load"
	body := strings.Repeat(" ", 4096)
	controlWire(t, conn, "POST", path, "Content-Length: 4096\r\n", body)
	// Pipeline a read-only management request immediately after the body.
	controlWire(t, conn, "GET", "/api/v1/instances/admitted", "Content-Length: 0\r\n", "")
	if controlReply(t, br, 409, "busy").Close {
		t.Fatal("complete small body lost keepalive")
	}
	controlReply(t, br, 200, "")
	if o := nextRejectionObservation(t, observed); o.bytes != 4096 {
		t.Fatalf("boundary read: %+v", o)
	}
	nextRejectionObservation(t, observed)
	// Wait beyond cleanup's deadline and use the SAME connection again.
	time.Sleep(200 * time.Millisecond)
	controlWire(t, conn, "GET", "/api/v1/instances/admitted", "Content-Length: 0\r\nConnection: close\r\n", "")
	controlReply(t, br, 200, "")
	nextRejectionObservation(t, observed)
}

// Out-of-budget bodies do not have a delivery guarantee, particularly when a
// client keeps writing after rejection. Require a bounded closed connection and
// check the existing response whenever it is received, not a platform-specific
// FIN/RST outcome.
func fallbackReply(t *testing.T, conn net.Conn, br *bufio.Reader) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(br, nil)
	if err == nil {
		if response.StatusCode != 409 {
			t.Fatalf("fallback status: %d", response.StatusCode)
		}
		body, bodyErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if bodyErr == nil && !strings.Contains(string(body), `"error":"busy"`) {
			t.Fatalf("fallback JSON: %s", body)
		}
		err = bodyErr
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("fallback response blocked: %v", err)
	}
	if err != nil {
		t.Logf("out-of-budget transport ended without complete response: %v", err)
	}
	_, err = br.ReadByte()
	if err == nil {
		t.Fatal("fallback connection remained reusable")
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("post-handler body close blocked: %v", err)
	}
}

func TestControlAcceptedSlowBodiesUnchanged(t *testing.T) {
	forwarded := make(chan string, 2)
	router, _, _, _ := admissionServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		forwarded <- string(body)
		_, _ = io.WriteString(w, `{}`)
	}))
	s, observed := rejectionTCPServer(t, router)
	for _, a := range []struct{ path, body string }{
		{"/api/v1/instances/admitted/proxy/completion", `{"prompt":"` + strings.Repeat("x", 5000) + `"}`},
		{"/api/v1/llama-cpp/admitted/models/synthetic/load", `{"model":"synthetic"}` + strings.Repeat(" ", 5000)},
	} {
		conn, br := controlConn(t, s)
		controlWire(t, conn, "POST", a.path, fmt.Sprintf("Content-Length: %d\r\nConnection: close\r\n", len(a.body)), a.body[:10])
		time.Sleep(200 * time.Millisecond)
		if _, err := io.WriteString(conn, a.body[10:]); err != nil {
			t.Fatal(err)
		}
		controlReply(t, br, 200, "")
		o := nextRejectionObservation(t, observed)
		if o.bytes != len(a.body) || o.elapsed < 150*time.Millisecond {
			t.Fatalf("accepted body behavior: %+v", o)
		}
		select {
		case got := <-forwarded:
			if got != a.body {
				t.Fatal("accepted slow body changed")
			}
		case <-time.After(time.Second):
			t.Fatal("no forwarding")
		}
	}
}

type noReadDeadlineWriter struct{ http.ResponseWriter }

func (w noReadDeadlineWriter) SetReadDeadline(time.Time) error { return http.ErrNotSupported }
func (w noReadDeadlineWriter) Unwrap() http.ResponseWriter     { return w.ResponseWriter }

func TestControlRejectionTCPUnsupportedDeadline(t *testing.T) {
	router, _, inst, _ := admissionServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("rejected control reached backend") }))
	lease, err := inst.BeginMaintenance()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	s, observed := rejectionTCPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		router.ServeHTTP(noReadDeadlineWriter{w}, r)
	}))
	conn, br := controlConn(t, s)
	controlWire(t, conn, "POST", "/api/v1/llama-cpp/admitted/models/synthetic/load", "Content-Length: 25\r\n", "")
	controlReply(t, br, 409, "busy")
	o := nextRejectionObservation(t, observed)
	if o.bytes != 0 || o.elapsed > time.Second {
		t.Fatalf("unsupported deadline read or waited: %+v", o)
	}
	_, err = br.ReadByte()
	if err == nil {
		t.Fatal("unsupported deadline reused incomplete connection")
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("post-handler Close blocked without deadline: %v", err)
	}
}

func TestControlRejectionTCPSlowBody(t *testing.T) {
	router, _, inst, _ := admissionServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("slow rejected control reached backend") }))
	lease, err := inst.BeginMaintenance()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	s, observed := rejectionTCPServer(t, router)
	conn, br := controlConn(t, s)
	controlWire(t, conn, "POST", "/api/v1/llama-cpp/admitted/models/synthetic/load", "Content-Length: 25\r\n", "a")
	ticker := time.NewTicker(15 * time.Millisecond)
	defer ticker.Stop()
	safety := time.NewTimer(time.Second)
	defer safety.Stop()
	sent := 1
	for {
		select {
		case o := <-observed:
			if o.elapsed < 75*time.Millisecond || o.elapsed > time.Second || o.bytes < 1 || o.bytes > sent || o.bytes >= 25 {
				t.Fatalf("slow body reset total deadline: %+v sent=%d", o, sent)
			}
			fallbackReply(t, conn, br)
			return
		case <-ticker.C:
			if _, err := io.WriteString(conn, "x"); err == nil {
				sent++
			}
		case <-safety.C:
			t.Fatal("trickle extended total cleanup deadline")
		}
	}
}

func TestControlRejectionTCPFallbacks(t *testing.T) {
	router, _, inst, _ := admissionServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("fallback reached backend") }))
	lease, err := inst.BeginMaintenance()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	s, observed := rejectionTCPServer(t, router)
	for _, tc := range []struct {
		name, framing, body   string
		want                  int
		halfClose, disconnect bool
	}{
		{"oversize", "Content-Length: 4097\r\n", "", 0, false, false},
		{"chunked", "Transfer-Encoding: chunked\r\n", "1\r\nx\r\n", 0, false, false},
		{"expect", "Content-Length: 25\r\nExpect: 100-continue\r\n", "", 0, false, false},
		{"incomplete", "Content-Length: 25\r\n", "abc", 3, false, false},
		{"truncated", "Content-Length: 25\r\n", "abc", 3, true, false},
		{"disconnected", "Content-Length: 25\r\n", "abc", 3, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, br := controlConn(t, s)
			controlWire(t, conn, "POST", "/api/v1/llama-cpp/admitted/models/synthetic/load", tc.framing, tc.body)
			if tc.halfClose {
				if err := conn.CloseWrite(); err != nil {
					t.Fatal(err)
				}
			}
			if tc.disconnect {
				_ = conn.Close()
			} else {
				fallbackReply(t, conn, br)
			}
			o := nextRejectionObservation(t, observed)
			if o.bytes != tc.want || o.elapsed > time.Second {
				t.Fatalf("fallback bounds: %+v; want bytes=%d, elapsed<1s", o, tc.want)
			}
			if tc.name == "incomplete" && o.elapsed < 75*time.Millisecond {
				t.Fatalf("did not attempt bounded read: %+v", o)
			}
		})
	}
}
