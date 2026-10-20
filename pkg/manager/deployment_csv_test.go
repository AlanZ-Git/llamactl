package manager

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"llamactl/pkg/instance"
	"testing"
)

func TestDeploymentCSVPreservesUnknownAndEscapesFormulaCells(t *testing.T) {
	report := DeploymentTest{Instance: "=synthetic", Parameters: map[string]json.RawMessage{"gpu_layers": json.RawMessage("0"), "flash_attention": json.RawMessage("false")}, Samples: []instance.GenerationObservation{{Kind: "initial", ResponseBytes: 0}}, Identity: DeploymentIdentity{EngineVersion: "unknown"}}
	var buffer bytes.Buffer
	if err := WriteDeploymentCSV(&buffer, report); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&buffer).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]string{}
	for _, row := range rows[1:] {
		if len(row) != 2 {
			t.Fatal(row)
		}
		fields[row[0]] = row[1]
	}
	for key, want := range map[string]string{"instance": "'=synthetic", "samples[0].ttft_ms": "null", "samples[0].prompt_tokens": "null", "parameters.gpu_layers": "0", "parameters.flash_attention": "false", "identity.engine_version": "unknown"} {
		if fields[key] != want {
			t.Fatalf("%s: %q != %q", key, fields[key], want)
		}
	}
}
