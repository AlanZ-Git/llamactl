package instance

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"time"
)

func ServiceRequestDeadline(r *http.Request, absolute time.Time) (time.Time, error) {
	deadline := time.Now().Add(600 * time.Second)
	if !absolute.IsZero() && absolute.Before(deadline) {
		deadline = absolute
	}
	values := r.Header.Values("X-Request-Timeout-Seconds")
	if len(values) > 1 {
		return time.Time{}, controlError("invalid_deadline")
	}
	if len(values) == 1 {
		n, err := strconv.Atoi(values[0])
		if err != nil || n < 1 || n > 600 {
			return time.Time{}, controlError("invalid_deadline")
		}
		if d := time.Now().Add(time.Duration(n) * time.Second); d.Before(deadline) {
			deadline = d
		}
	}
	return deadline, nil
}

// ReadServiceBody performs the read on the handler goroutine. Real HTTP requests
// use a connection read deadline to interrupt a trickled body (Body.Close alone
// can wait on net/http's read mutex). In-memory bodies must support concurrent
// Close, as required by http.Request. The cancellation callback is joined.
func ReadServiceBody(w http.ResponseWriter, r *http.Request, ctx context.Context) ([]byte, error) {
	if r.Body == nil {
		return nil, controlError("request_contract_mismatch")
	}
	body := r.Body
	var controller *http.ResponseController
	if w != nil {
		controller = http.NewResponseController(w)
	}
	deadline, _ := ctx.Deadline()
	deadlineSupported := controller != nil && controller.SetReadDeadline(deadline) == nil
	interrupted := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(interrupted)
		if deadlineSupported {
			_ = controller.SetReadDeadline(time.Now())
		} else {
			_ = body.Close()
		}
	})
	b, err := io.ReadAll(io.LimitReader(body, (1<<20)+1))
	if !stop() {
		<-interrupted
	}
	if deadlineSupported && (err != nil || len(b) > 1<<20 || ctx.Err() != nil) {
		_ = controller.SetReadDeadline(time.Now())
	}
	_ = body.Close()
	if deadlineSupported {
		_ = controller.SetReadDeadline(time.Time{})
	}
	if ctx.Err() != nil {
		return nil, controlError("deadline_exceeded")
	}
	if err != nil || len(b) > 1<<20 {
		return nil, controlError("request_contract_mismatch")
	}
	return b, nil
}
