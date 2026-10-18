package instance

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"sort"
	"strings"

	"llamactl/pkg/backends"
	"llamactl/pkg/config"
)

// mergeChildEnvironment applies layers without duplicate keys. Windows compares
// environment names case-insensitively. Sorting each map also resolves case aliases
// within a single trusted layer deterministically (last sorted spelling wins).
func mergeChildEnvironment(host []string, isolated bool, goos string, layers ...map[string]string) []string {
	values := make(map[string]string)
	canonical := func(key string) string {
		if goos == "windows" {
			return strings.ToUpper(key)
		}
		return key
	}
	put := func(key, value string) { values[canonical(key)] = key + "=" + value }
	for _, entry := range host {
		// Windows drive-current-directory entries start with '='.
		start := 0
		if strings.HasPrefix(entry, "=") {
			start = 1
		}
		pos := strings.IndexByte(entry[start:], '=')
		if pos < 0 {
			continue
		}
		pos += start
		key := entry[:pos]
		if isolated && strings.HasPrefix(canonical(key), "LLAMA_") {
			continue
		}
		put(key, entry[pos+1:])
	}
	for _, layer := range layers {
		keys := make([]string, 0, len(layer))
		for key := range layer {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			put(key, layer[key])
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, values[key])
	}
	return result
}

func trustedLlamaEnvironment(opts *Options, cfg *config.BackendConfig) map[string]string {
	var backend map[string]string
	if cfg != nil {
		backend = cfg.LlamaCpp.Environment
	}
	result := make(map[string]string)
	for _, entry := range mergeChildEnvironment(nil, false, runtime.GOOS, backend, opts.Environment) {
		key, value, _ := strings.Cut(entry, "=")
		if runtime.GOOS == "windows" {
			key = strings.ToUpper(key)
		}
		result[key] = value
	}
	return result
}

func (i *Instance) childEnvironment() []string {
	opts := i.GetOptions()
	if opts.EnvironmentPolicy == "isolated" {
		return mergeChildEnvironment(os.Environ(), true, runtime.GOOS, i.globalBackendSettings.LlamaCpp.Environment, opts.Environment)
	}
	// Keep the effective legacy precedence, including Docker configuration.
	backend := opts.BackendOptions.BuildEnvironment(i.globalBackendSettings, opts.DockerEnabled, nil)
	return mergeChildEnvironment(os.Environ(), false, runtime.GOOS, backend, opts.Environment)
}

// ValidateEnvironmentPolicy checks the opt-in contract before any process or file
// changes. It applies the explicit loopback default to this candidate only.
func (o *Options) ValidateEnvironmentPolicy(cfg *config.AppConfig) error {
	if o.EnvironmentPolicy != "" && o.EnvironmentPolicy != "inherit" && o.EnvironmentPolicy != "isolated" {
		return fmt.Errorf("environment_policy must be inherit or isolated")
	}
	if o.EnvironmentPolicy != "isolated" {
		return nil
	}
	if cfg == nil {
		return fmt.Errorf("isolated environment requires backend configuration")
	}
	_, local := o.Nodes[cfg.LocalNode]
	if len(o.Nodes) > 0 && !local {
		return fmt.Errorf("isolated environment requires a local instance")
	}
	if o.BackendOptions.BackendType != backends.BackendTypeLlamaCpp || o.BackendOptions.LlamaServerOptions == nil {
		return fmt.Errorf("isolated environment requires native llama.cpp")
	}
	if (o.DockerEnabled != nil && *o.DockerEnabled) || o.BackendOptions.IsDockerEnabled(&cfg.Backends, o.DockerEnabled) {
		return fmt.Errorf("isolated environment does not support Docker")
	}
	llama := o.BackendOptions.LlamaServerOptions
	if llama.APIKeyFile != "" || llama.ModelsPreset != "" || llama.SSLKeyFile != "" || llama.SSLCertFile != "" || llama.APIPrefix != "" {
		return fmt.Errorf("isolated environment does not support key files, external presets, TLS or API prefixes")
	}
	for _, arg := range cfg.Backends.LlamaCpp.Args {
		if reservedLlamaOption(arg) {
			return fmt.Errorf("isolated environment does not support raw routing/authentication arguments")
		}
	}
	for key := range llama.ExtraArgs {
		if reservedLlamaOption(key) {
			return fmt.Errorf("isolated environment does not support routing/authentication extra_args")
		}
	}
	// Presets are model configuration, not another source of server credentials or
	// routing. Do not read external files or attempt to interpret model parameters.
	if o.PresetIni != nil {
		for _, line := range strings.Split(*o.PresetIni, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "[") {
				continue
			}
			key, _, _ := strings.Cut(line, "=")
			if reservedLlamaOption(strings.TrimSpace(key)) {
				return fmt.Errorf("isolated preset cannot contain routing/authentication options")
			}
		}
	}
	for _, layer := range []map[string]string{cfg.Backends.LlamaCpp.Environment, o.Environment} {
		for key, value := range layer {
			if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, '\x00') {
				return fmt.Errorf("isolated environment contains an invalid environment entry")
			}
		}
	}
	env := trustedLlamaEnvironment(o, &cfg.Backends)
	for key := range env {
		if strings.HasPrefix(key, "LLAMA_") && key != "LLAMA_API_KEY" && key != "LLAMA_ARG_HOST" {
			suffix := strings.TrimPrefix(strings.TrimPrefix(key, "LLAMA_"), "ARG_")
			if reservedLlamaOption(suffix) {
				return fmt.Errorf("isolated environment has unsupported routing/authentication environment configuration")
			}
		}
	}
	host := llama.Host
	if value, ok := env["LLAMA_ARG_HOST"]; ok {
		if !isLoopback(value) || (host != "" && host != value) {
			return fmt.Errorf("isolated environment requires consistent loopback host settings")
		}
		host = value
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if !isLoopback(host) {
		return fmt.Errorf("isolated environment requires a literal loopback IP")
	}
	llama.Host = host
	key, hasEnvKey := env["LLAMA_API_KEY"]
	if hasEnvKey && llama.APIKey != "" && key != llama.APIKey {
		return fmt.Errorf("conflicting explicit backend API key sources")
	}
	if llama.APIKey != "" {
		key = llama.APIKey
	}
	if strings.ContainsAny(key, ", \t\r\n") || strings.ContainsFunc(key, func(r rune) bool { return r < 32 || r == 127 }) {
		return fmt.Errorf("backend API key must be a single literal token")
	}
	return nil
}

func isLoopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func reservedLlamaOption(arg string) bool {
	key, _, _ := strings.Cut(arg, "=")
	key = strings.ToLower(strings.ReplaceAll(strings.TrimLeft(key, "-"), "_", "-"))
	switch key {
	case "host", "port", "api-key", "api-key-file", "api-prefix", "ssl-key-file", "ssl-cert-file", "models-preset":
		return true
	}
	return false
}

func (i *Instance) backendAPIKey(opts *Options) string {
	if opts == nil || i.IsRemote() || opts.BackendOptions.BackendType != backends.BackendTypeLlamaCpp || opts.BackendOptions.LlamaServerOptions == nil || i.globalBackendSettings == nil || opts.BackendOptions.IsDockerEnabled(i.globalBackendSettings, opts.DockerEnabled) {
		return ""
	}
	if key := opts.BackendOptions.LlamaServerOptions.APIKey; key != "" {
		return key
	}
	return trustedLlamaEnvironment(opts, i.globalBackendSettings)["LLAMA_API_KEY"]
}
