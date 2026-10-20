package manager

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestDeploymentPublicRequestPresence(t *testing.T) {
	wire := DeploymentTestRequest{BootID: "boot", Revision: 0, Key: "key", Model: "alpha", Fixture: "synthetic-text-v1"}
	missing, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(missing, []byte("warmup")) || bytes.Contains(missing, []byte("patch")) || bytes.Contains(missing, []byte("seed")) {
		t.Fatal("optional values not missing", string(missing))
	}
	no := false
	zero := int64(0)
	patch := map[string]json.RawMessage{"threads": json.RawMessage("null"), "context": json.RawMessage("0"), "flash_attention": json.RawMessage("false")}
	wire.Warmup = &no
	wire.Seed = &zero
	wire.Patch = &patch
	explicit, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	req, err := parseDeploymentRequest(explicit)
	if err != nil {
		t.Fatal(err)
	}
	if req.Measured != 3 || req.MaxTokens != 256 || string(req.Patch["threads"]) != "null" || !bytes.Contains(explicit, []byte(`"warmup":false`)) || !bytes.Contains(explicit, []byte(`"seed":0`)) {
		t.Fatal("public wire values lost", string(explicit))
	}
	empty := map[string]json.RawMessage{}
	wire.Patch = &empty
	explicit, _ = json.Marshal(wire)
	if !bytes.Contains(explicit, []byte(`"patch":{}`)) {
		t.Fatal("explicit empty patch collapsed")
	}
}
