package manager

import (
	"context"
	"llamactl/pkg/resources"
)

// ResourceSnapshot reads the manager's one shared sampler; no client request
// starts a collector, performs OS I/O, or changes admission.
func (im *instanceManager) ResourceSnapshot() (resources.Snapshot, bool) {
	if im.resources == nil {
		return resources.Snapshot{}, false
	}
	return im.resources.Snapshot(), true
}

func (im *instanceManager) resourcesIdle() bool {
	if im.globalConfig.ResourcePolicy != nil && !im.globalConfig.ResourcePolicy.Snapshot().AllowAI {
		return true
	}
	for _, inst := range im.registry.list() {
		if !inst.IsRemote() && inst.IsRunning() {
			return false
		}
	}
	return true
}

func (im *instanceManager) collectOwnedResources(ctx context.Context) ([]resources.ProcessReading, string) {
	out := make([]resources.ProcessReading, 0)
	coverage := "manager_owned_local_only"
	for _, inst := range im.registry.list() {
		if inst.IsRemote() {
			continue
		}
		if len(out) >= resources.MaxProcesses {
			coverage = "instance_limit"
			break
		}
		if ctx.Err() != nil {
			coverage = "collection_canceled"
			break
		}
		out = append(out, inst.SampleOwnedResources(ctx))
	}
	return out, coverage
}
