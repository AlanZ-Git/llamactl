//go:build !linux && !windows

package resources

import (
	"context"
	"time"
)

func collectSystem(context.Context) SystemReading {
	return SystemReading{At: time.Now(), Source: "unsupported_platform", Reason: "native_collector_unsupported", MemoryReason: "native_collector_unsupported", PhysicalReason: "native_collector_unsupported"}
}
