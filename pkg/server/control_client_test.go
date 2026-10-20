package server

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runP3Example(t *testing.T, origin, instanceName string, stream bool) ([]byte, error) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python 3 unavailable; stdlib example not executed")
	}
	script, err := filepath.Abs("../../examples/control_client.py")
	if err != nil {
		t.Fatal(err)
	}
	args := []string{script, "--base-url", origin, "--instance", instanceName, "--model", "alpha", "--patch", `{"context":2048,"gpu_layers":0,"flash_attention":false}`, "--http-timeout", "5"}
	if stream {
		args = append(args, "--stream")
	}
	cmd := exec.Command(python, args...)
	// No inherited model credentials, proxy settings or Python startup overrides.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "PYTHONDONTWRITEBYTECODE=1", "LLAMACTL_CONTROL_KEY=synthetic-management-key"}
	return cmd.CombinedOutput()
}

func TestP3ExampleDoesNotRecoverBoot(t *testing.T) {
	router, _, _ := p3HTTPFixture(t)
	front := httptest.NewServer(router)
	defer front.Close()
	out, err := runP3Example(t, front.URL, "protocol", false)
	if err == nil || !strings.Contains(string(out), "administrator/owner action required") {
		t.Fatalf("example blindly recovered/replayed: %s %v", out, err)
	}
}
