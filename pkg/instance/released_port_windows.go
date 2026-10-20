//go:build windows

package instance

import (
	"context"
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var getExtendedTCPTable = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")

// A failed bind is not listener evidence on Windows: accepted connections can
// retain TIME_WAIT after the Job is empty. Observe both IP families instead.
// Conservatively reject any listener on this port, even at a different address.
func releasedPortState(ctx context.Context, _ string, port int) error {
	return releasedTCPPort(ctx, port, windowsTCPTable)
}

func windowsTCPTable(ctx context.Context, ipv6 bool) ([]byte, error) {
	if err := getExtendedTCPTable.Find(); err != nil {
		return nil, err
	}
	family := uintptr(2) // AF_INET
	if ipv6 {
		family = 23
	} // AF_INET6
	const ownerPIDAll = 5 // TCP_TABLE_OWNER_PID_ALL (also exposes TIME_WAIT for diagnostics).
	const maxTableBytes = 16 << 20
	size := uint32(4096)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if size < 4 || size > maxTableBytes {
			return nil, fmt.Errorf("invalid TCP table buffer size")
		}
		buf := make([]byte, size)
		code, _, _ := getExtendedTCPTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0, family, ownerPIDAll, 0)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if code == 0 {
			if size > uint32(len(buf)) {
				return nil, fmt.Errorf("incomplete TCP table result")
			}
			return buf[:size], nil
		}
		if code != uintptr(windows.ERROR_INSUFFICIENT_BUFFER) {
			return nil, fmt.Errorf("GetExtendedTcpTable family=%d code=%d: %w", family, code, syscall.Errno(code))
		}
		if size <= uint32(len(buf)) {
			return nil, fmt.Errorf("invalid TCP table resize")
		}
	}
}
