//go:build windows

package instance

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const consoleHelperArg = "--llamactl-owned-console-helper"
const consoleBootstrapEnv = "LLAMACTL_INTERNAL_CONSOLE_HANDLES"
const consoleMapSize = 256 << 10
const consolePayloadOffset = 128
const consoleMagic = 0x4c434f31

const (
	consoleIdle uint32 = iota
	consolePending
	consoleClaimed
	consoleComplete
	consoleCanceled
)

// Only scalar, aligned fields cross the process boundary. Publishing Startup or
// Command uses interlocked operations; never share a Go pointer or mutex.
type consoleShared struct {
	Magic   uint32
	Version uint32
	Startup uint32
	Command uint32
	Result  uint32
	Length  uint32
	Root    uint64
}

type consoleCommand struct {
	Path string
	Args []string
	Env  []string
	Dir  string
}

type consoleIPC struct {
	mapping windows.Handle
	ready   windows.Handle
	request windows.Handle
	ack     windows.Handle
	quit    windows.Handle
	// Temporary assign/query capability for the backend-only child Job. Never
	// inherited by the backend; closed by the helper before readiness.
	backendJob windows.Handle
	view    uintptr
	shared  *consoleShared
}

// This runs in both the product and the test executable, before main performs
// config/database/network startup. The marker alone is not a launch capability:
// the private inherited mapping/events and owned Job must also be valid.
func init() {
	if len(os.Args) < 2 || os.Args[1] != consoleHelperArg {
		return
	}
	if len(os.Args) != 2 {
		os.Exit(125)
	}
	ipc, err := openConsoleIPC()
	if err != nil {
		os.Exit(125)
	}
	code := runConsoleHelper(ipc, nil, nil)
	ipc.close()
	os.Exit(int(code))
}

func withoutConsoleBootstrap(env []string) []string {
	result := make([]string, 0, len(env))
	for _, item := range env {
		key, _, _ := strings.Cut(item, "=")
		if !strings.EqualFold(key, consoleBootstrapEnv) {
			result = append(result, item)
		}
	}
	return result
}

func newConsoleIPC(cmd *exec.Cmd) (*consoleIPC, error) {
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	data, err := json.Marshal(consoleCommand{cmd.Path, cmd.Args, withoutConsoleBootstrap(env), cmd.Dir})
	if err != nil || len(data) > consoleMapSize-consolePayloadOffset {
		return nil, fmt.Errorf("owned console startup description exceeds limit")
	}
	c := &consoleIPC{}
	success := false
	defer func() {
		if !success {
			c.close()
		}
	}()
	c.mapping, err = windows.CreateFileMapping(windows.InvalidHandle, nil, windows.PAGE_READWRITE, 0, consoleMapSize, nil)
	if err != nil {
		return nil, err
	}
	if err = c.mapView(); err != nil {
		return nil, err
	}
	for _, target := range []*windows.Handle{&c.ready, &c.request, &c.ack, &c.quit} {
		*target, err = windows.CreateEvent(nil, 1, 0, nil)
		if err != nil {
			return nil, err
		}
	}
	c.shared.Magic, c.shared.Version, c.shared.Length = consoleMagic, 2, uint32(len(data))
	copy(unsafe.Slice((*byte)(unsafe.Pointer(c.view+consolePayloadOffset)), len(data)), data)
	success = true
	return c, nil
}

func (c *consoleIPC) handles() []windows.Handle {
	return []windows.Handle{c.mapping, c.ready, c.request, c.ack, c.quit, c.backendJob}
}

func (c *consoleIPC) mapView() error {
	view, err := windows.MapViewOfFile(c.mapping, windows.FILE_MAP_READ|windows.FILE_MAP_WRITE, 0, 0, consoleMapSize)
	if err != nil {
		return err
	}
	c.view, c.shared = view, (*consoleShared)(unsafe.Pointer(view))
	return nil
}

func (c *consoleIPC) close() {
	if c.view != 0 {
		windows.UnmapViewOfFile(c.view)
		c.view, c.shared = 0, nil
	}
	for _, h := range c.handles() {
		if h != 0 {
			windows.CloseHandle(h)
		}
	}
	c.mapping, c.ready, c.request, c.ack, c.quit, c.backendJob = 0, 0, 0, 0, 0, 0
}

func openConsoleIPC() (*consoleIPC, error) {
	parts := strings.Split(os.Getenv(consoleBootstrapEnv), ",")
	if len(parts) != 6 {
		return nil, fmt.Errorf("missing owned console capabilities")
	}
	var handles [6]windows.Handle
	seen := make(map[windows.Handle]bool)
	for n, part := range parts {
		value, err := strconv.ParseUint(part, 10, 64)
		h := windows.Handle(value)
		if err != nil || value != uint64(h) || h == 0 || h == windows.InvalidHandle || seen[h] {
			return nil, fmt.Errorf("invalid owned console capabilities")
		}
		seen[h], handles[n] = true, h
	}
	c := &consoleIPC{mapping: handles[0], ready: handles[1], request: handles[2], ack: handles[3], quit: handles[4], backendJob: handles[5]}
	if err := c.mapView(); err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			c.close()
		}
	}()
	if c.shared.Magic != consoleMagic || c.shared.Version != 2 || c.shared.Length == 0 || c.shared.Length > consoleMapSize-consolePayloadOffset || atomic.LoadUint32(&c.shared.Startup) != 0 || atomic.LoadUint32(&c.shared.Command) != consoleIdle {
		return nil, fmt.Errorf("invalid owned console bootstrap")
	}
	for _, h := range handles[1:5] {
		status, err := windows.WaitForSingleObject(h, 0)
		if err != nil || status != uint32(windows.WAIT_TIMEOUT) {
			return nil, fmt.Errorf("invalid owned console event")
		}
	}
	if member, err := processInOwnedJob(windows.CurrentProcess(), c.backendJob); err != nil || member {
		return nil, fmt.Errorf("invalid backend Job scope")
	}
	if evidence, err := observeOwnedJob(c.backendJob); err != nil || !evidence.empty() {
		return nil, fmt.Errorf("backend Job was not empty at bootstrap")
	}
	for _, h := range handles {
		if err := windows.SetHandleInformation(h, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
			return nil, err
		}
	}
	_ = os.Unsetenv(consoleBootstrapEnv) // This private helper process only.
	success = true
	return c, nil
}

// Hooks are nil in production; synthetic self-reexec tests pause the real state
// machine at claim boundaries instead of replacing GenerateConsoleCtrlEvent.
func runConsoleHelper(c *consoleIPC, beforeClaim, afterClaim func()) uint32 {
	startupTimer := time.AfterFunc(5*time.Second, func() { os.Exit(125) })
	defer startupTimer.Stop()
	startupFailed := func() uint32 {
		atomic.StoreUint32(&c.shared.Startup, 2)
		_ = windows.SetEvent(c.ready)
		return 125
	}
	var inJob uint32
	ok, _, _ := jobKernel.NewProc("IsProcessInJob").Call(uintptr(windows.CurrentProcess()), 0, uintptr(unsafe.Pointer(&inJob)))
	if ok == 0 || inJob == 0 {
		return startupFailed()
	}
	if cp, err := windows.GetConsoleCP(); err != nil || cp == 0 {
		return startupFailed()
	}
	// Validate the newly created console before launching any backend. This is
	// only an isolation check on our own console, never PID-based targeting or
	// parent enumeration. Refuse an accidentally shared console capability.
	var consoleProcess uint32
	n, _, _ := jobKernel.NewProc("GetConsoleProcessList").Call(uintptr(unsafe.Pointer(&consoleProcess)), 1)
	if n != 1 || consoleProcess != windows.GetCurrentProcessId() {
		return startupFailed()
	}
	setHandler := jobKernel.NewProc("SetConsoleCtrlHandler")
	if ok, _, _ := setHandler.Call(0, 0); ok == 0 {
		return startupFailed()
	}
	// Child processes copy this attribute at creation. Keep Ctrl-C enabled
	// through backend creation; only then make the helper itself ignore it.
	var command consoleCommand
	data := unsafe.Slice((*byte)(unsafe.Pointer(c.view+consolePayloadOffset)), int(c.shared.Length))
	if err := json.Unmarshal(data, &command); err != nil || command.Path == "" || len(command.Args) == 0 {
		return startupFailed()
	}
	cmd := &exec.Cmd{Path: command.Path, Args: command.Args, Env: withoutConsoleBootstrap(command.Env), Dir: command.Dir,
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
	// Keep backend descendants separate from the console supervisor/host. Both
	// the inherited supervisor Job and this explicit backend Job apply during
	// creation, before backend code runs. Only stdio handles reach the backend.
	if c.backendJob == 0 {
		return startupFailed()
	}
	root, err := createConsoleProcess(cmd, c.backendJob, 0x0002000d, 0, nil, nil)
	if err != nil {
		return startupFailed()
	}
	defer windows.CloseHandle(root)
	if err := windows.CloseHandle(c.backendJob); err != nil {
		return startupFailed()
	}
	c.backendJob = 0 // No Job handle remains in the ready helper.
	// Microsoft documents that changing this per-process attribute does not
	// affect existing processes. The already-created backend (and its children)
	// retains Ctrl-C processing. No Go callback waits on package initialization.
	if ok, _, _ := setHandler.Call(0, 1); ok == 0 {
		return startupFailed()
	}
	_ = os.Stdin.Close()
	_ = os.Stdout.Close()
	_ = os.Stderr.Close()
	c.shared.Root = uint64(root)
	atomic.StoreUint32(&c.shared.Startup, 1)
	if err := windows.SetEvent(c.ready); err != nil {
		return 125
	}
	startupTimer.Stop()
	for {
		status, err := windows.WaitForMultipleObjects([]windows.Handle{c.quit, c.request}, false, windows.INFINITE)
		if err != nil {
			return 125
		}
		if status == windows.WAIT_OBJECT_0 {
			return 0
		}
		if status != windows.WAIT_OBJECT_0+1 {
			return 125
		}
		if err := windows.ResetEvent(c.request); err != nil {
			return 125
		}
		if beforeClaim != nil {
			beforeClaim()
		}
		if !atomic.CompareAndSwapUint32(&c.shared.Command, consolePending, consoleClaimed) {
			continue // Cancellation won, or this generation already dispatched.
		}
		if afterClaim != nil {
			afterClaim()
		}
		err = windows.GenerateConsoleCtrlEvent(windows.CTRL_C_EVENT, 0)
		code := uint32(0)
		if err != nil {
			code = 1 // No raw OS diagnostics or command data cross this channel.
		}
		atomic.StoreUint32(&c.shared.Result, code)
		atomic.StoreUint32(&c.shared.Command, consoleComplete)
		if err := windows.SetEvent(c.ack); err != nil {
			return 125
		}
	}
}

// CreateProcess always receives an exact handle whitelist. Explicit Job
// association is installed at creation, never assigned after child code can run.
func createConsoleProcess(cmd *exec.Cmd, job windows.Handle, jobAttribute uintptr, flags uint32, extra []windows.Handle, environment func([]windows.Handle) []string) (windows.Handle, error) {
	if cmd.Err != nil {
		return 0, cmd.Err
	}
	count := uint32(1)
	if job != 0 {
		count++
	}
	attrs, err := windows.NewProcThreadAttributeList(count)
	if err != nil {
		return 0, err
	}
	defer attrs.Delete()
	if job != 0 {
		if err := attrs.Update(jobAttribute, unsafe.Pointer(&job), unsafe.Sizeof(job)); err != nil {
			return 0, err
		}
	}
	handles := make([]windows.Handle, 3+len(extra))
	defer func() {
		for _, h := range handles {
			if h != 0 {
				windows.CloseHandle(h)
			}
		}
	}()
	originals := make([]windows.Handle, 0, len(handles))
	for _, stream := range []any{cmd.Stdin, cmd.Stdout, cmd.Stderr} {
		file, ok := stream.(*os.File)
		if !ok {
			return 0, fmt.Errorf("owned process requires pipe handles")
		}
		h := windows.Handle(file.Fd())
		if kind, err := windows.GetFileType(h); err != nil || kind != windows.FILE_TYPE_PIPE {
			return 0, fmt.Errorf("owned process requires pipe handles")
		}
		originals = append(originals, h)
	}
	originals = append(originals, extra...)
	for n, h := range originals {
		if err := windows.DuplicateHandle(windows.CurrentProcess(), h, windows.CurrentProcess(), &handles[n], 0, true, windows.DUPLICATE_SAME_ACCESS); err != nil {
			return 0, err
		}
	}
	if err := attrs.Update(0x00020002, unsafe.Pointer(&handles[0]), uintptr(len(handles))*unsafe.Sizeof(handles[0])); err != nil {
		return 0, err
	}
	si := windows.StartupInfoEx{}
	si.Cb = uint32(unsafe.Sizeof(si))
	si.Flags = windows.STARTF_USESTDHANDLES | windows.STARTF_USESHOWWINDOW
	si.ShowWindow = windows.SW_HIDE
	si.StdInput, si.StdOutput, si.StdErr = handles[0], handles[1], handles[2]
	si.ProcThreadAttributeList = attrs.List()
	application, err := windows.UTF16PtrFromString(cmd.Path)
	if err != nil {
		return 0, err
	}
	args := make([]string, len(cmd.Args))
	for n, arg := range cmd.Args {
		args[n] = syscall.EscapeArg(arg)
	}
	line, err := windows.UTF16PtrFromString(strings.Join(args, " "))
	if err != nil {
		return 0, err
	}
	var dir *uint16
	if cmd.Dir != "" {
		dir, err = windows.UTF16PtrFromString(cmd.Dir)
		if err != nil {
			return 0, err
		}
	}
	env := cmd.Env
	if environment != nil {
		env = environment(handles[3:])
	} else if env == nil {
		env = os.Environ()
	}
	block := utf16.Encode([]rune(strings.Join(env, "\x00") + "\x00\x00"))
	var pi windows.ProcessInformation
	flags |= windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT
	if err := windows.CreateProcess(application, line, nil, nil, true, flags, &block[0], dir, &si.StartupInfo, &pi); err != nil {
		return 0, err
	}
	windows.CloseHandle(pi.Thread)
	return pi.Process, nil
}
