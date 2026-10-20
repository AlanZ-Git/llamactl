package instance

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"time"
)

const tcpStateListen = 2
const tcpStateTimeWait = 11

// Windows IP Helper table interpretation is separated for portable regression
// tests. GetExtendedTcpTable returns DWORD count + MIB_TCP{,6}ROW_OWNER_PID rows.
// No PID is used: the table is observation only, never termination authority.
func tcpTablePortHasState(table []byte, ipv6 bool, port int, wanted uint32) (bool, error) {
	rowSize, stateOffset, portOffset := 24, 0, 8
	if ipv6 {
		rowSize, stateOffset, portOffset = 56, 48, 20
	}
	if len(table) < 4 {
		return false, fmt.Errorf("incomplete TCP table header")
	}
	count := binary.LittleEndian.Uint32(table[:4])
	if uint64(count) > uint64((len(table)-4)/rowSize) {
		return false, fmt.Errorf("incomplete TCP table rows: count=%d bytes=%d row_size=%d", count, len(table), rowSize)
	}
	found := false
	for n := 0; n < int(count); n++ {
		row := table[4+n*rowSize : 4+(n+1)*rowSize]
		state := binary.LittleEndian.Uint32(row[stateOffset : stateOffset+4])
		if state < 1 || state > 12 {
			return false, fmt.Errorf("invalid TCP row state: row=%d state=%d", n, state)
		}
		// Port is in network byte order in the first two bytes of its DWORD.
		localPort := int(binary.BigEndian.Uint16(row[portOffset : portOffset+2]))
		if state == wanted && localPort == port {
			found = true
		}
	}
	return found, nil
}

// Job exit and TCP listener-table retirement need not be simultaneous. Settle
// LISTEN observations only within the supplied operation deadline. Query/parser
// failures remain unknown immediately; they are never treated as an empty table.
func releasedTCPPort(ctx context.Context, port int, query func(context.Context, bool) ([]byte, error)) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("invalid TCP port")
	}
	var last error
	expired := func() error {
		if err := ctx.Err(); err != nil {
			if last != nil {
				return fmt.Errorf("%v: %w", last, err)
			}
			return err
		}
		return nil
	}
	for sample := 1; ; sample++ {
		var listening [2]bool
		for index, ipv6 := range []bool{false, true} {
			if err := expired(); err != nil {
				return err
			}
			family := "IPv4"
			if ipv6 {
				family = "IPv6"
			}
			table, err := query(ctx, ipv6)
			if err != nil {
				return fmt.Errorf("read %s TCP table (sample %d): %w", family, sample, err)
			}
			listening[index], err = tcpTablePortHasState(table, ipv6, port, tcpStateListen)
			if err != nil {
				return fmt.Errorf("decode %s TCP table (sample %d): %w", family, sample, err)
			}
		}
		if err := expired(); err != nil {
			return err
		}
		if !listening[0] && !listening[1] {
			if sample > 1 {
				log.Printf("Owner release TCP port %d LISTEN observation cleared after %d complete samples", port, sample)
			}
			return ctx.Err()
		}
		last = fmt.Errorf("TCP listener remains (IPv4=%t IPv6=%t)", listening[0], listening[1])
		if _, bounded := ctx.Deadline(); !bounded {
			return last
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("%v: %w", last, ctx.Err())
		case <-timer.C:
		}
	}
}
