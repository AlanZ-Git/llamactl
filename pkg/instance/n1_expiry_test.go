package instance

import (
	"context"
	"testing"
	"time"
)

func TestN1ExpiryIsNotContextCancellation(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		cancel, closing, ready, active  bool
		absolutePast, idlePast, expired bool
	}{
		{name: "live"},
		{name: "cancelled", cancel: true},
		{name: "closing", closing: true},
		{name: "absolute", absolutePast: true, expired: true},
		{name: "absolute_while_closing", closing: true, absolutePast: true, expired: true},
		{name: "absolute_with_active_request", ready: true, active: true, absolutePast: true, expired: true},
		{name: "idle", ready: true, idlePast: true, expired: true},
		{name: "idle_while_closing", closing: true, ready: true, idlePast: true, expired: true},
		{name: "active_request_is_not_idle", ready: true, active: true, idlePast: true},
		{name: "not_ready_is_not_idle", idlePast: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			now := time.Now()
			r := &Reservation{instance: &Instance{}, ctx: ctx, cancel: cancel, absolute: now.Add(time.Minute), idleUntil: now.Add(time.Minute), ready: tc.ready}
			if tc.absolutePast {
				r.absolute = now.Add(-time.Minute)
			}
			if tc.idlePast {
				r.idleUntil = now.Add(-time.Minute)
			}
			if tc.active {
				r.active = 1
			}
			if tc.cancel {
				cancel()
			}
			if tc.closing {
				r.BeginClosing()
			}
			if (tc.cancel || tc.closing) && ctx.Err() != context.Canceled {
				t.Fatal("probe did not cancel the reservation")
			}
			if got := r.Expired(); got != tc.expired {
				t.Fatalf("Expired()=%v, want %v (context=%v)", got, tc.expired, ctx.Err())
			}
		})
	}
}
