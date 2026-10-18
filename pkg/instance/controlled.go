package instance

import (
	"fmt"
	"strings"

	"llamactl/pkg/config"
)

// ValidateControlled applies only candidate defaults; isolated alone stays legacy.
func (o *Options) ValidateControlled(cfg *config.AppConfig) error {
	if !o.Controlled {
		return nil
	}
	if o.EnvironmentPolicy != "isolated" {
		return fmt.Errorf("controlled requires environment_policy=isolated")
	}
	if err := o.ValidateEnvironmentPolicy(cfg); err != nil {
		return err
	}
	if (o.AutoRestart != nil && *o.AutoRestart) || (o.OnDemandStart != nil && *o.OnDemandStart) || (o.IdleTimeout != nil && *o.IdleTimeout != 0) {
		return fmt.Errorf("controlled requires auto_restart=false, on_demand_start=false and idle_timeout=0")
	}
	disabled, zero := false, 0
	o.AutoRestart, o.OnDemandStart, o.IdleTimeout = &disabled, &disabled, &zero
	llama := o.BackendOptions.LlamaServerOptions
	if llama.ModelsAutoload || llama.SleepIdleSeconds != 0 {
		return fmt.Errorf("controlled forbids models_autoload and sleep_idle_seconds")
	}
	// A model-less native server is a Router. Its automatic loading and eviction
	// must be explicitly disabled rather than relying on engine defaults.
	if llama.GetModel() == "" && llama.ModelURL == "" && (!llama.NoModelsAutoload || llama.ModelsMax != 1) {
		return fmt.Errorf("controlled requires no_models_autoload=true and models_max=1")
	}
	// Raw server argv has no reliable scope/alias boundary. Reject rather than
	// guess whether a new engine flag can override the fixed Router contract.
	if len(cfg.Backends.LlamaCpp.Args) != 0 || len(llama.ExtraArgs) != 0 {
		return fmt.Errorf("controlled does not support raw backend args or extra_args")
	}
	for _, layer := range []map[string]string{cfg.Backends.LlamaCpp.Environment, o.Environment} {
		for key := range layer {
			key = strings.ToUpper(key)
			if strings.HasPrefix(key, "LLAMA_") && key != "LLAMA_API_KEY" && key != "LLAMA_ARG_HOST" {
				return fmt.Errorf("controlled requires typed backend parameters, not LLAMA_* environment overrides")
			}
		}
	}
	if o.PresetIni != nil {
		for _, line := range strings.Split(*o.PresetIni, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "[") {
				continue
			}
			key, _, ok := strings.Cut(line, "=")
			key = strings.TrimLeft(strings.ToLower(strings.ReplaceAll(strings.TrimSpace(key), "_", "-")), "-")
			// Router controls and sleeping can load/unload without a manager lease.
			// b11717 also has a preset-only startup switch, identified internally by
			// __PRESET_LOAD_ON_STARTUP, independent of models-autoload.
			if !ok || key == "load-on-startup" || key == "preset-load-on-startup" || strings.Contains(key, "autoload") || strings.HasPrefix(key, "models-") || strings.Contains(key, "sleep") {
				return fmt.Errorf("controlled preset contains unsupported Router/automatic behavior")
			}
		}
	}
	return nil
}
