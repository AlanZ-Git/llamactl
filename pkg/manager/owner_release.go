package manager

import (
	"llamactl/pkg/resourcepolicy"
	"time"
)

func (im *instanceManager) releaseOwned(ticket *resourcepolicy.ReleaseTicket) {
	ctx := ticket.Context()
	if ticket.Mode() == "immediate" {
		if err := ticket.CancelWork(); err != nil {
			ticket.Finish("unreleased_unknown", "release_timeout")
			return
		}
	} else {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for !ticket.Drained() {
			select {
			case <-ctx.Done():
				ticket.Finish("unreleased_unknown", "drain_timeout")
				return
			case <-ticker.C:
			}
		}
	}
	ticket.Terminating()
	// list(), unlike ListInstances(), performs no remote refresh and no network IO.
	instances := im.registry.list()
	type result struct{ state, code string }
	results := make(chan result, len(instances))
	count := 0
	for _, inst := range instances {
		if inst.IsRemote() {
			continue
		}
		count++
		go func() { state, code := inst.ReleaseOwned(ticket); results <- result{state, code} }()
	}
	state, code := "released", ""
	for n := 0; n < count; n++ {
		select {
		case r := <-results:
			if r.state == "failed" || (r.state != "released" && state == "released") {
				state, code = r.state, r.code
			}
		case <-ctx.Done():
			ticket.Finish("unreleased_unknown", "release_timeout")
			return
		}
	}
	ticket.Finish(state, code)
}
