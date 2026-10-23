package instance

import (
	"encoding/json"
	"io"
	"llamactl/pkg/resourcepolicy"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestN1ThinkingContractStrictFalse(t *testing.T) {
	c := RequestContract{TemplateVersion: "v1", Parameters: map[string]json.RawMessage{"max_tokens": json.RawMessage(`4`), "chat_template_kwargs": json.RawMessage(`{"enable_thinking":false}`)}}
	if err := ValidateRequestContract(c); err != nil {
		t.Fatalf("explicit false rejected: %v", err)
	}
	for _, suffix := range []string{"", `,"chat_template_kwargs":{"enable_thinking":false}`} {
		b, err := applyRequestContract([]byte(`{"model":"alpha","messages":[{"role":"user","content":"test"}]`+suffix+`}`), c)
		if err != nil || !strings.Contains(string(b), `"enable_thinking":false`) {
			t.Fatalf("false lost: %s %v", b, err)
		}
	}
	for _, raw := range []string{`null`, `{}`, `{"enable_thinking":null}`, `{"enable_thinking":true}`, `{"enable_thinking":false,"extra":false}`, `{"enable_thinking":false,"enable_thinking":false}`} {
		if _, err := applyRequestContract([]byte(`{"model":"alpha","messages":[{"role":"user","content":"test"}],"chat_template_kwargs":`+raw+`}`), c); err == nil {
			t.Errorf("accepted conflicting/invalid %s", raw)
		}
	}
	for _, raw := range []string{`null`, `{}`, `{"enable_thinking":null}`, `{"enable_thinking":0}`, `{"enable_thinking":false,"extra":false}`, `{"enable_thinking":false,"enable_thinking":true}`} {
		c.Parameters["chat_template_kwargs"] = json.RawMessage(raw)
		if err := ValidateRequestContract(c); err == nil {
			t.Errorf("invalid contract accepted: %s", raw)
		}
	}
}

func TestN1SlowBodyDoesNotHoldAdmission(t *testing.T) {
	for _, action := range []string{"status", "closing", "owner"} {
		t.Run(action, func(t *testing.T) {
			inst, _ := protocolFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("invalid body forwarded") }))
			inst.policy, _ = resourcepolicy.Open("")
			token := publishSyntheticReady(t, inst, "alpha")
			s := inst.ControlSnapshot()
			u, err := inst.ReserveN1("use", "scope", "service_use", s.BootID, s.Revision, time.Now().Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			c := RequestContract{TemplateVersion: "v1", Parameters: map[string]json.RawMessage{"max_tokens": json.RawMessage(`4`)}}
			if err = u.ActivateService(RecipeRef{ID: "recipe", Version: 1}, "alpha", c, time.Minute); err != nil {
				t.Fatal(err)
			}
			reader, writer := io.Pipe()
			req := httptest.NewRequest("POST", "/v1/chat/completions", reader)
			req.Header.Set("X-Service-Use", "use")
			req.Header.Set("X-Ready-Token", token)
			req.Header.Set("X-Prompt-Template-Version", "v1")
			// Digest of the frozen system-prefix/template-version pair.
			req.Header.Set("X-Prompt-Digest", c.PromptDigest())
			req = WithServiceScope(req, "scope")
			done := make(chan struct{})
			go func() { defer close(done); _ = inst.ServeHTTP(httptest.NewRecorder(), req) }()
			_, _ = writer.Write([]byte(`{"model":`))
			completed := make(chan struct{})
			go func() {
				defer close(completed)
				switch action {
				case "status":
					inst.ControlSnapshot()
				case "closing":
					u.BeginClosing()
				case "owner":
					p := inst.policy.Snapshot()
					_, _ = inst.policy.Update(false, p.BootID, p.Revision)
					u.BeginClosing()
				}
			}()
			blocked := false
			select {
			case <-completed:
			case <-time.After(200 * time.Millisecond):
				blocked = true
			}
			_ = writer.Close()
			<-done
			<-completed
			if u.ActiveRequests() != 0 {
				t.Error("body counted before validation")
			}
			u.BeginClosing()
			_ = u.CloseConfirmed()
			if blocked {
				t.Fatal("slow body held admission and blocked " + action)
			}
		})
	}
}
