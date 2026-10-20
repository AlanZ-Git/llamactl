package server

import (
	"io"
	"llamactl/pkg/manager"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

func (h *Handler) DeploymentTests() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cm, ok := h.InstanceManager.(manager.ControlManager)
		tm, testsOK := h.InstanceManager.(manager.DeploymentTestManager)
		if !ok || !testsOK {
			writeError(w, 503, "control_unavailable", "control unavailable")
			return
		}
		name, id := chi.URLParam(r, "name"), chi.URLParam(r, "id")
		inst, err := cm.ControlInstance(name)
		if err != nil {
			writeControlError(w, err)
			return
		}
		scope, admin, ok := h.controlIdentity(w, r, inst)
		if !ok {
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if _, err := cm.ControlStatus(name); err != nil {
			writeControlError(w, err)
			return
		}
		if r.Method == http.MethodGet {
			format := r.URL.Query().Get("format")
			if format != "" && format != "json" && format != "csv" {
				writeError(w, 422, "invalid_config", "format must be json or csv")
				return
			}
			report, err := tm.GetDeploymentTest(name, scope, id, admin)
			if err != nil {
				writeControlError(w, err)
				return
			}
			if format == "csv" {
				w.Header().Set("Content-Type", "text/csv; charset=utf-8")
				w.Header().Set("Content-Disposition", `attachment; filename="deployment-test.csv"`)
				w.Header().Set("X-Content-Type-Options", "nosniff")
				_ = manager.WriteDeploymentCSV(w, report)
			} else {
				writeJSON(w, 200, report)
			}
			return
		}
		if r.Header.Get("Content-Encoding") != "" {
			writeError(w, 422, "invalid_config", "encoded tests unsupported")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, (64<<10)+1))
		if err != nil || len(body) > 64<<10 {
			writeError(w, 422, "invalid_config", "test exceeds 64 KiB")
			return
		}
		var report manager.DeploymentTest
		if id != "" {
			report, err = tm.CancelDeploymentTest(name, scope, id, admin, body)
		} else {
			report, err = tm.SubmitDeploymentTest(name, scope, body)
		}
		if err != nil {
			writeControlError(w, err)
			return
		}
		if id != "" {
			writeJSON(w, 200, report)
			return
		}
		w.Header().Set("Location", strings.TrimSuffix(r.URL.Path, "/")+"/"+report.ID)
		writeJSON(w, 202, report)
	}
}
