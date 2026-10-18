package instance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"llamactl/pkg/httpreject"
	"llamactl/pkg/resourcepolicy"
	"net/http"
	"strings"
)

type proxyAccess int

const (
	proxyUnknown proxyAccess = iota
	proxyRead
	proxyUse
	proxyMaintenance
)

// Exact paths only: no cleaning/decoding that could classify one path and send
// another to the engine. All production proxy entry points terminate here.
func classifyProxy(r *http.Request) proxyAccess {
	if r.URL.RawPath != "" && r.URL.RawPath != r.URL.Path {
		return proxyUnknown
	}
	// GET queries can trigger Router autoload/reload; only query-free reads are exempt.
	if r.Method == http.MethodGet && r.URL.RawQuery == "" {
		switch r.URL.Path {
		case "/health", "/models", "/v1/models", "/props", "/slots", "/metrics":
			return proxyRead
		}
	}
	if r.Method == http.MethodPost {
		switch r.URL.Path {
		case "/models/load", "/models/unload":
			return proxyMaintenance
		case "/completion", "/completions", "/v1/completions", "/chat/completions", "/v1/chat/completions", "/embedding", "/embeddings", "/v1/embeddings", "/rerank", "/reranking", "/v1/rerank", "/v1/reranking", "/infill", "/tokenize", "/detokenize", "/apply-template":
			return proxyUse
		}
	}
	return proxyUnknown
}

func unsupportedRequest(details string) error {
	return &AdmissionError{Code: "unsupported_controlled_request", Details: details}
}

// PrepareInferenceRequest validates the original body before an inference handler
// rewrites it. The caller must defer the returned release through ServeHTTP, which
// still performs final admission and validation. Reserving use also prevents a
// legacy-to-controlled transition from racing a lossy legacy normalization.
type policyContextKey struct{}

// CheckRequestPolicy validates an adapter's existing reservation at a late
// autostart boundary. It grants no authority and cannot reopen admission.
func (i *Instance) CheckRequestPolicy(r *http.Request) error {
	if permit, ok := r.Context().Value(policyContextKey{}).(*resourcepolicy.Permit); ok {
		return permit.Check()
	}
	permit, err := i.policy.Acquire()
	if err != nil {
		return err
	}
	permit.Close()
	return nil
}

// ReserveProxyRequest reserves owner-policy admission before an adapter's
// optional autostart. It is not a forwarding bypass: ServeHTTP still validates
// instance admission, request shape and this permit's epoch. Close after forwarding.
// On failure the stable HTTP admission response is written, or scheduled at the
// enclosing control-rejection boundary after the adapter releases its permits.
func (i *Instance) ReserveProxyRequest(w http.ResponseWriter, r *http.Request) (func(), error) {
	access := classifyProxy(r)
	if access == proxyRead || (access == proxyMaintenance && r.URL.Path == "/models/unload") {
		return func() {}, nil
	}
	permit, err := i.policy.Acquire()
	if err != nil {
		writeProxyError(w, r, err)
		return nil, err
	}
	*r = *r.WithContext(context.WithValue(r.Context(), policyContextKey{}, permit))
	return permit.Close, nil
}

func (i *Instance) PrepareInferenceRequest(r *http.Request) (func(), error) {
	permit, err := i.policy.Acquire()
	if err != nil {
		return nil, err
	}
	accepted := false
	defer func() {
		if !accepted {
			permit.Close()
		}
	}()
	release, controlled, err := i.acquireUse(false)
	if err != nil {
		return nil, err
	}
	if controlled {
		access := classifyProxy(r)
		if access != proxyUse {
			release()
			return nil, unsupportedRequest("not a controlled inference path")
		}
		if err := validateControlledRequest(r, access); err != nil {
			release()
			return nil, err
		}
	}
	accepted = true
	*r = *r.WithContext(context.WithValue(r.Context(), policyContextKey{}, permit))
	return func() { release(); permit.Close() }, nil
}

// Validate the fixed native JSON protocol without rewriting it. Reject overrides
// even when false/null, duplicate keys, nested, or passed as query parameters.
func validateControlledRequest(r *http.Request, access proxyAccess) error {
	if access == proxyUnknown {
		return unsupportedRequest("unknown or mutable proxy path is not supported in controlled mode")
	}
	if r.URL.RawQuery != "" {
		return unsupportedRequest("controlled proxy query parameters are not supported")
	}
	if access == proxyRead {
		return nil
	}
	if r.Header.Get("Content-Encoding") != "" {
		return unsupportedRequest("encoded controlled requests are not supported")
	}
	if r.Body == nil {
		return unsupportedRequest("controlled request requires a JSON object")
	}
	const maxBody = 16 << 20
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	r.Body.Close()
	if err != nil {
		return unsupportedRequest("cannot read controlled request")
	}
	if len(body) > maxBody {
		return unsupportedRequest("controlled JSON request exceeds 16 MiB")
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := inspectJSON(dec, true); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return unsupportedRequest("controlled request must contain one JSON object")
	}
	if access == proxyMaintenance {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(body, &object); err != nil {
			return unsupportedRequest("invalid model request")
		}
		for key := range object {
			if key != "model" {
				return unsupportedRequest("Router maintenance accepts only model")
			}
		}
		var model string
		if json.Unmarshal(object["model"], &model) != nil || strings.TrimSpace(model) == "" {
			return unsupportedRequest("Router maintenance requires a model name")
		}
	}
	return nil
}

func inspectJSON(dec *json.Decoder, root bool) error {
	token, err := dec.Token()
	if err != nil {
		return unsupportedRequest("invalid JSON request")
	}
	delimiter, container := token.(json.Delim)
	if root && (!container || delimiter != '{') {
		return unsupportedRequest("controlled request requires a JSON object")
	}
	if !container {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]bool)
		for dec.More() {
			token, err := dec.Token()
			if err != nil {
				return unsupportedRequest("invalid JSON object")
			}
			key, ok := token.(string)
			if !ok || seen[key] {
				return unsupportedRequest("duplicate or invalid JSON key")
			}
			seen[key] = true
			normalized := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
			if strings.Contains(normalized, "autoload") || normalized == "models_max" || normalized == "sleep_idle_seconds" {
				return unsupportedRequest("per-request autoload/Router overrides are forbidden")
			}
			if err := inspectJSON(dec, false); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := inspectJSON(dec, false); err != nil {
				return err
			}
		}
	default:
		return unsupportedRequest("invalid JSON delimiter")
	}
	if _, err := dec.Token(); err != nil {
		return unsupportedRequest("invalid JSON ending")
	}
	return nil
}

func writeProxyError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, ok := AdmissionHTTPError(err)
	if !ok {
		status, code = http.StatusBadGateway, "proxy_failed"
	}
	if code == "ai_paused" && classifyProxy(r) == proxyUse {
		status = http.StatusServiceUnavailable
	}
	write := func() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		body := map[string]any{"error": code, "details": err.Error()}
		if code == "ai_paused" {
			body["retryable"] = false
		}
		_ = json.NewEncoder(w).Encode(body)
	}
	if (code == "busy" || code == "ai_paused") && classifyProxy(r) == proxyMaintenance && httpreject.Defer(r, write) {
		return
	}
	write()
}

func (i *Instance) serveAdmitted(w http.ResponseWriter, r *http.Request) (err error) {
	defer func() {
		if err != nil {
			writeProxyError(w, r, err)
		}
	}()
	access := classifyProxy(r)
	var permit *resourcepolicy.Permit
	if access != proxyRead && !(access == proxyMaintenance && r.URL.Path == "/models/unload") {
		if prior, ok := r.Context().Value(policyContextKey{}).(*resourcepolicy.Permit); ok {
			if err := prior.Check(); err != nil {
				return err
			}
		}
		permit, err = i.policy.Acquire()
		if err != nil {
			return err
		}
		defer permit.Close()
	}
	// Acquire and inspect mode under the same lock. Controlled mode cannot change
	// while running; a maintenance lease also excludes mode changes while stopped.
	if access == proxyMaintenance {
		lease, leaseErr := i.BeginMaintenance()
		if leaseErr != nil {
			return leaseErr
		}
		defer lease.Close()
		if i.IsControlled() {
			if !i.IsRunning() {
				return &AdmissionError{Code: "instance_not_running", Details: "controlled instances are use-running-only"}
			}
			if err := validateControlledRequest(r, access); err != nil {
				return err
			}
		}
		if err := permit.Check(); err != nil {
			return err
		}
		return i.forwardHTTP(w, r)
	}
	release, controlled, err := i.acquireUse(access == proxyRead)
	if err != nil {
		return err
	}
	defer release()
	if controlled {
		if err := validateControlledRequest(r, access); err != nil {
			return err
		}
	}
	if i.proxy == nil {
		return fmt.Errorf("instance %s has no proxy component", i.Name)
	}
	if err := permit.Check(); err != nil {
		return err
	}
	return i.forwardHTTP(w, r)
}
