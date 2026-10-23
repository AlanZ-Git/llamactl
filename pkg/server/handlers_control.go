package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"llamactl/pkg/auth"
	"llamactl/pkg/instance"
	"llamactl/pkg/manager"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// controlIdentity authenticates this narrow resource even if legacy management
// authentication is disabled. Client-supplied scope headers have no authority.
func (h *Handler) controlIdentity(w http.ResponseWriter, r *http.Request, inst *instance.Instance) (string, bool, bool) {
	a := h.authMiddleware
	if a == nil {
		writeError(w, 401, "authentication_required", "control requires authentication")
		return "", false, false
	}
	if sess := sessionFromRequest(r, a.sessions); sess != nil {
		return "session:" + sess.Sub, true, true
	}
	key := a.extractAPIKey(r)
	if key != "" && a.isValidManagementKey(key) {
		digest := sha256.Sum256([]byte(key))
		return "admin:" + hex.EncodeToString(digest[:]), true, true
	}
	if key != "" && a.authStore != nil {
		keys, err := a.authStore.GetActiveKeys(r.Context())
		if err == nil {
			for _, k := range keys {
				if auth.VerifyKey(key, k.KeyHash) {
					if !k.Controller || k.PermissionMode != auth.PermissionModePerInstance {
						writeError(w, 403, "controller_required", "inference keys cannot control instances")
						return "", false, false
					}
					perms, err := a.authStore.GetPermissions(r.Context(), k.ID)
					if err != nil || len(perms) != 1 || perms[0].InstanceID != inst.ID || perms[0].CanStart || perms[0].CanEvict {
						writeError(w, 403, "permission_denied", "controller is restricted to its fixed instance")
						return "", false, false
					}
					return fmt.Sprintf("controller:%d", k.ID), false, true
				}
			}
		}
	}
	writeError(w, 401, "authentication_required", "valid management or scoped-controller credential required")
	return "", false, false
}
func writeControlError(w http.ResponseWriter, err error) {
	status, code, ok := instance.AdmissionHTTPError(err)
	if !ok {
		status, code = 500, "control_failed"
	}
	switch code {
	case "administrator_required":
		status = 403
	case "instance_not_found", "operation_not_found", "control_not_enabled", "test_unknown_not_resumable", "n1_not_found":
		status = 404
	case "invalid_config", "unsupported_parameter", "invalid_deadline", "invalid_test_budget", "parameter_range_mismatch", "invalid_findings", "invalid_quality_claim", "candidate_coverage_mismatch", "receipt_coverage_mismatch", "invalid_attempt_reference":
		status = 422
	case "operation_capacity", "mapping_capacity":
		status = 429
	}
	writeError(w, status, code, code)
}
func (h *Handler) ControlHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cm, ok := h.InstanceManager.(manager.ControlManager)
		if !ok {
			writeError(w, 503, "control_unavailable", "control unavailable")
			return
		}
		inst, err := cm.ControlInstance(chi.URLParam(r, "name"))
		if err != nil {
			writeError(w, 404, "instance_not_found", "instance not found")
			return
		}
		scope, admin, ok := h.controlIdentity(w, r, inst)
		if !ok {
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		switch {
		case r.Method == "GET" && chi.URLParam(r, "id") != "":
			op, err := cm.GetControlOperation(inst.Name, scope, chi.URLParam(r, "id"), admin)
			if err != nil {
				writeControlError(w, err)
				return
			}
			writeJSON(w, 200, op)
		case r.Method == "GET":
			s, err := cm.ControlStatus(inst.Name)
			if err != nil {
				writeControlError(w, err)
				return
			}
			writeJSON(w, 200, s)
		case r.Method == "PUT":
			if !admin {
				writeError(w, 403, "administrator_required", "recovery requires administrator verification")
				return
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10+1))
			if err != nil || len(body) > 64<<10 {
				writeError(w, 422, "invalid_config", "invalid recovery")
				return
			}
			f, err := instance.ControlObject(body, "boot_id", "expected_revision", "verified_stopped")
			var boot string
			var revision uint64
			var verified bool
			if err != nil || len(f) != 3 || json.Unmarshal(f["boot_id"], &boot) != nil || json.Unmarshal(f["expected_revision"], &revision) != nil || json.Unmarshal(f["verified_stopped"], &verified) != nil || !verified || boot == "" || string(f["expected_revision"]) == "null" {
				writeError(w, 422, "invalid_config", "explicit verified_stopped=true, boot_id and revision required")
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			s, err := cm.RecoverControl(ctx, inst.Name, boot, revision)
			if err != nil {
				writeControlError(w, err)
				return
			}
			writeJSON(w, 200, s)
		case r.Method == "POST":
			if r.Header.Get("Content-Encoding") != "" {
				writeError(w, 422, "invalid_config", "encoded operations unsupported")
				return
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10+1))
			if err != nil || len(body) > 64<<10 {
				writeError(w, 422, "invalid_config", "operation exceeds 64 KiB")
				return
			}
			op, err := cm.SubmitControl(inst.Name, scope, body)
			if err != nil {
				writeControlError(w, err)
				return
			}
			w.Header().Set("Location", "/api/v1/instances/"+inst.Name+"/operations/"+op.ID)
			writeJSON(w, 202, op)
		}
	}
}
