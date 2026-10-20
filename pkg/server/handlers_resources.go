package server

import (
	"llamactl/pkg/resources"
	"net/http"
)

// Resources is in the existing management-read group, not the scoped controller
// group. The cache belongs to the manager and is never refreshed by this handler.
func (h *Handler) Resources() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		provider, ok := h.InstanceManager.(interface {
			ResourceSnapshot() (resources.Snapshot, bool)
		})
		if !ok {
			writeError(w, http.StatusServiceUnavailable, "resources_unavailable", "Manager resource sampler unavailable")
			return
		}
		snapshot, ok := provider.ResourceSnapshot()
		if !ok {
			writeError(w, http.StatusServiceUnavailable, "resources_unavailable", "Manager resource sampler unavailable")
			return
		}
		writeJSON(w, http.StatusOK, snapshot)
	}
}
