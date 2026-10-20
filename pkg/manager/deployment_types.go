package manager

import (
	"encoding/json"
	"llamactl/pkg/instance"
	"llamactl/pkg/resourcepolicy"
	"time"
)

// DeploymentTestRequest is the public wire shape. Nil optional pointers mean
// missing; an explicit false, zero or empty patch remains present when marshaled.
// Scalar null is rejected by the server; nullable patch members use RawMessage.
type DeploymentTestRequest struct {
	BootID      string                      `json:"boot_id"`
	Revision    uint64                      `json:"expected_revision"`
	Key         string                      `json:"idempotency_key"`
	Model       string                      `json:"model"`
	Fixture     string                      `json:"fixture_id"`
	Patch       *map[string]json.RawMessage `json:"patch,omitempty"`
	Measured    *int                        `json:"measured_generations,omitempty"`
	Warmup      *bool                       `json:"warmup,omitempty"`
	MaxTokens   *int                        `json:"max_output_tokens,omitempty"`
	Timeout     *int                        `json:"timeout_seconds,omitempty"`
	Concurrency *int                        `json:"concurrency,omitempty"`
	Seed        *int64                      `json:"seed,omitempty"`
}

type DeploymentCancelRequest struct {
	BootID   string `json:"boot_id"`
	Revision uint64 `json:"expected_revision"`
	Key      string `json:"idempotency_key"`
}

// The validated request carries applied defaults separately from its canonical
// wire presence, so idempotency does not collapse missing/null/zero/false.
type deploymentRequest struct {
	BootID      string                     `json:"boot_id"`
	Revision    uint64                     `json:"expected_revision"`
	Key         string                     `json:"idempotency_key"`
	Model       string                     `json:"model"`
	Fixture     string                     `json:"fixture_id"`
	Patch       map[string]json.RawMessage `json:"patch,omitempty"`
	Measured    int                        `json:"measured_generations"`
	Warmup      bool                       `json:"warmup"`
	MaxTokens   int                        `json:"max_output_tokens"`
	Timeout     int                        `json:"timeout_seconds"`
	Concurrency int                        `json:"concurrency"`
	Seed        int64                      `json:"seed"`
	canonical   string
}

type DeploymentIdentity struct {
	ApplicationVersion   string `json:"application_version"`
	ApplicationCommit    string `json:"application_commit"`
	ApplicationBuildTime string `json:"application_build_time"`
	SourceModified       *bool  `json:"source_modified"`
	Engine               string `json:"engine"`
	EngineVersion        string `json:"engine_version"`
	EngineHash           string `json:"engine_hash"`
	ModelHash            string `json:"model_hash"`
	RuntimeIdentity      string `json:"runtime_identity"`
}

type DeploymentTest struct {
	ID                   string                           `json:"id"`
	BootID               string                           `json:"boot_id"`
	Instance             string                           `json:"instance"`
	Model                string                           `json:"model"`
	State                string                           `json:"state"`
	Stage                string                           `json:"stage"`
	ErrorCode            string                           `json:"error_code,omitempty"`
	ErrorStage           string                           `json:"error_stage,omitempty"`
	StartedAt            time.Time                        `json:"started_at"`
	Deadline             time.Time                        `json:"deadline"`
	EndedAt              *time.Time                       `json:"ended_at"`
	ElapsedMS            float64                          `json:"elapsed_ms"`
	Revision             uint64                           `json:"revision"`
	ProcessGeneration    uint64                           `json:"process_generation"`
	LoadGeneration       uint64                           `json:"load_generation"`
	Parameters           map[string]json.RawMessage       `json:"parameters"`
	ParameterDefaults    string                           `json:"parameter_defaults"`
	Identity             DeploymentIdentity               `json:"identity"`
	FixtureID            string                           `json:"fixture_id"`
	FixtureDigest        string                           `json:"fixture_digest"`
	MeasuredGenerations  int                              `json:"measured_generations"`
	Warmup               bool                             `json:"warmup"`
	MaxOutputTokens      int                              `json:"max_output_tokens"`
	Seed                 int64                            `json:"seed"`
	Temperature          int                              `json:"temperature"`
	Concurrency          int                              `json:"concurrency"`
	AttemptedGenerations int                              `json:"attempted_generations"`
	CompletedMeasured    int                              `json:"completed_measured"`
	LoadMode             string                           `json:"load_mode"`
	CacheCondition       string                           `json:"cache_condition"`
	CachePromptRequested bool                             `json:"cache_prompt_requested"`
	ReadyDurationMS      *float64                         `json:"ready_duration_ms"`
	Cleanup              string                           `json:"cleanup"`
	CleanupError         string                           `json:"cleanup_error,omitempty"`
	CleanupDurationMS    float64                          `json:"cleanup_duration_ms"`
	Samples              []instance.GenerationObservation `json:"samples"`
	Resources            DeploymentResources              `json:"resources"`
}

type DeploymentTestManager interface {
	SubmitDeploymentTest(string, string, []byte) (DeploymentTest, error)
	GetDeploymentTest(string, string, string, bool) (DeploymentTest, error)
	CancelDeploymentTest(string, string, string, bool, []byte) (DeploymentTest, error)
}

func parseDeploymentRequest(data []byte) (deploymentRequest, error) {
	req := deploymentRequest{Measured: 3, MaxTokens: 256, Timeout: 600, Concurrency: 1, Seed: 0}
	if len(data) > 64<<10 {
		return req, protocolError("invalid_config")
	}
	f, err := instance.ControlObject(data, "boot_id", "expected_revision", "idempotency_key", "model", "fixture_id", "patch", "measured_generations", "warmup", "max_output_tokens", "timeout_seconds", "concurrency", "seed")
	if err != nil {
		return req, protocolError("unsupported_parameter")
	}
	for _, k := range []string{"boot_id", "expected_revision", "idempotency_key", "model", "fixture_id"} {
		if f[k] == nil {
			return req, protocolError("invalid_config")
		}
	}
	for k, v := range f {
		if string(v) == "null" {
			return req, protocolError("invalid_config")
		}
		switch k {
		case "boot_id", "idempotency_key", "model", "fixture_id":
			var s string
			if json.Unmarshal(v, &s) != nil || len(s) == 0 || len(s) > 128 {
				return req, protocolError("invalid_config")
			}
			f[k], _ = json.Marshal(s)
		case "patch":
			req.Patch, err = instance.NormalizeControlPatch(v)
			if err != nil {
				return req, protocolError("unsupported_parameter")
			}
			f[k], _ = json.Marshal(req.Patch)
		case "warmup":
			if json.Unmarshal(v, &req.Warmup) != nil {
				return req, protocolError("invalid_config")
			}
			f[k], _ = json.Marshal(req.Warmup)
		default:
			min, max := int64(0), int64(9007199254740991)
			switch k {
			case "measured_generations":
				min, max = 1, 3
			case "max_output_tokens":
				min, max = 1, 256
			case "timeout_seconds":
				min, max = 1, 600
			case "concurrency":
				min, max = 1, 1
			case "seed":
				min, max = 0, 2147483647
			}
			f[k], err = instance.NormalizeControlInteger(v, min, max)
			if err != nil {
				return req, protocolError("invalid_test_budget")
			}
		}
	}
	canonical, _ := json.Marshal(f)
	if json.Unmarshal(canonical, &req) != nil {
		return req, protocolError("invalid_config")
	}
	req.canonical = string(canonical)
	if !resourcepolicy.ValidReleaseID(req.Key) || !resourcepolicy.ValidReleaseID(req.Model) || req.Fixture != instance.DeploymentFixtureID {
		return req, protocolError("invalid_config")
	}
	return req, nil
}
