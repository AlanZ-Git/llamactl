//go:build windows

package instance

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

func createOwnedJob() (windows.Handle, error) {
	h, _, err := createJob.Call(0, 0)
	if h == 0 {
		return 0, fmt.Errorf("create owned Job: %w", err)
	}
	job := windows.Handle(h)
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if ok, _, err := setJob.Call(h, 9, uintptr(unsafe.Pointer(&limits)), unsafe.Sizeof(limits)); ok == 0 {
		windows.CloseHandle(job)
		return 0, fmt.Errorf("configure owned Job: %w", err)
	}
	return job, nil
}

// The console supervisor and the backend subtree have distinct Job scopes.
// Counts and PID lists are observations, never a source of termination targets.
type ownedJobEvidence struct {
	Active   uint32
	Total    uint32
	Assigned uint32
	PIDs     []uintptr
}

func (e ownedJobEvidence) empty() bool {
	return e.Active == 0 && e.Assigned == 0 && len(e.PIDs) == 0
}

func observeOwnedJob(job windows.Handle) (ownedJobEvidence, error) {
	type accounting struct {
		User, Kernel, PeriodUser, PeriodKernel int64
		Faults, Total, Active, Terminated      uint32
	}
	var info accounting
	ok, _, err := queryJob.Call(uintptr(job), 1, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info), 0)
	if ok == 0 {
		return ownedJobEvidence{}, fmt.Errorf("query owned Job accounting: %w", err)
	}
	evidence := ownedJobEvidence{Active: info.Active, Total: info.Total}
	// JOBOBJECT_BASIC_PROCESS_ID_LIST is two DWORDs followed by ULONG_PTRs.
	// Retry a growing list, but never treat truncation or a query error as empty.
	word := unsafe.Sizeof(uintptr(0))
	for capacity := uint32(16); capacity <= 65536; capacity *= 2 {
		bytes := uintptr(8) + uintptr(capacity)*word
		buffer := make([]uintptr, (bytes+word-1)/word)
		base := unsafe.Pointer(&buffer[0])
		ok, _, err := queryJob.Call(uintptr(job), 3, uintptr(base), bytes, 0)
		if ok == 0 {
			if err == windows.ERROR_MORE_DATA {
				continue
			}
			return evidence, fmt.Errorf("query owned Job process list: %w", err)
		}
		assigned := *(*uint32)(base)
		listed := *(*uint32)(unsafe.Add(base, 4))
		if listed > capacity || listed > assigned {
			return evidence, fmt.Errorf("invalid owned Job process list")
		}
		if assigned != listed {
			continue
		}
		evidence.Assigned = assigned
		evidence.PIDs = append([]uintptr(nil), unsafe.Slice((*uintptr)(unsafe.Add(base, 8)), int(listed))...)
		return evidence, nil
	}
	return evidence, fmt.Errorf("owned Job process list remained incomplete")
}

func processInOwnedJob(process, job windows.Handle) (bool, error) {
	var member uint32
	ok, _, err := jobKernel.NewProc("IsProcessInJob").Call(uintptr(process), uintptr(job), uintptr(unsafe.Pointer(&member)))
	if ok == 0 {
		return false, err
	}
	return member != 0, nil
}

type ownedProcessEvidence struct {
	PID      uint32
	Wait     uint32
	ExitCode uint32
}

func observeOwnedProcess(process windows.Handle) (ownedProcessEvidence, error) {
	var evidence ownedProcessEvidence
	var err error
	evidence.PID, err = windows.GetProcessId(process)
	if err != nil {
		return evidence, err
	}
	evidence.Wait, err = windows.WaitForSingleObject(process, 0)
	if err != nil {
		return evidence, err
	}
	if err := windows.GetExitCodeProcess(process, &evidence.ExitCode); err != nil {
		return evidence, err
	}
	return evidence, nil
}

type ownedExitEvidence struct {
	Supervisor    ownedJobEvidence
	Backend       ownedJobEvidence
	Root          ownedProcessEvidence
	Helper        ownedProcessEvidence
	BackendKnown  bool
	QuitSent      bool
	ForceRequests uint32
}

func (e ownedExitEvidence) mayQuitHelper() bool {
	return e.BackendKnown && e.Root.PID != 0 && e.Helper.PID != 0 && e.Backend.empty() && e.Root.Wait == windows.WAIT_OBJECT_0 && e.Helper.Wait == uint32(windows.WAIT_TIMEOUT)
}

func (e ownedExitEvidence) complete() bool {
	return e.Root.PID != 0 && e.Helper.PID != 0 && e.Supervisor.empty() && e.Backend.empty() && e.Root.Wait == windows.WAIT_OBJECT_0 && e.Helper.Wait == windows.WAIT_OBJECT_0
}

// Caller holds t.mu. Keep the last observation after handles are closed so a
// native failure can report the actual proof rather than re-query recycled values.
func (t *ownedTree) observeExitLocked() (ownedExitEvidence, error) {
	evidence := ownedExitEvidence{BackendKnown: t.process != t.helper, QuitSent: t.quitSent, ForceRequests: t.forceRequests}
	var err error
	evidence.Supervisor, err = observeOwnedJob(t.job)
	if err != nil {
		return evidence, err
	}
	evidence.Backend, err = observeOwnedJob(t.backendJob)
	if err != nil {
		return evidence, err
	}
	evidence.Root, err = observeOwnedProcess(t.process)
	if err != nil {
		return evidence, err
	}
	evidence.Helper, err = observeOwnedProcess(t.helper)
	return evidence, err
}
