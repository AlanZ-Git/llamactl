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

func (h *Handler) releasePolicy(w http.ResponseWriter, gate *resourcepolicy.Gate, data []byte) {
	fields, err := resourcepolicy.DecodeObject(data, "boot_id", "expected_revision", "request_id", "mode")
	if err != nil {
		status := http.StatusBadRequest
		if strings.HasPrefix(err.Error(), "unknown field ") {
			status = http.StatusUnprocessableEntity
		}
		writeError(w, status, "invalid_release_request", err.Error())
		return
	}
	var boot, id, mode string
	var revision uint64
	if json.Unmarshal(fields["boot_id"], &boot) != nil || boot == "" || json.Unmarshal(fields["expected_revision"], &revision) != nil || json.Unmarshal(fields["request_id"], &id) != nil || !resourcepolicy.ValidReleaseID(id) || json.Unmarshal(fields["mode"], &mode) != nil || (mode != "graceful" && mode != "immediate") {
		writeError(w, http.StatusBadRequest, "invalid_release_request", "Required types: nonempty boot_id, uint64 expected_revision, request_id of 1-128 ASCII letters/digits/dot/underscore/hyphen, mode graceful or immediate")
		return
	}
	snapshot, err := gate.RequestRelease(boot, revision, id, mode)
	status := http.StatusAccepted
	if errors.Is(err, resourcepolicy.ErrConflict) {
		status = http.StatusConflict
	} else if errors.Is(err, resourcepolicy.ErrReleaseCapacity) {
		status = http.StatusServiceUnavailable
	} else if err != nil {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, snapshot)
}

func (h *Handler) ResourcePolicy() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
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
		if r.Method == http.MethodPost {
			h.releasePolicy(w, gate, data)
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
