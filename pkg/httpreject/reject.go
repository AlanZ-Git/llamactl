// Package httpreject settles small HTTP/1 control bodies only after admission
// has rejected a request. It is not request-size middleware or a JSON validator.
package httpreject

import (
	"context"
	"io"
	"net/http"
	"time"
)

// These are cleanup budgets, not limits on accepted requests. 4 KiB comfortably
// covers model-name controls; larger configuration bodies are not drained.
const (
	maxBody     = 4 << 10
	readTimeout = 100 * time.Millisecond
)

type pendingKey struct{}
type pending struct{ write func() }

// Handler establishes a rejection-only response boundary. Callers opt in only
// maintenance handlers; Defer must be used only for local admission errors.
// No response writer or accepted request body is wrapped or buffered. Waiting
// until next returns also lets all its leases and permits unwind before I/O.
func Handler(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 {
			next(w, r)
			return
		}
		p := new(pending)
		// Keep the original wire body even if a decoder/proxy replaces r.Body.
		next(w, r.WithContext(context.WithValue(r.Context(), pendingKey{}, p)))
		if p.write == nil {
			return
		}
		if settle(w, r) {
			p.write()
			return
		}
		// Do not let net/http's response-time drain take over this budget.
		// Identity framing allows a complete close-delimited JSON response even
		// when Hijack bypasses finishRequest (and its chunk terminator).
		w.Header().Set("Connection", "close")
		w.Header().Set("Transfer-Encoding", "identity")
		controller := http.NewResponseController(w)
		_ = controller.SetReadDeadline(time.Now())
		_ = controller.SetWriteDeadline(time.Now().Add(readTimeout))
		p.write()
		_ = controller.Flush()
		// finishRequest calls the ORIGINAL body.Close, which can drain another
		// 256 KiB, even with Connection: close on the response. On HTTP/1 the
		// native writer supports Hijack: close without calling Body.Close.
		if conn, _, err := controller.Hijack(); err == nil {
			_ = conn.Close()
		}
		// Opaque/custom writers may support neither deadlines nor Hijack. Never
		// read or Close a body without a deadline; their host owns teardown.
	}
}

// Defer records an already-decided admission response for the enclosing Handler.
// It returns false outside that boundary so existing standalone callers retain
// their normal response behavior. It does not change admission or error mapping.
func Defer(r *http.Request, write func()) bool {
	p, ok := r.Context().Value(pendingKey{}).(*pending)
	if !ok {
		return false
	}
	p.write = write
	return true
}

func settle(w http.ResponseWriter, r *http.Request) bool {
	if r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
		return true
	}
	if r.ContentLength < 0 || r.ContentLength > maxBody || len(r.TransferEncoding) != 0 || r.Header.Get("Expect") != "" {
		return false
	}
	controller := http.NewResponseController(w)
	deadline := time.Now().Add(readTimeout)
	if err := controller.SetReadDeadline(deadline); err != nil {
		return false
	}
	var buf [1024]byte
	remaining := r.ContentLength
	for remaining > 0 {
		if !time.Now().Before(deadline) || r.Context().Err() != nil {
			return false
		}
		n, err := r.Body.Read(buf[:min(int64(len(buf)), remaining)])
		remaining -= int64(n)
		if err != nil && err != io.EOF {
			return false
		}
		// EOF can also mean a management JSON decoder already consumed part
		// or all of the wire body. net/http reports truncation as UnexpectedEOF.
		if err == io.EOF || remaining == 0 {
			return controller.SetReadDeadline(time.Time{}) == nil
		}
		if n == 0 {
			return false
		}
	}
	return false
}
