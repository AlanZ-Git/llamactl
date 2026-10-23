package instance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"sort"
	"strconv"
	"strings"

	"llamactl/pkg/config"
	"llamactl/pkg/resourcepolicy"
)

// ControlConfig is trusted local administration, never a remote patch payload.
// PresetIni remains a derived representation in the same Options record.
type ControlConfig struct {
	Models map[string]ControlModel `json:"models"`
	Target string                  `json:"target,omitempty"`
}
type ControlModel struct {
	Path       string                     `json:"path"`
	Projector  string                     `json:"projector,omitempty"`
	Parameters map[string]json.RawMessage `json:"parameters,omitempty"`
}

type ParameterCapability struct {
	Type     string   `json:"type"`
	Minimum  int64    `json:"minimum,omitempty"`
	Maximum  int64    `json:"maximum,omitempty"`
	Nullable bool     `json:"nullable"`
	Enum     []string `json:"enum,omitempty"`
	INI      string   `json:"-"`
}

func (p ParameterCapability) MarshalJSON() ([]byte, error) {
	fields := map[string]any{"type": p.Type, "nullable": p.Nullable}
	if p.Type == "integer" {
		fields["minimum"], fields["maximum"] = p.Minimum, p.Maximum
	}
	if len(p.Enum) != 0 {
		fields["enum"] = p.Enum
	}
	return json.Marshal(fields)
}

// ControlParameters returns a fresh schema; callers cannot mutate validation.
func ControlParameters() map[string]ParameterCapability {
	return map[string]ParameterCapability{
		"context":         {Type: "integer", Maximum: 1048576, Nullable: true, INI: "ctx-size"},
		"gpu_layers":      {Type: "integer", Maximum: 999, Nullable: true, INI: "gpu-layers"},
		"threads":         {Type: "integer", Maximum: 1024, Nullable: true, INI: "threads"},
		"batch":           {Type: "integer", Minimum: 1, Maximum: 65536, Nullable: true, INI: "batch-size"},
		"ubatch":          {Type: "integer", Minimum: 1, Maximum: 65536, Nullable: true, INI: "ubatch-size"},
		"cache_ram":       {Type: "integer", Maximum: 1048576, Nullable: true, INI: "cache-ram"},
		"kv_type_k":       {Type: "string", Enum: []string{"f16", "q8_0", "q4_0"}, Nullable: true, INI: "cache-type-k"},
		"kv_type_v":       {Type: "string", Enum: []string{"f16", "q8_0", "q4_0"}, Nullable: true, INI: "cache-type-v"},
		"flash_attention": {Type: "boolean", INI: "flash-attn"},
	}
}

// ControlObject rejects duplicates, unknown fields and trailing JSON without
// collapsing missing, null, false or numeric zero.
func ControlObject(data []byte, allowed ...string) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("expected object")
	}
	out := map[string]json.RawMessage{}
	for dec.More() {
		tok, err = dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("invalid key")
		}
		found := false
		for _, a := range allowed {
			if a == key {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("unsupported field %s", key)
		}
		if _, exists := out[key]; exists {
			return nil, fmt.Errorf("duplicate field %s", key)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		out[key] = raw
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing JSON")
	}
	return out, nil
}

// NormalizeControlInteger accepts only exact schema-bounded JSON integers.
func NormalizeControlInteger(raw json.RawMessage, min, max int64) (json.RawMessage, error) {
	return controlInteger(raw, min, max)
}

func controlInteger(raw json.RawMessage, min, max int64) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	n, ok := v.(json.Number)
	if !ok {
		return nil, fmt.Errorf("expected integer")
	}
	// The envelope is bounded before parsing; cap exponent/numerator work too.
	if len(n.String()) > 32 {
		return nil, fmt.Errorf("integer too long")
	}
	f, err := strconv.ParseFloat(n.String(), 64)
	if err != nil || f < float64(min) || f > float64(max) {
		return nil, fmt.Errorf("integer out of range")
	}
	if f == 0 {
		mantissa := strings.SplitN(strings.ToLower(n.String()), "e", 2)[0]
		for _, digit := range mantissa {
			if digit >= '1' && digit <= '9' {
				return nil, fmt.Errorf("expected integer")
			}
		}
		return json.RawMessage("0"), nil
	}
	rat, ok := new(big.Rat).SetString(n.String())
	if !ok || !rat.IsInt() || !rat.Num().IsInt64() {
		return nil, fmt.Errorf("expected integer")
	}
	num := rat.Num().Int64()
	if num < min || num > max {
		return nil, fmt.Errorf("integer out of range")
	}
	return json.RawMessage(strconv.FormatInt(num, 10)), nil
}

func NormalizeControlPatch(data []byte) (map[string]json.RawMessage, error) {
	schema := ControlParameters()
	keys := make([]string, 0, len(schema))
	for k := range schema {
		keys = append(keys, k)
	}
	fields, err := ControlObject(data, keys...)
	if err != nil {
		return nil, err
	}
	for key, value := range fields {
		spec := schema[key]
		if string(value) == "null" {
			if !spec.Nullable {
				return nil, fmt.Errorf("%s cannot be null", key)
			}
			continue
		}
		switch spec.Type {
		case "integer":
			fields[key], err = controlInteger(value, spec.Minimum, spec.Maximum)
		case "boolean":
			var v bool
			err = json.Unmarshal(value, &v)
			if err == nil {
				fields[key], _ = json.Marshal(v)
			}
		case "string":
			var v string
			err = json.Unmarshal(value, &v)
			found := false
			for _, s := range spec.Enum {
				if s == v {
					found = true
				}
			}
			if !found {
				err = fmt.Errorf("unsupported enum")
			}
			if err == nil {
				fields[key], _ = json.Marshal(v)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("invalid %s: %w", key, err)
		}
	}
	return fields, nil
}

func validateParameterCombination(params map[string]json.RawMessage) error {
	integer := func(k string, def int) int {
		var n int
		if b, ok := params[k]; ok {
			_ = json.Unmarshal(b, &n)
			return n
		}
		return def
	}
	if integer("ubatch", 512) > integer("batch", 2048) {
		return fmt.Errorf("ubatch must not exceed batch (including engine defaults 512/2048)")
	}
	if v := string(params["kv_type_v"]); v != "" && v != `"f16"` && string(params["flash_attention"]) != "true" {
		return fmt.Errorf("quantized V cache requires explicit flash_attention=true")
	}
	return nil
}

func (o *Options) validateControlConfig(cfg *config.AppConfig) error {
	if o.Control == nil {
		return nil
	}
	if !o.Controlled {
		return fmt.Errorf("control requires controlled=true")
	}
	if len(o.Control.Models) == 0 || len(o.Control.Models) > 64 {
		return fmt.Errorf("control requires 1..64 trusted models")
	}
	if o.Control.Target != "" {
		if _, ok := o.Control.Models[o.Control.Target]; !ok {
			return fmt.Errorf("unknown target")
		}
	}
	// No global/model scope ambiguity, discovery or native option backdoors.
	data, err := json.Marshal(o.BackendOptions.LlamaServerOptions)
	if err != nil {
		return err
	}
	var global map[string]json.RawMessage
	if err := json.Unmarshal(data, &global); err != nil {
		return err
	}
	for k := range global {
		switch k {
		case "host", "port", "api_key", "no_models_autoload", "models_max":
		default:
			return fmt.Errorf("control forbids global backend option %s", k)
		}
	}
	ids := make([]string, 0, len(o.Control.Models))
	for id := range o.Control.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var ini strings.Builder
	for _, id := range ids {
		model := o.Control.Models[id]
		if !resourcepolicy.ValidReleaseID(id) || strings.HasPrefix(id, ".") {
			return fmt.Errorf("invalid logical model ID")
		}
		if model.Path == "" || strings.TrimSpace(model.Path) != model.Path || strings.ContainsAny(model.Path+model.Projector, "\r\n\x00") {
			return fmt.Errorf("invalid trusted model path")
		}
		raw, err := json.Marshal(model.Parameters)
		if err != nil {
			return err
		}
		if model.Parameters == nil {
			raw = []byte(`{}`)
		}
		params, err := NormalizeControlPatch(raw)
		if err != nil {
			return err
		}
		for k, v := range params {
			if string(v) == "null" {
				delete(params, k)
			}
		}
		if err := validateParameterCombination(params); err != nil {
			return err
		}
		model.Parameters = params
		o.Control.Models[id] = model
		fmt.Fprintf(&ini, "[%s]\nmodel = %s\nfit = off\n", id, model.Path)
		if model.Projector != "" {
			fmt.Fprintf(&ini, "mmproj = %s\n", model.Projector)
		}
		keys := make([]string, 0, len(params))
		for k := range params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		schema := ControlParameters()
		for _, k := range keys {
			value := string(params[k])
			if schema[k].Type == "string" {
				_ = json.Unmarshal(params[k], &value)
			}
			if k == "flash_attention" {
				if value == "true" {
					value = "on"
				} else {
					value = "off"
				}
			}
			fmt.Fprintf(&ini, "%s = %s\n", schema[k].INI, value)
		}
	}
	rendered := ini.String()
	if o.PresetIni != nil && *o.PresetIni != rendered {
		return fmt.Errorf("control preset must match trusted model mapping; raw INI is not supported")
	}
	o.PresetIni = &rendered
	return nil
}

// ControlCandidate is pure validation before acquiring maintenance or touching files.
func (i *Instance) ControlCandidate(model string, patch map[string]json.RawMessage) (*Options, error) {
	opts, err := i.GetOptions().Clone()
	if err != nil {
		return nil, err
	}
	if opts.Control == nil {
		return nil, fmt.Errorf("control is not configured")
	}
	m, ok := opts.Control.Models[model]
	if !ok {
		return nil, fmt.Errorf("unknown logical model")
	}
	if m.Parameters == nil {
		m.Parameters = map[string]json.RawMessage{}
	}
	for k, v := range patch {
		if string(v) == "null" {
			delete(m.Parameters, k)
		} else {
			m.Parameters[k] = v
		}
	}
	opts.Control.Models[model] = m
	opts.ActiveRecipe = nil
	opts.DeploymentDigest = ""
	opts.Control.Target = model
	opts.PresetIni = nil
	if err := opts.ValidateControlled(i.appConfig()); err != nil {
		return nil, err
	}
	for _, path := range []string{m.Path, m.Projector} {
		if path == "" {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("approved model file unavailable")
		}
	}
	return opts, nil
}
