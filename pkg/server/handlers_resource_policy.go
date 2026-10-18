package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"llamactl/pkg/resourcepolicy"
	"net/http"
	"strings"
)

// ownerAuthorized deliberately does not rely on optional management middleware,
// inference permissions, or the absence of an inference-key context value.
func (h *Handler) ownerAuthorized(r *http.Request) bool {
	if h.authMiddleware == nil {
		return false
	}
	if sessionFromRequest(r, h.authMiddleware.sessions) != nil {
		return true
	}
	provided := h.authMiddleware.extractAPIKey(r)
	if provided == "" {
		return false
	}
	for _, key := range h.cfg.Auth.ManagementKeys {
		if len(key) == len(provided) && subtle.ConstantTimeCompare([]byte(key), []byte(provided)) == 1 {
			return true
		}
	}
	return false
}

func (h *Handler) ResourcePolicy() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.ownerAuthorized(r) {
			writeError(w, http.StatusUnauthorized, "owner_auth_required", "Configured management key or authenticated owner session required")
			return
		}
		gate := h.cfg.ResourcePolicy
		if gate == nil {
			writeError(w, http.StatusServiceUnavailable, "policy_unavailable", "Owner policy was not initialized by the manager")
			return
		}
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, gate.Snapshot())
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, 4097))
		if err != nil || len(data) > 4096 {
			writeError(w, http.StatusBadRequest, "invalid_policy_request", "Policy JSON must be at most 4096 bytes")
			return
		}
		fields, err := resourcepolicy.DecodeObject(data, "allow_ai", "boot_id", "expected_revision")
		if err != nil {
			status := http.StatusBadRequest
			if strings.HasPrefix(err.Error(), "unknown field ") {
				status = http.StatusUnprocessableEntity
			}
			writeError(w, status, "invalid_policy_request", err.Error())
			return
		}
		var allow bool
		var boot string
		var revision uint64
		if json.Unmarshal(fields["allow_ai"], &allow) != nil || json.Unmarshal(fields["boot_id"], &boot) != nil || boot == "" || json.Unmarshal(fields["expected_revision"], &revision) != nil {
			writeError(w, http.StatusBadRequest, "invalid_policy_request", "Required types: allow_ai boolean, nonempty boot_id string, expected_revision unsigned integer")
			return
		}
		snapshot, err := gate.Update(allow, boot, revision)
		status := http.StatusOK
		if err != nil {
			status = http.StatusInternalServerError
			if errors.Is(err, resourcepolicy.ErrConflict) {
				status = http.StatusConflict
			}
		}
		// Policy writes always return exactly a snapshot; status distinguishes failure.
		writeJSON(w, status, snapshot)
	}
}
