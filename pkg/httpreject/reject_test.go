package httpreject

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type countedBody struct {
	reader       io.Reader
	bytes, reads int
}

func (b *countedBody) Read(p []byte) (int, error) {
	b.reads++
	n, err := b.reader.Read(p)
	b.bytes += n
	return n, err
}
func (*countedBody) Close() error { panic("settlement must not call Body.Close") }

type deadlineWriter struct {
	*httptest.ResponseRecorder
	deadlines          []time.Time
	failSet, failClear bool
}

func (w *deadlineWriter) SetReadDeadline(d time.Time) error {
	w.deadlines = append(w.deadlines, d)
	if w.failSet || (d.IsZero() && w.failClear) {
		return http.ErrNotSupported
	}
	return nil
}

type unwrapWriter struct{ http.ResponseWriter }

func (w unwrapWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

func TestSettlementBounds(t *testing.T) {
	for _, tc := range []struct {
		name                                    string
		length                                  int64
		chunked, expect, unsupported, failClear bool
		want                                    int
		close                                   bool
	}{
		{name: "empty", length: 0},
		{name: "small", length: 25, want: 25},
		{name: "below-boundary", length: 4095, want: 4095},
		{name: "boundary", length: 4096, want: 4096},
		{name: "oversize", length: 4097, close: true},
		{name: "unknown", length: -1, close: true},
		{name: "chunked", length: 25, chunked: true, close: true},
		{name: "expect-continue", length: 25, expect: true, close: true},
		{name: "unsupported-deadline", length: 25, unsupported: true, close: true},
		{name: "failed-clear", length: 25, want: 25, failClear: true, close: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &countedBody{reader: strings.NewReader(strings.Repeat("x", 8192))}
			r := httptest.NewRequest("POST", "/control", nil)
			r.Body, r.ContentLength = body, tc.length
			if tc.chunked {
				r.TransferEncoding = []string{"chunked"}
			}
			if tc.expect {
				r.Header.Set("Expect", "100-continue")
			}
			w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder(), failSet: tc.unsupported, failClear: tc.failClear}
			returned := false
			h := Handler(func(w http.ResponseWriter, r *http.Request) {
				defer func() { returned = true }()
				if !Defer(r, func() {
					if !returned {
						t.Error("response preceded handler defers")
					}
					w.WriteHeader(409)
					_, _ = io.WriteString(w, `{"error":"busy"}`)
				}) {
					t.Fatal("no rejection boundary")
				}
				if body.reads != 0 {
					t.Error("body read before handler returned")
				}
			})
			start := time.Now()
			h(unwrapWriter{w}, r)
			if body.bytes != tc.want {
				t.Errorf("read %d; want %d", body.bytes, tc.want)
			}
			if tc.want == 0 && body.reads != 0 {
				t.Errorf("unexpected body read")
			}
			if time.Since(start) > time.Second {
				t.Fatal("settlement did not return promptly")
			}
			if (w.Header().Get("Connection") == "close") != tc.close {
				t.Errorf("connection header: %v", w.Header())
			}
			if w.Code != 409 || w.Body.String() != `{"error":"busy"}` {
				t.Errorf("response changed: %d %s", w.Code, w.Body.String())
			}
			if tc.want > 0 && !tc.failClear {
				if len(w.deadlines) != 2 || !w.deadlines[1].IsZero() {
					t.Errorf("stale deadline: %v", w.deadlines)
				}
				if d := w.deadlines[0].Sub(start); d <= 0 || d > 150*time.Millisecond {
					t.Errorf("deadline budget: %v", d)
				}
			}
		})
	}
}

func TestSettlementReadFailures(t *testing.T) {
	for _, err := range []error{io.ErrUnexpectedEOF, errors.New("disconnected"), nil} {
		body := &countedBody{reader: errorReader{err}}
		r := httptest.NewRequest("POST", "/control", nil)
		r.Body, r.ContentLength = body, 25
		w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
		Handler(func(w http.ResponseWriter, r *http.Request) {
			Defer(r, func() { w.WriteHeader(409) })
		})(w, r)
		if body.reads != 1 || w.Header().Get("Connection") != "close" {
			t.Fatalf("err=%v reads=%d headers=%v", err, body.reads, w.Header())
		}
	}
}

func TestSettlementOpaqueWriterAndCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		body := &countedBody{reader: errorReader{errors.New("must not read")}}
		r := httptest.NewRequest("POST", "/control", nil)
		r.Body, r.ContentLength = body, 25
		w := httptest.NewRecorder()
		var writer http.ResponseWriter = struct{ http.ResponseWriter }{w}
		if canceled {
			ctx, cancel := context.WithCancel(r.Context())
			cancel()
			r = r.WithContext(ctx)
			writer = &deadlineWriter{ResponseRecorder: w}
		}
		Handler(func(w http.ResponseWriter, r *http.Request) { Defer(r, func() { w.WriteHeader(409) }) })(writer, r)
		if body.reads != 0 || w.Code != 409 || w.Header().Get("Connection") != "close" {
			t.Fatal("unsafe fallback")
		}
	}
}

func TestSettlementDoesNotWrapAcceptedBodyOrWriter(t *testing.T) {
	body := &countedBody{reader: strings.NewReader("unchanged")}
	r := httptest.NewRequest("POST", "/control", nil)
	r.Body, r.ContentLength = body, 9
	w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	Handler(func(got http.ResponseWriter, r *http.Request) {
		if got != w || r.Body != body {
			t.Fatal("accepted request was wrapped")
		}
		got.WriteHeader(202)
	})(w, r)
	if body.reads != 0 || len(w.deadlines) != 0 || w.Code != 202 {
		t.Fatal("accepted request changed")
	}
	if Defer(r, func() {}) {
		t.Fatal("standalone caller unexpectedly deferred")
	}
	r.ProtoMajor = 2
	Handler(func(w http.ResponseWriter, r *http.Request) {
		if Defer(r, func() {}) {
			t.Fatal("HTTP/2 opted into HTTP/1 settlement")
		}
	})(w, r)
}

func TestSettlementUsesOriginalBodyAfterHandlerReplacement(t *testing.T) {
	original := &countedBody{reader: strings.NewReader("already parsed")}
	r := httptest.NewRequest("POST", "/control", nil)
	r.Body, r.ContentLength = original, 14
	w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	Handler(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 7)
		_, _ = io.ReadFull(r.Body, buf)
		r.Body = &countedBody{reader: errorReader{errors.New("must not read replacement")}}
		Defer(r, func() { w.WriteHeader(409) })
	})(w, r)
	if original.bytes != 14 || w.Header().Get("Connection") != "" {
		t.Fatalf("original not settled: %d %v", original.bytes, w.Header())
	}
}
