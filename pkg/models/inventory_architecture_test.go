package models

import "testing"

func TestN1FixedEngineQwen35Metadata(t *testing.T) {
	for _, arch := range []string{"qwen35", "qwen35moe"} {
		if !knownArchitecture(arch) {
			t.Errorf("supported fixed-engine metadata %s rejected", arch)
		}
	}
	if knownArchitecture("unknown_future_architecture") {
		t.Fatal("unknown metadata guessed supported")
	}
}
