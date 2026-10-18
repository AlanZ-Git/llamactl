package instance

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"llamactl/pkg/backends"
	"llamactl/pkg/config"
)

func TestStartupCleanupHelperProcess(t *testing.T) {
	if os.Getenv("LLAMACTL_STARTUP_CLEANUP_HELPER") != "1" {
		return
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

func TestStartupCleanupLoggerFailurePreservesPreviousGeneration(t *testing.T) {
	cfg, opts := policyFixture(t, "inherit")
	inst, err := NewChecked("previous-log", cfg, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := inst.logger.create(); err != nil {
		t.Fatal(err)
	}
	defer inst.logger.close()
	previousLog := inst.logger.logFile
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, nil, 0600); err != nil {
		t.Fatal(err)
	}
	inst.logger.logDir = blocked
	if err := inst.Start(); err == nil || !strings.Contains(err.Error(), "failed to create log files") {
		t.Fatalf("expected logger creation failure, got %v", err)
	}
	if inst.logger.logFile != previousLog {
		t.Error("logger creation failure closed the previous generation's logger")
	}
	if inst.process.ctx.Err() != context.Canceled || inst.IsRunning() {
		t.Error("logger creation failure retained context or claimed Running")
	}
}

func TestStartupCleanupFailures(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"logger", "command-build", "missing-executable", "invalid-executable", "occupied-port"} {
		t.Run(stage, func(t *testing.T) {
			cfg := &config.AppConfig{}
			cfg.Instances.InstancesDir = t.TempDir()
			cfg.Instances.LogsDir = t.TempDir()
			cfg.Backends.Custom = map[string]config.BackendSettings{
				"helper": {Command: executable},
			}
			opts := &Options{
				Environment: map[string]string{"LLAMACTL_STARTUP_CLEANUP_HELPER": "1"},
				BackendOptions: backends.Options{
					BackendType: backends.BackendTypeCustom,
					CustomServerOptions: &backends.CustomServerOptions{
						Name: "helper", Host: "127.0.0.1",
						Args: []string{"-test.run=^TestStartupCleanupHelperProcess$"},
					},
				},
			}
			inst, err := NewChecked("startup-cleanup", cfg, opts, nil)
			if err != nil {
				t.Fatal(err)
			}
			// No service is bound by the helper; bypass the port guard except
			// in the dedicated occupied-port case.
			inst.GetOptions().BackendOptions.CustomServerOptions.Port = 0
			p := inst.process
			t.Cleanup(func() {
				if inst.IsRunning() {
					_ = inst.Stop()
				}
				inst.logger.close()
				if p.cancel != nil {
					p.cancel()
				}
			})

			// A previous generation's handles must not be swept up when the
			// new attempt fails before allocating its own pipes.
			oldRead, oldWrite, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer oldRead.Close()
			defer oldWrite.Close()
			oldCtx, oldCancel := context.WithCancel(context.Background())
			defer oldCancel()
			p.ctx, p.cancel = oldCtx, oldCancel
			p.stdin, p.stdout, p.stderr = oldWrite, oldRead, oldRead

			wantError := "failed to start instance"
			switch stage {
			case "logger":
				blocked := filepath.Join(t.TempDir(), "not-a-directory")
				if err := os.WriteFile(blocked, nil, 0600); err != nil {
					t.Fatal(err)
				}
				inst.logger.logDir = blocked
				wantError = "failed to create log files"
			case "command-build":
				cfg.Backends.Custom["helper"] = config.BackendSettings{}
				wantError = "failed to build command"
			case "missing-executable":
				cfg.Backends.Custom["helper"] = config.BackendSettings{Command: filepath.Join(t.TempDir(), "missing.exe")}
			case "invalid-executable":
				invalid := filepath.Join(t.TempDir(), "invalid.exe")
				if err := os.WriteFile(invalid, []byte("not an executable\n"), 0700); err != nil {
					t.Fatal(err)
				}
				cfg.Backends.Custom["helper"] = config.BackendSettings{Command: invalid}
			case "occupied-port":
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				go func() {
					for {
						conn, err := listener.Accept()
						if err != nil {
							return
						}
						_ = conn.Close()
					}
				}()
				inst.GetOptions().BackendOptions.CustomServerOptions.Port = listener.Addr().(*net.TCPAddr).Port
				wantError = "refusing to start a second server"
			}

			if err := inst.Start(); err == nil || !strings.Contains(err.Error(), wantError) {
				t.Fatalf("expected %s failure, got %v", stage, err)
			}
			if inst.GetStatus() != Stopped || p.monitorDone != nil {
				t.Error("failed start claimed Running or installed a monitor")
			}
			if p.ctx == nil || !errors.Is(p.ctx.Err(), context.Canceled) {
				t.Error("failed start did not cancel its context")
			}
			if inst.logger.logFile != nil {
				t.Error("failed start retained an open logger")
			}
			if oldCtx.Err() != nil {
				t.Error("failed start canceled the previous generation")
			}
			for _, f := range []*os.File{oldRead, oldWrite} {
				if _, err := f.Stat(); err != nil {
					t.Error("failed start closed a previous generation's pipe")
				}
			}
			if stage == "missing-executable" || stage == "invalid-executable" {
				for _, pipe := range []any{p.stdin, p.stdout, p.stderr, p.cmd.Stdin, p.cmd.Stdout, p.cmd.Stderr} {
					f, ok := pipe.(*os.File)
					if !ok {
						t.Fatalf("expected a real pipe file, got %T", pipe)
					}
					// Windows reports ERROR_INVALID_HANDLE for these closed pipe
					// endpoints rather than wrapping os.ErrClosed as Unix does.
					if _, err := f.Stat(); err == nil {
						t.Error("failed start retained a valid pipe endpoint")
					}
				}
			}
			if t.Failed() {
				return // Preserve the first failure without replacing leaked handles.
			}
			if path := inst.logger.logFilePath; path != "" {
				if err := os.Remove(path); err != nil {
					t.Fatalf("failed start left log file locked: %v", err)
				}
			}

			cfg.Backends.Custom["helper"] = config.BackendSettings{Command: executable}
			inst.logger.logDir = cfg.Instances.LogsDir
			inst.GetOptions().BackendOptions.CustomServerOptions.Port = 0
			if err := inst.Start(); err != nil {
				t.Fatalf("valid start after failure: %v", err)
			}
			ctx, cmd, stdin := p.ctx, p.cmd, p.stdin
			if err := inst.Start(); err == nil {
				t.Fatal("duplicate start succeeded")
			}
			if ctx.Err() != nil || p.cmd != cmd || p.stdin != stdin || inst.logger.logFile == nil {
				t.Error("duplicate start disturbed the running generation")
			}
			if err := inst.Stop(); err != nil {
				t.Fatal(err)
			}
			if inst.GetStatus() != Stopped {
				t.Error("valid helper did not stop")
			}
		})
	}
}
