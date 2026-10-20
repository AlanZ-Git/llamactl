//go:build windows

package instance

import (
	"context"
	"fmt"
	"llamactl/pkg/resources"
	"sort"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func (t *ownedTree) sampleResources(ctx context.Context) resources.ProcessReading {
	r := resources.ProcessReading{At: time.Now(), Source: "windows_process_api", Coverage: "unavailable", Reason: "tree_lifecycle_busy"}
	if !t.mu.TryLock() {
		return r
	}
	if t.exited || t.startupErr != nil || t.process == t.helper {
		t.mu.Unlock()
		r.Reason = "owned_backend_not_live"
		return r
	}
	var job, root windows.Handle
	err := windows.DuplicateHandle(windows.CurrentProcess(), t.backendJob, windows.CurrentProcess(), &job, 0, false, windows.DUPLICATE_SAME_ACCESS)
	if err == nil {
		err = windows.DuplicateHandle(windows.CurrentProcess(), t.process, windows.CurrentProcess(), &root, 0, false, windows.DUPLICATE_SAME_ACCESS)
	}
	t.mu.Unlock()
	if job != 0 {
		defer windows.CloseHandle(job)
	}
	if root != 0 {
		defer windows.CloseHandle(root)
	}
	if err != nil {
		r.Reason = "held_handle_duplication_failed"
		return r
	}
	bounded, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
	defer cancel()
	// Bounded Job-only list, not a machine process inventory. Opened children must
	// prove membership through the *opened handle*, so recycled PIDs cannot escape
	// the captured generation. Root observations use the original held object.
	word := unsafe.Sizeof(uintptr(0))
	size := uintptr(8) + resources.MaxProcesses*word
	buffer := make([]uintptr, (size+word-1)/word)
	base := unsafe.Pointer(&buffer[0])
	ok, _, queryErr := queryJob.Call(uintptr(job), 3, uintptr(base), size, 0)
	reason := "job_membership_observed_nonatomic"
	pids := []uintptr{}
	if ok != 0 {
		assigned := *(*uint32)(base)
		listed := *(*uint32)(unsafe.Add(base, 4))
		if listed <= resources.MaxProcesses && listed <= assigned {
			pids = append(pids, unsafe.Slice((*uintptr)(unsafe.Add(base, 8)), int(listed))...)
			if listed != assigned {
				reason = "partial_job_list"
			}
		} else {
			reason = "invalid_job_list"
		}
	} else if queryErr == windows.ERROR_MORE_DATA {
		reason = "process_limit"
	} else {
		reason = "job_list_unavailable"
	}
	rootPID, _ := windows.GetProcessId(root)
	readings := []resources.ProcessReading{}
	observe := func(h windows.Handle) {
		member, err := processInOwnedJob(h, job)
		status, waitErr := windows.WaitForSingleObject(h, 0)
		if err != nil || !member || waitErr != nil || status != uint32(windows.WAIT_TIMEOUT) {
			reason = "partial_process_observation"
			return
		}
		sample := resources.WindowsProcess(h)
		pid, pidErr := windows.GetProcessId(h)
		var created, exited, kern, user windows.Filetime
		if pidErr != nil || windows.GetProcessTimes(h, &created, &exited, &kern, &user) != nil {
			reason = "process_identity_unavailable"
			return
		}
		sample.Identity = fmt.Sprintf("%d:%d", pid, uint64(created.HighDateTime)<<32|uint64(created.LowDateTime))
		readings = append(readings, sample)
	}
	observe(root)
	for _, pid := range pids {
		if bounded.Err() != nil {
			reason = "process_observation_budget"
			break
		}
		if uint32(pid) == rootPID {
			continue
		}
		if pid == 0 || pid > uintptr(^uint32(0)) {
			reason = "invalid_job_pid"
			continue
		}
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ|windows.SYNCHRONIZE, false, uint32(pid))
		if err != nil {
			reason = "partial_process_access"
			continue
		}
		observe(h)
		windows.CloseHandle(h)
	}
	sort.Slice(readings, func(a, b int) bool { return readings[a].Identity < readings[b].Identity })
	return sumOwnedReadings(readings, "windows_process_api", "root_and_observed_job_members", reason)
}
