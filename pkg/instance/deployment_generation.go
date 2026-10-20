package instance

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const DeploymentFixtureID = "synthetic-text-v1"
const deploymentFixture = "This is a synthetic deployment check. List three colors in a short sentence. Do not include any personal information."

func DeploymentFixtureDigest() string {
	sum := sha256.Sum256([]byte(deploymentFixture))
	return hex.EncodeToString(sum[:])
}

// GenerationObservation contains measurements, never backend text or error bodies.
// TTFT is client-observed first nonempty content, not a tokenizer measurement.
type GenerationObservation struct {
	Kind             string    `json:"kind"`
	StartedAt        time.Time `json:"started_at"`
	DurationMS       float64   `json:"duration_ms"`
	TTFTMS           *float64  `json:"ttft_ms"`
	PromptTokens     *int64    `json:"prompt_tokens"`
	CompletionTokens *int64    `json:"completion_tokens"`
	PrefillMS        *float64  `json:"backend_prefill_ms"`
	DecodeMS         *float64  `json:"backend_decode_ms"`
	ResponseBytes    int       `json:"response_bytes"`
	Completed        bool      `json:"completed"`
	ErrorCode        string    `json:"error_code,omitempty"`
}

// CheckNoLoadedModel never adopts an untracked backend load or unloads it.
func (m *Maintenance) CheckNoLoadedModel(ctx context.Context) error {
	b, err := m.RouterRequest(ctx, "GET", "/models", "")
	if err != nil {
		return controlError("model_ownership_unknown")
	}
	var reply struct {
		Data []struct {
			Status struct {
				Value string `json:"value"`
			} `json:"status"`
		} `json:"data"`
	}
	if json.Unmarshal(b, &reply) != nil || len(reply.Data) == 0 {
		return controlError("model_ownership_unknown")
	}
	for _, entry := range reply.Data {
		if entry.Status.Value != "unloaded" {
			return controlError("model_already_loaded")
		}
	}
	return nil
}

// GenerateDeploymentFixture is deliberately not an arbitrary backend proxy. The
// request shape, prompt and endpoint are fixed and use the existing maintenance
// capability without reentering the inference gate. There are no retries.
func (m *Maintenance) GenerateDeploymentFixture(ctx context.Context, model string, maxTokens int, seed int64) (out GenerationObservation, err error) {
	// Retain Go's monotonic clock for TTFT/duration; JSON still emits RFC3339.
	out.StartedAt = time.Now()
	defer func() { out.DurationMS = float64(time.Since(out.StartedAt)) / float64(time.Millisecond) }()
	if maxTokens < 1 || maxTokens > 256 {
		return out, controlError("invalid_test_budget")
	}
	if err = m.CheckControl(ctx, true); err != nil {
		return out, err
	}
	s := m.instance.ControlSnapshot()
	if s.State != "ready" || s.ActualModel != model {
		return out, controlError("model_not_ready")
	}
	body, _ := json.Marshal(map[string]any{
		"model": model, "messages": []map[string]string{{"role": "user", "content": deploymentFixture}},
		"stream": true, "stream_options": map[string]bool{"include_usage": true}, "max_tokens": maxTokens,
		"seed": seed, "temperature": 0, "cache_prompt": false,
		"chat_template_kwargs": map[string]bool{"enable_thinking": false},
	})
	endpoint := "http://" + net.JoinHostPort(m.instance.GetHost(), strconv.Itoa(m.instance.GetPort())) + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return out, controlError("generation_failed")
	}
	req.Header.Set("Content-Type", "application/json")
	if key := m.instance.backendAPIKey(m.instance.GetOptions()); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	if err = m.CheckControl(ctx, true); err != nil {
		return out, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return out, controlError("generation_transport_failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, controlError("generation_backend_failed")
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return out, controlError("generation_stream_invalid")
	}
	err = readDeploymentStream(resp.Body, &out)
	return out, err
}

func readDeploymentStream(body io.Reader, out *GenerationObservation) error {
	// Output tokens bound requested work, while byte/frame bounds also protect
	// against a misbehaving backend. No raw stream is retained in the report.
	scanner := bufio.NewScanner(io.LimitReader(body, 2<<20))
	scanner.Buffer(make([]byte, 4096), 64<<10)
	var data strings.Builder
	consume := func() (bool, error) {
		frame := strings.TrimSpace(data.String())
		data.Reset()
		if frame == "" {
			return false, nil
		}
		if frame == "[DONE]" {
			if out.ResponseBytes == 0 {
				return false, controlError("generation_empty_response")
			}
			out.Completed = true
			return true, nil
		}
		var event struct {
			Error   json.RawMessage `json:"error"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				Prompt     *int64 `json:"prompt_tokens"`
				Completion *int64 `json:"completion_tokens"`
			} `json:"usage"`
			Timings *struct {
				Prefill *float64 `json:"prompt_ms"`
				Decode  *float64 `json:"predicted_ms"`
			} `json:"timings"`
		}
		if json.Unmarshal([]byte(frame), &event) != nil {
			return false, controlError("generation_stream_invalid")
		}
		if len(event.Error) > 0 && string(event.Error) != "null" {
			return false, controlError("generation_backend_failed")
		}
		for _, choice := range event.Choices {
			if choice.Delta.Content != "" {
				if out.TTFTMS == nil {
					ms := float64(time.Since(out.StartedAt)) / float64(time.Millisecond)
					out.TTFTMS = &ms
				}
				out.ResponseBytes += len(choice.Delta.Content)
			}
		}
		if event.Usage != nil {
			if event.Usage.Prompt != nil && *event.Usage.Prompt >= 0 {
				out.PromptTokens = event.Usage.Prompt
			}
			if event.Usage.Completion != nil && *event.Usage.Completion >= 0 {
				out.CompletionTokens = event.Usage.Completion
			}
		}
		if event.Timings != nil {
			if event.Timings.Prefill != nil && *event.Timings.Prefill >= 0 {
				out.PrefillMS = event.Timings.Prefill
			}
			if event.Timings.Decode != nil && *event.Timings.Decode >= 0 {
				out.DecodeMS = event.Timings.Decode
			}
		}
		return false, nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			done, err := consume()
			if err != nil || done {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			if data.Len()+len(line) > 64<<10 {
				return controlError("generation_stream_invalid")
			}
			data.WriteString(strings.TrimPrefix(line, "data:"))
			data.WriteByte('\n')
		}
	}
	return controlError("generation_stream_incomplete")
}
