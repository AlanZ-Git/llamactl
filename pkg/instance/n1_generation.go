package instance

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type N1Generation struct {
	State            string `json:"state"`
	Quality          string `json:"quality"`
	ErrorCode        string `json:"error_code,omitempty"`
	PromptTokens     *int64 `json:"prompt_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
	DurationMS       int64  `json:"duration_ms"`
	Truncated        bool   `json:"truncated"`
	Body             []byte `json:"-"`
}

func (m *Maintenance) GenerateN1(ctx context.Context, model string, messages json.RawMessage, contract RequestContract, maxBytes int64) (out N1Generation, err error) {
	started := time.Now()
	out.State = "failed"
	out.Quality = "unknown"
	defer func() { out.DurationMS = time.Since(started).Milliseconds() }()
	if err = m.CheckControl(ctx, true); err != nil {
		return out, err
	}
	if maxBytes < 1 || maxBytes > 1<<20 {
		return out, controlError("invalid_test_budget")
	}
	if err = ValidateRequestContract(contract); err != nil {
		return out, err
	}
	s := m.instance.ControlSnapshot()
	if s.State != "ready" || s.ActualModel != model {
		return out, controlError("model_not_ready")
	}
	raw, _ := json.Marshal(map[string]any{"model": model, "messages": messages, "stream": false})
	body, err := applyRequestContract(raw, contract)
	if err != nil {
		return out, err
	}
	endpoint := "http://" + net.JoinHostPort(m.instance.GetHost(), strconv.Itoa(m.instance.GetPort())) + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return out, err
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
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return out, controlError("generation_transport_failed")
	}
	if int64(len(b)) > maxBytes {
		out.Truncated = true
		return out, controlError("output_truncated")
	}
	if resp.StatusCode != 200 {
		return out, controlError("generation_backend_failed")
	}
	var result struct {
		Model   string          `json:"model"`
		Error   json.RawMessage `json:"error"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			Prompt     *int64 `json:"prompt_tokens"`
			Completion *int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(b, &result) != nil || len(result.Choices) != 1 || (len(result.Error) > 0 && string(result.Error) != "null") {
		return out, controlError("generation_response_invalid")
	}
	if result.Model != model {
		return out, controlError("generation_model_mismatch")
	}
	if result.Usage != nil {
		if result.Usage.Prompt != nil && *result.Usage.Prompt >= 0 {
			out.PromptTokens = result.Usage.Prompt
		}
		if result.Usage.Completion != nil && *result.Usage.Completion >= 0 {
			out.CompletionTokens = result.Usage.Completion
		}
	}
	var maxTokens int64
	_ = json.Unmarshal(contract.Parameters["max_tokens"], &maxTokens)
	out.Body = []byte(result.Choices[0].Message.Content)
	if result.Choices[0].FinishReason == "length" || (out.CompletionTokens != nil && *out.CompletionTokens > maxTokens) {
		out.Truncated = true
		return out, controlError("output_truncated")
	}
	// N1's text-only request contract does not authorize tool calls or filtering
	// as a successful completion. A missing/unknown finish is not proof of output.
	if result.Choices[0].FinishReason != "stop" {
		return out, controlError("generation_response_invalid")
	}
	if strings.TrimSpace(string(out.Body)) == "" {
		return out, controlError("generation_empty_response")
	}
	if err = m.CheckControl(ctx, true); err != nil {
		return out, err
	}
	out.State = "succeeded"
	out.Quality = "synthetic_only"
	return out, nil
}
