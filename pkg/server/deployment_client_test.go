package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeploymentStdlibExampleRealRouter(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python 3 unavailable; example not exercised")
	}
	script, err := filepath.Abs("../../examples/deployment_test.py")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"success", "error"} {
		t.Run(mode, func(t *testing.T) {
			f := newDeploymentFixture(t, mode, false)
			cmd := exec.Command(python, script, "--base-url", f.front.URL, "--instance", "router", "--model", "alpha", "--measured-generations", "1", "--format", "csv")
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "PYTHONDONTWRITEBYTECODE=1", "LLAMACTL_CONTROL_KEY=synthetic-management-key"}
			out, err := cmd.CombinedOutput()
			if (err == nil) != (mode == "success") {
				t.Fatalf("example %s: %v %s", mode, err, out)
			}
			if !strings.Contains(string(out), "field,value") || !strings.Contains(string(out), "Retain original submit envelope") {
				t.Fatalf("example missing actual report/envelope: %s", out)
			}
			if strings.Contains(string(out), "secret-backend-body") || strings.Contains(string(out), "synthetic-not-a-model.gguf") || strings.Contains(string(out), "ready_token") {
				t.Fatal("example leaked private data")
			}
			if f.count("/v1/chat/completions") != 1 || f.count("/models/unload") != 1 {
				t.Fatal("example duplicated or failed to clean its single request")
			}
		})
	}
}
