//go:build windows

package resources

import (
	"context"
	"encoding/binary"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var kernel = windows.NewLazySystemDLL("kernel32.dll")
var systemTimes = kernel.NewProc("GetSystemTimes")
var memoryStatus = kernel.NewProc("GlobalMemoryStatusEx")
var activeProcessors = kernel.NewProc("GetActiveProcessorCount")
var logicalInformation = kernel.NewProc("GetLogicalProcessorInformationEx")
var processMemory = windows.NewLazySystemDLL("psapi.dll").NewProc("GetProcessMemoryInfo")

func fileTicks(f windows.Filetime) uint64 { return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime) }

func collectSystem(ctx context.Context) SystemReading {
	r := SystemReading{At: time.Now(), Source: "windows_native", Reason: "get_system_times_failed", MemoryReason: "global_memory_status_failed"}
	if ctx.Err() != nil {
		r.Reason = "collection_canceled"
		return r
	}
	logical, _, _ := activeProcessors.Call(0xffff)
	if logical > 0 && logical <= 65536 {
		r.Logical = int(logical)
	}
	var idle, kern, user windows.Filetime
	if ok, _, _ := systemTimes.Call(uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kern)), uintptr(unsafe.Pointer(&user))); ok != 0 {
		r.Total = fileTicks(kern) + fileTicks(user)
		r.Idle = fileTicks(idle)
		r.CPUValid = r.Total > 0 && r.Idle <= r.Total
	}
	// GetSystemTimes describes the caller's primary processor group on >64-CPU
	// hosts; do not mislabel that as whole-host utilization.
	if r.Logical > 64 {
		r.CPUValid = false
		r.Reason = "processor_group_cpu_scope_unsupported"
	}
	var m struct {
		Length, Load                                                                          uint32
		TotalPhys, AvailPhys, TotalPage, AvailPage, TotalVirtual, AvailVirtual, AvailExtended uint64
	}
	m.Length = uint32(unsafe.Sizeof(m))
	if ok, _, _ := memoryStatus.Call(uintptr(unsafe.Pointer(&m))); ok != 0 && m.TotalPhys > 0 && m.AvailPhys <= m.TotalPhys {
		r.MemoryTotal = m.TotalPhys
		r.MemoryAvailable = m.AvailPhys
		r.MemoryValid = true
	}
	// RelationshipProcessorCore (0): count variable-size records, not group bits.
	var size uint32
	logicalInformation.Call(0, 0, uintptr(unsafe.Pointer(&size)))
	if size >= 8 && size <= 1024*1024 && ctx.Err() == nil {
		data := make([]byte, size)
		if ok, _, _ := logicalInformation.Call(0, uintptr(unsafe.Pointer(&data[0])), uintptr(unsafe.Pointer(&size))); ok != 0 && size <= uint32(len(data)) {
			count, valid := 0, true
			for offset := uint32(0); offset < size; {
				if size-offset < 8 {
					valid = false
					break
				}
				relationship := binary.LittleEndian.Uint32(data[offset:])
				length := binary.LittleEndian.Uint32(data[offset+4:])
				if length < 8 || length > size-offset {
					valid = false
					break
				}
				if relationship == 0 {
					count++
				}
				offset += length
			}
			if valid {
				r.Physical = count
			}
		}
	}
	return r
}

// WindowsProcess reads an already-held process object, never opens a PID. The
// caller proves membership and keeps the handle alive for the whole observation.
func WindowsProcess(handle windows.Handle) ProcessReading {
	r := ProcessReading{At: time.Now(), Source: "windows_process_api", Reason: "process_api_failed",
		CPUReason: "get_process_times_failed", WorkingSetReason: "get_process_memory_info_failed", PrivateReason: "get_process_memory_info_failed"}
	var created, exited, kern, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kern, &user); err == nil {
		ticks := fileTicks(kern) + fileTicks(user)
		if ticks <= ^uint64(0)/100 {
			ns := ticks * 100
			r.CPUTimeNS = &ns
		}
	}
	var m struct {
		Size, PageFaults                                                                                                               uint32
		PeakWorkingSet, WorkingSet, QuotaPeakPaged, QuotaPaged, QuotaPeakNonPaged, QuotaNonPaged, Pagefile, PeakPagefile, PrivateUsage uintptr
	}
	m.Size = uint32(unsafe.Sizeof(m))
	if ok, _, _ := processMemory.Call(uintptr(handle), uintptr(unsafe.Pointer(&m)), uintptr(m.Size)); ok != 0 {
		r.WorkingSet = Number(float64(m.WorkingSet))
		r.PrivateBytes = Number(float64(m.PrivateUsage))
	}
	return r
}
