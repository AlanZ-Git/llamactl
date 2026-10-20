//go:build windows

package instance

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const b1FixtureMarker = "--owner-console-fixture"
const b1HarnessMarker = "--owner-console-harness"

var b1Roles = []string{"root", "child", "grandchild"}

// Unlike os/signal on Windows, this records CTRL_C and CTRL_BREAK separately.
// Returning TRUE also keeps an erroneous broadcast from killing the test manager
// or sentinel before they can report it. Only synthetic subprocesses install it.
type b1ControlCounts struct {
	ctrlC     atomic.Uint32
	ctrlBreak atomic.Uint32
	other     atomic.Uint32
}

type b1ConsoleEvidence struct {
	PID       uint32 `json:"pid"`
	CtrlC     uint32 `json:"ctrl_c"`
	CtrlBreak uint32 `json:"ctrl_break"`
	Other     uint32 `json:"other"`
	EOF       bool   `json:"eof"`
	Completed bool   `json:"completed"`
}

func b1InstallControlHandler(t *testing.T, preserveIgnore ...bool) *b1ControlCounts {
	t.Helper()
	counts := &b1ControlCounts{}
	setHandler := jobKernel.NewProc("SetConsoleCtrlHandler")
	// The original fixture enables Ctrl-C explicitly. The inheritance regression
	// deliberately does not: a real backend may only install its handler.
	if len(preserveIgnore) == 0 || !preserveIgnore[0] {
		if ok, _, err := setHandler.Call(0, 0); ok == 0 {
			t.Fatalf("enable fixture Ctrl-C: %v", err)
		}
	}
	callback := syscall.NewCallback(func(event uint32) uintptr {
		switch event {
		case windows.CTRL_C_EVENT:
			counts.ctrlC.Add(1)
		case windows.CTRL_BREAK_EVENT:
			counts.ctrlBreak.Add(1)
		default:
			counts.other.Add(1)
		}
		return 1
	})
	if ok, _, err := setHandler.Call(callback, 1); ok == 0 {
		t.Fatalf("install fixture console handler: %v", err)
	}
	t.Cleanup(func() { setHandler.Call(callback, 0) })
	return counts
}

func (c *b1ControlCounts) evidence(eof bool) b1ConsoleEvidence {
	return b1ConsoleEvidence{PID: windows.GetCurrentProcessId(), CtrlC: c.ctrlC.Load(), CtrlBreak: c.ctrlBreak.Load(), Other: c.other.Load(), EOF: eof}
}

func b1HelperArgs(marker string) []string {
	for n, arg := range os.Args {
		if arg == marker {
			return os.Args[n+1:]
		}
	}
	return nil
}

func b1HardDeadline() {
	go func() {
		time.Sleep(60 * time.Second)
		os.Exit(91) // A safety exit is never normal-completion evidence.
	}()
}

func b1FixtureCommand(role, behavior, dir, port string) *exec.Cmd {
	return exec.Command(os.Args[0], "-test.run=^TestOwnerWindowsB1Fixture$", "--", b1FixtureMarker, role, behavior, dir, port)
}

// No production main, model, or HTTP shutdown protocol is involved. Each member
// has its own real handler and loopback observation endpoint. EOF is observed but
// deliberately ignored. A test-only file barrier holds graceful cleanup open so
// repeated requests cannot hide a second Ctrl-C behind a fast process exit.
func TestOwnerWindowsB1Fixture(t *testing.T) {
	args := b1HelperArgs(b1FixtureMarker)
	if args == nil {
		return
	}
	b1HardDeadline()
	if len(args) != 4 {
		t.Fatal("invalid console fixture arguments")
	}
	role, behavior, dir, port := args[0], args[1], args[2], args[3]
	counts := b1InstallControlHandler(t, behavior == "inheritance")
	var eof atomic.Bool
	go func() {
		_, err := io.Copy(io.Discard, os.Stdin)
		if err == nil {
			eof.Store(true)
		}
	}()
	ln, err := net.Listen("tcp4", "127.0.0.1:"+port)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{ReadHeaderTimeout: time.Second, WriteTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(counts.evidence(eof.Load()))
	})}
	defer server.Close()
	go server.Serve(ln)
	var childDone chan error
	if role == "root" || role == "child" {
		next := "child"
		if role == "child" {
			next = "grandchild"
		}
		child := b1FixtureCommand(next, behavior, dir, "0")
		// No new console/group and no breakaway: inherit both console and Job.
		child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		childDone = make(chan error, 1)
		go func() { childDone <- child.Wait() }()
	}
	if err := os.WriteFile(filepath.Join(dir, role+".addr"), []byte(ln.Addr().String()), 0600); err != nil {
		t.Fatal(err)
	}
	for {
		if role == "root" {
			if _, err := os.Stat(filepath.Join(dir, "leave-root")); err == nil {
				_ = server.Close()
				return // Dedicated regression: descendants must outlive this root.
			}
		}
		evidence := counts.evidence(eof.Load())
		if role != "sentinel" && (evidence.CtrlC > 1 || evidence.CtrlBreak != 0 || evidence.Other != 0) {
			// Model b11717's second-interrupt hazard, without accepting it as a
			// graceful exit. Persist receipt before the abnormal fixture exit.
			b1WriteEvidence(t, dir, role+".abnormal", evidence)
			os.Exit(95)
		}
		_, finishErr := os.Stat(filepath.Join(dir, "finish"))
		if (behavior == "graceful" || behavior == "inheritance") && evidence.CtrlC == 1 && finishErr == nil {
			// Children must finish normally too; a killed descendant cannot be
			// hidden by a zero root exit code or a closed listener.
			if childDone != nil {
				select {
				case err := <-childDone:
					if err != nil {
						t.Fatalf("descendant completion: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("descendant did not complete normally")
				}
			}
			_ = server.Close()
			evidence = counts.evidence(eof.Load())
			if evidence.CtrlC != 1 || evidence.CtrlBreak != 0 || evidence.Other != 0 {
				t.Fatalf("extra console event during cleanup: %+v", evidence)
			}
			evidence.Completed = true
			b1WriteEvidence(t, dir, role+".complete", evidence)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func b1WriteEvidence(t *testing.T, dir, name string, evidence b1ConsoleEvidence) {
	t.Helper()
	data, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func b1ReadStatus(addr string) (b1ConsoleEvidence, error) {
	// Never consult a system proxy or leave loopback, including on redirects.
	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 300 * time.Millisecond,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get("http://" + addr + "/")
	if err != nil {
		return b1ConsoleEvidence{}, err
	}
	defer resp.Body.Close()
	var result b1ConsoleEvidence
	err = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&result)
	return result, err
}

func b1AwaitStatus(t *testing.T, dir, role string, matches func(b1ConsoleEvidence) bool) b1ConsoleEvidence {
	t.Helper()
	addr := treeAddress(t, dir, role)
	deadline := time.Now().Add(5 * time.Second)
	var evidence b1ConsoleEvidence
	var err error
	for time.Now().Before(deadline) {
		evidence, err = b1ReadStatus(addr)
		if err == nil && matches(evidence) {
			return evidence
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s console status did not match: %+v, error=%v", role, evidence, err)
	return evidence
}

// Every test manager gets its own CREATE_NEW_CONSOLE and creation-time Job.
// Thus even an incorrect future product broadcast is confined to test processes,
// never the invoking terminal/CI worker. This launcher is deliberately independent
// of startOwnedTree: isolation must not depend on the code under regression.
func b1StartHarness(t *testing.T, mode, dir string) (windows.Handle, windows.Handle) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestOwnerWindowsB1Harness$", "-test.v", "--", b1HarnessMarker, mode, dir)
	if cmd.Err != nil {
		t.Fatal(cmd.Err)
	}
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.Create(filepath.Join(dir, "harness.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	h, _, err := createJob.Call(0, 0)
	if h == 0 {
		t.Fatalf("create test containment Job: %v", err)
	}
	job := windows.Handle(h)
	success := false
	defer func() {
		if !success {
			windows.CloseHandle(job)
		}
	}()
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if ok, _, err := setJob.Call(h, 9, uintptr(unsafe.Pointer(&limits)), unsafe.Sizeof(limits)); ok == 0 {
		t.Fatalf("configure test containment Job: %v", err)
	}
	attrs, err := windows.NewProcThreadAttributeList(2)
	if err != nil {
		t.Fatal(err)
	}
	defer attrs.Delete()
	if err := attrs.Update(0x0002000d, unsafe.Pointer(&job), unsafe.Sizeof(job)); err != nil {
		t.Fatal(err)
	}
	var handles [3]windows.Handle
	defer func() {
		for _, handle := range handles {
			if handle != 0 {
				windows.CloseHandle(handle)
			}
		}
	}()
	for n, file := range []*os.File{input, output, output} {
		if err := windows.DuplicateHandle(windows.CurrentProcess(), windows.Handle(file.Fd()), windows.CurrentProcess(), &handles[n], 0, true, windows.DUPLICATE_SAME_ACCESS); err != nil {
			t.Fatal(err)
		}
	}
	if err := attrs.Update(0x00020002, unsafe.Pointer(&handles[0]), unsafe.Sizeof(handles)); err != nil {
		t.Fatal(err)
	}
	si := windows.StartupInfoEx{}
	si.Cb = uint32(unsafe.Sizeof(si))
	si.Flags = windows.STARTF_USESTDHANDLES | windows.STARTF_USESHOWWINDOW
	si.ShowWindow = windows.SW_HIDE
	si.StdInput, si.StdOutput, si.StdErr = handles[0], handles[1], handles[2]
	si.ProcThreadAttributeList = attrs.List()
	application, err := windows.UTF16PtrFromString(cmd.Path)
	if err != nil {
		t.Fatal(err)
	}
	args := make([]string, len(cmd.Args))
	for n, arg := range cmd.Args {
		args[n] = syscall.EscapeArg(arg)
	}
	commandLine, err := windows.UTF16PtrFromString(strings.Join(args, " "))
	if err != nil {
		t.Fatal(err)
	}
	var pi windows.ProcessInformation
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_NEW_CONSOLE)
	if err := windows.CreateProcess(application, commandLine, nil, nil, true, flags, nil, nil, &si.StartupInfo, &pi); err != nil {
		t.Fatalf("create private-console test manager: %v", err)
	}
	windows.CloseHandle(pi.Thread)
	success = true
	return job, pi.Process
}

func b1JobEmpty(job windows.Handle, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	for {
		evidence, err := observeOwnedJob(job)
		if err != nil {
			return err
		}
		if evidence.empty() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("test containment Job is not empty: %+v", evidence)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func b1RunConsoleTest(t *testing.T, mode string) {
	t.Helper()
	dir := t.TempDir()
	job, process := b1StartHarness(t, mode, dir)
	t.Cleanup(func() {
		// Explicit test teardown, after assertions; never graceful evidence.
		if err := b1JobEmpty(job, 100*time.Millisecond); err != nil {
			t.Errorf("test containment cleanup needed: %v", err)
			if ok, _, killErr := terminateJob.Call(uintptr(job), 99); ok == 0 {
				t.Errorf("terminate test containment Job: %v", killErr)
			}
			if err := b1JobEmpty(job, 5*time.Second); err != nil {
				t.Error(err)
			}
		}
		windows.CloseHandle(process)
		windows.CloseHandle(job)
		if data, err := os.ReadFile(filepath.Join(dir, "harness.log")); err == nil {
			t.Logf("private-console harness (%s):\n%s", mode, data)
		} else {
			t.Error(err)
		}
	})
	status, err := windows.WaitForSingleObject(process, 55000)
	if err != nil || status != windows.WAIT_OBJECT_0 {
		t.Fatalf("private-console harness exceeded 55s or wait failed: status=%d error=%v", status, err)
	}
	var code uint32
	if err := windows.GetExitCodeProcess(process, &code); err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("private-console %s assertions failed: exit code=%d (see harness log)", mode, code)
	}
}
