package instance

import (
	"fmt"
	"llamactl/pkg/config"
	"os"
	"path/filepath"
)

// preparedPreset owns only its uniquely named same-directory temporary file.
// It never removes the destination to make a rename succeed.
type preparedPreset struct {
	path  string
	temp  string
	clear bool
}

func checkPresetTarget(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("preset.ini target is not a regular file")
	}
	return nil
}

func preparePresetIni(name string, opts *Options, instancesDir string) (*preparedPreset, error) {
	path := filepath.Join(instancesDir, name, "preset.ini")
	p := &preparedPreset{path: path, clear: opts == nil || opts.PresetIni == nil || *opts.PresetIni == ""}
	if err := checkPresetTarget(path); err != nil {
		return nil, fmt.Errorf("prepare preset target: %w", err)
	}
	if p.clear {
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			return p, nil
		} else if err != nil {
			return nil, fmt.Errorf("prepare preset clear: %w", err)
		}
	} else if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("prepare preset directory: %w", err)
	}
	// For clear, also probe the parent before stopping the process. The eventual
	// remove/rename can still fail (ACLs, sharing locks, concurrent filesystem changes).
	f, err := os.CreateTemp(filepath.Dir(path), ".preset-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("prepare preset temporary file: %w", err)
	}
	p.temp = f.Name()
	success := false
	defer func() {
		if !success {
			p.cleanup()
		}
	}()
	if !p.clear {
		if _, err := f.WriteString(*opts.PresetIni); err != nil {
			f.Close()
			return nil, fmt.Errorf("prepare preset write: %w", err)
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, fmt.Errorf("prepare preset flush: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("prepare preset close: %w", err)
	}
	success = true
	return p, nil
}

func (p *preparedPreset) cleanup() {
	if p != nil && p.temp != "" {
		_ = os.Remove(p.temp)
		p.temp = ""
	}
}
func (p *preparedPreset) commit() error {
	if p == nil {
		return nil
	}
	if err := checkPresetTarget(p.path); err != nil {
		return fmt.Errorf("commit preset target: %w", err)
	}
	if p.clear {
		if err := os.Remove(p.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("commit preset clear: %w", err)
		}
		p.cleanup()
		return nil
	}
	// os.Rename replaces existing files on supported platforms; Windows sharing
	// and rename semantics may reject this. Preserve the old file and return error.
	if err := os.Rename(p.temp, p.path); err != nil {
		return fmt.Errorf("commit preset replacement: %w", err)
	}
	p.temp = ""
	return nil
}

func syncPresetIni(name string, opts *Options, instancesDir string) error {
	p, err := preparePresetIni(name, opts, instancesDir)
	if err != nil {
		return err
	}
	defer p.cleanup()
	return p.commit()
}

// PreparedOptions is a single candidate, not a file/DB/process transaction.
// Call Close on every path. Commit only after the local process has stopped.
type PreparedOptions struct {
	instance  *Instance
	opts      *Options
	preset    *preparedPreset
	committed bool
}

func (i *Instance) PrepareOptions(opts *Options) (*PreparedOptions, error) {
	candidate, err := opts.Clone()
	if err != nil {
		return nil, fmt.Errorf("prepare options: %w", err)
	}
	if i.options != nil && i.GetOptions() != nil {
		candidate.Nodes = i.GetOptions().Nodes
	}
	cfg := i.appConfig()
	if err := candidate.ValidateEnvironmentPolicy(cfg); err != nil {
		return nil, fmt.Errorf("prepare environment: %w", err)
	}
	if err := candidate.BackendOptions.ValidateInstanceOptions(); err != nil {
		return nil, fmt.Errorf("prepare backend: %w", err)
	}
	candidate.validateAndApplyDefaults(i.Name, i.globalInstanceSettings)
	p := &PreparedOptions{instance: i, opts: candidate}
	if !i.IsRemote() && i.globalInstanceSettings != nil {
		p.preset, err = preparePresetIni(i.Name, candidate, i.globalInstanceSettings.InstancesDir)
		if err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (p *PreparedOptions) Close() { p.preset.cleanup() }

func (p *PreparedOptions) Commit() error {
	if p.committed {
		return fmt.Errorf("options candidate already committed")
	}
	i := p.instance
	// Serialize with start/automatic restart; a stopped status alone does not
	// prove that the previous child has finished draining/exiting.
	if i.process != nil {
		i.process.mu.Lock()
		defer i.process.mu.Unlock()
		if i.process.monitorDone != nil {
			return fmt.Errorf("commit options requires confirmed process exit")
		}
	}
	if !i.IsRemote() && (i.GetStatus() == Running || i.GetStatus() == ShuttingDown || i.GetStatus() == Restarting) {
		return fmt.Errorf("commit options requires a stopped instance")
	}
	if err := p.preset.commit(); err != nil {
		return err
	}
	i.options.set(p.opts)
	if i.proxy != nil {
		i.proxy.clear()
	}
	p.committed = true
	return nil
}

func (i *Instance) appConfig() *config.AppConfig {
	cfg := &config.AppConfig{Nodes: i.globalNodesConfig, LocalNode: i.localNodeName}
	if i.globalBackendSettings != nil {
		cfg.Backends = *i.globalBackendSettings
	}
	if i.globalInstanceSettings != nil {
		cfg.Instances = *i.globalInstanceSettings
	}
	return cfg
}
