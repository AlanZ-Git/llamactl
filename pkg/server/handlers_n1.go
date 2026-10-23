package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"llamactl/pkg/auth"
	"llamactl/pkg/instance"
	"llamactl/pkg/manager"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

func (h *Handler) n1Identity(w http.ResponseWriter, r *http.Request, inst *instance.Instance, capability string) (string, bool, bool) {
	a := h.authMiddleware
	if a == nil {
		writeError(w, 401, "authentication_required", "authentication required")
		return "", false, false
	}
	if session := sessionFromRequest(r, a.sessions); session != nil {
		return "session:" + session.Sub, true, true
	}
	key := a.extractAPIKey(r)
	if key != "" && a.isValidManagementKey(key) {
		sum := sha256.Sum256([]byte(key))
		return "admin:" + hex.EncodeToString(sum[:]), true, true
	}
	nm, ok := h.InstanceManager.(manager.N1Manager)
	if !ok {
		writeError(w, 503, "n1_unavailable", "N1 unavailable")
		return "", false, false
	}
	if a.authStore != nil && key != "" {
		keys, err := a.authStore.GetActiveKeys(r.Context())
		if err == nil {
			for _, k := range keys {
				if !auth.VerifyKey(key, k.KeyHash) {
					continue
				}
				allowed, err := nm.N1Capability(r.Context(), k.ID, inst.ID, capability)
				if err != nil || !allowed {
					writeError(w, 403, "capability_required", capability+" capability required")
					return "", false, false
				}
				return fmt.Sprintf("key:%d", k.ID), false, true
			}
		}
	}
	writeError(w, 401, "authentication_required", "valid credential required")
	return "", false, false
}
func n1RouteCapability(method, path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch parts[0] {
	case "observation":
		return "observe"
	case "inventory":
		if len(parts) > 1 && (parts[1] == "verify" || parts[1] == "register") {
			return "evaluate"
		}
		return "discover"
	case "recipes":
		if method == "GET" {
			return "discover"
		}
		if strings.HasSuffix(path, "/publish") || strings.HasSuffix(path, "/withdraw") {
			return "recipe_publish"
		}
		return "recipe_write"
	case "evaluation-runs":
		return "evaluate"
	case "service-uses":
		return "service_use"
	}
	return "discover"
}
func (h *Handler) N1Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		nm, ok := h.InstanceManager.(manager.N1Manager)
		if !ok {
			writeError(w, 503, "n1_unavailable", "N1 unavailable")
			return
		}
		cm, ok := h.InstanceManager.(manager.ControlManager)
		if !ok {
			writeError(w, 503, "n1_unavailable", "N1 unavailable")
			return
		}
		name := chi.URLParam(r, "name")
		inst, err := cm.ControlInstance(name)
		if err != nil {
			writeControlError(w, err)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/api/v1/instances/"+name+"/")
		scope, admin, ok := h.n1Identity(w, r, inst, n1RouteCapability(r.Method, path))
		if !ok {
			return
		}
		if r.Header.Get("Content-Encoding") != "" {
			writeError(w, 422, "invalid_config", "encoded requests unsupported")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20+1))
		if err != nil || len(body) > 1<<20 {
			writeError(w, 422, "invalid_config", "body limit")
			return
		}
		status, out, err := nm.N1Request(r.Context(), name, scope, admin, r.Method, path, r.URL.Query(), body)
		if err != nil {
			writeControlError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, status, out)
	}
}
func (h *Handler) authorizeN1Inference(w http.ResponseWriter, r *http.Request, inst *instance.Instance) bool {
	if r.Method != "POST" || (r.Header.Get("X-Service-Use") == "" && inst.GetOptions().ActiveRecipe == nil) {
		return true
	}
	scope, _, ok := h.n1Identity(w, r, inst, "inference")
	if !ok {
		return false
	}
	*r = *instance.WithServiceScope(r, scope)
	return true
}
func (h *Handler) serveN1Aware(inst *instance.Instance, w http.ResponseWriter, r *http.Request) error {
	if !h.authorizeN1Inference(w, r, inst) {
		return fmt.Errorf("inference capability required")
	}
	return inst.ServeHTTP(w, r)
}
