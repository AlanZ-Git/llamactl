//go:build !linux && !windows

package instance

import (
	"context"
	"llamactl/pkg/resources"
	"time"
)

func (t *ownedTree) sampleResources(context.Context) resources.ProcessReading {
	return resources.ProcessReading{At: time.Now(), Source: "unsupported_platform", Coverage: "unavailable", Reason: "owned_process_metrics_unsupported"}
}
