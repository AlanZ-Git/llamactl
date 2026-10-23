package instance

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestN1GenerationRejectsEmptyReasoningAndTruncated(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		success    bool
	}{
		{"content", `{"model":"alpha","choices":[{"message":{"content":"synthetic"},"finish_reason":"stop"}]}`, true},
		{"missing_model", `{"choices":[{"message":{"content":"synthetic"},"finish_reason":"stop"}]}`, false},
		{"wrong_model", `{"model":"other","choices":[{"message":{"content":"synthetic"},"finish_reason":"stop"}]}`, false},
		{"missing_finish", `{"model":"alpha","choices":[{"message":{"content":"synthetic"}}]}`, false},
		{"invalid_finish", `{"model":"alpha","choices":[{"message":{"content":"synthetic"},"finish_reason":"invented"}]}`, false},
		{"tool_finish", `{"model":"alpha","choices":[{"message":{"content":"synthetic"},"finish_reason":"tool_calls"}]}`, false},
		{"empty", `{"model":"alpha","choices":[{"message":{"content":" "},"finish_reason":"stop"}]}`, false},
		{"reasoning_only", `{"model":"alpha","choices":[{"message":{"content":"","reasoning_content":"internal"},"finish_reason":"stop"}]}`, false},
		{"length", `{"model":"alpha","choices":[{"message":{"content":"partial"},"finish_reason":"length"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst, _ := protocolFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if string(body["model"]) != `"alpha"` {
					t.Error("requested model not bound")
				}
				if string(body["chat_template_kwargs"]) != `{"enable_thinking":false}` {
					t.Errorf("nested false not forwarded: %s", body["chat_template_kwargs"])
				}
				w.Write([]byte(tc.body))
			}))
			publishSyntheticReady(t, inst, "alpha")
			s := inst.ControlSnapshot()
			m, err := inst.BeginControl(s.BootID, s.Revision, true, false)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			contract := RequestContract{TemplateVersion: "v1", Parameters: map[string]json.RawMessage{"max_tokens": json.RawMessage(`8`), "chat_template_kwargs": json.RawMessage(`{"enable_thinking":false}`)}}
			generation, err := m.GenerateN1(context.Background(), "alpha", json.RawMessage(`[{"role":"user","content":"test"}]`), contract, 1024)
			if tc.success {
				if err != nil || generation.State != "succeeded" {
					t.Fatalf("%+v %v", generation, err)
				}
			} else if err == nil || generation.State == "succeeded" {
				t.Fatalf("invalid generation succeeded: %+v %v", generation, err)
			}
		})
	}
}
