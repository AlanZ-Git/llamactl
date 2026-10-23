package server

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Reuses the existing synthetic Router process, real routes and isolated SQLite.
// This is also the independently runnable parent fixture: no production startup.
func TestN1PythonHTTPFixture(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("stdlib python3 unavailable; run fixture on development host")
	}
	f := newN1Fixture(t)
	f.cfg.N1.DeliveryTTLSeconds = 1 // Narrow synthetic expiry test; product default is five minutes.
	second := n1SyntheticGGUF()
	second[len(second)-1] ^= 1 // Different synthetic tensor bytes, not a real weight.
	if err := os.WriteFile(filepath.Join(f.cfg.N1.Roots["fixture"], "second.gguf"), second, 0600); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("../../tools/evaluator/synthetic_http.py")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	key, _ := n1KeyShape(t, f, "synthetic-python-client", []string{"discover", "service_use", "inference"}, false)
	evaluatorKey, _ := n1KeyShape(t, f, "synthetic-evaluator", []string{"discover", "evaluate", "recipe_write", "recipe_publish", "observe"}, false)
	cmd := exec.CommandContext(ctx, python, script, f.front.URL, t.TempDir(), key, evaluatorKey)
	output, err := cmd.CombinedOutput()
	t.Logf("Python real HTTP fixture:\n%s", output)
	if err != nil {
		t.Fatal(err)
	}
}
