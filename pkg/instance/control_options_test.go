package instance

import (
	"encoding/json"
	"testing"
)

func TestP3ProtocolOptionsRejectUnsafe(t *testing.T) {
	for _, control := range []string{
		`{"models":{"safe":{"path":"synthetic.gguf","parameters":{"context":-1}}}}`,
		`{"models":{"safe":{"path":"synthetic.gguf","parameters":{"flash_attention":null}}}}`,
		`{"models":{"safe":{"path":"synthetic.gguf","parameters":{"extra_args":"--unsafe"}}}}`,
		`{"models":{"bad\n[section]":{"path":"synthetic.gguf"}}}`,
	} {
		t.Run(control, func(t *testing.T) {
			cfg, opts := policyFixture(t, "isolated")
			body := `{"controlled":true,"environment_policy":"isolated","backend_type":"llama_cpp","backend_options":{"no_models_autoload":true,"models_max":1,"port":12345},"control":` + control + `}`
			if err := json.Unmarshal([]byte(body), opts); err != nil {
				return
			}
			if _, err := NewDescriptor("protocol", cfg, opts, nil); err == nil {
				t.Fatal("unsafe protocol configuration silently accepted")
			}
		})
	}
}
