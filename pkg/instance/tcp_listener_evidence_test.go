package instance

import (
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"
)

type tcpEvidenceRow struct {
	port  int
	state uint32
}

func tcpEvidenceTable(ipv6 bool, rows ...tcpEvidenceRow) []byte {
	size, stateOffset, portOffset := 24, 0, 8
	if ipv6 {
		size, stateOffset, portOffset = 56, 48, 20
	}
	table := make([]byte, 4+len(rows)*size)
	binary.LittleEndian.PutUint32(table, uint32(len(rows)))
	for n, row := range rows {
		data := table[4+n*size : 4+(n+1)*size]
		binary.LittleEndian.PutUint32(data[stateOffset:], row.state)
		binary.BigEndian.PutUint16(data[portOffset:], uint16(row.port))
	}
	return table
}

func TestReleasedTCPPortSettlesTransientListen(t *testing.T) {
	for _, family := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		calls := 0
		query := func(observed context.Context, ipv6 bool) ([]byte, error) {
			if observed != ctx {
				t.Fatal("settling replaced the operation context")
			}
			round := calls / 2
			if ipv6 != (calls%2 == 1) {
				t.Fatalf("missing address-family observation: call=%d ipv6=%v", calls, ipv6)
			}
			calls++
			if round == 0 && ipv6 == family {
				return tcpEvidenceTable(ipv6, tcpEvidenceRow{12345, tcpStateListen}), nil
			}
			return tcpEvidenceTable(ipv6), nil
		}
		err := releasedTCPPort(ctx, 12345, query)
		cancel()
		if err != nil || calls != 4 {
			t.Fatalf("LISTEN then empty must settle using both tables: ipv6=%v calls=%d err=%v", family, calls, err)
		}
	}
}

func TestReleasedTCPPortSettlingPersistentAndCanceled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	calls := 0
	err := releasedTCPPort(ctx, 12345, func(_ context.Context, ipv6 bool) ([]byte, error) {
		calls++
		return tcpEvidenceTable(ipv6, tcpEvidenceRow{12345, tcpStateListen}), nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "listener") || calls < 2 {
		t.Fatalf("persistent listener must remain unknown at original deadline: calls=%d err=%v", calls, err)
	}

	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	calls = 0
	err = releasedTCPPort(ctx, 12345, func(_ context.Context, ipv6 bool) ([]byte, error) {
		calls++
		if ipv6 {
			cancel()
		}
		return tcpEvidenceTable(ipv6, tcpEvidenceRow{12345, tcpStateListen}), nil
	})
	if !errors.Is(err, context.Canceled) || calls != 2 {
		t.Fatalf("cancellation must stop settling: calls=%d err=%v", calls, err)
	}
}

func TestReleasedTCPPortSettlingDoesNotHideUnknownEvidence(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		queryErr := errors.New("synthetic table API error")
		calls := 0
		err := releasedTCPPort(ctx, 12345, func(_ context.Context, ipv6 bool) ([]byte, error) {
			calls++
			if calls == 4 {
				if malformed {
					return []byte{1, 0, 0, 0}, nil
				}
				return nil, queryErr
			}
			if calls == 1 {
				return tcpEvidenceTable(ipv6, tcpEvidenceRow{12345, tcpStateListen}), nil
			}
			return tcpEvidenceTable(ipv6), nil
		})
		cancel()
		if err == nil || calls != 4 {
			t.Fatalf("settling did not observe the complete unknown-evidence sequence: calls=%d err=%v", calls, err)
		}
		if !strings.Contains(err.Error(), "IPv6") || !strings.Contains(err.Error(), "sample 2") {
			t.Fatal("error lost observation context", err)
		}
		if !malformed && !errors.Is(err, queryErr) {
			t.Fatal("underlying API error lost", err)
		}
	}
}

func TestReleasedTCPPortDistinguishesTimeWaitFromListener(t *testing.T) {
	const port = 0x1234
	for _, ipv6 := range []bool{false, true} {
		table := tcpEvidenceTable(ipv6, tcpEvidenceRow{port, tcpStateTimeWait}, tcpEvidenceRow{0x3412, tcpStateListen})
		if found, err := tcpTablePortHasState(table, ipv6, port, tcpStateTimeWait); err != nil || !found {
			t.Fatal("TIME_WAIT not decoded", found, err)
		}
		calls := 0
		query := func(_ context.Context, family bool) ([]byte, error) {
			calls++
			if family == ipv6 {
				return table, nil
			}
			return tcpEvidenceTable(family), nil
		}
		if err := releasedTCPPort(context.Background(), port, query); err != nil || calls != 2 {
			t.Fatalf("TIME_WAIT without listener: %v calls=%d", err, calls)
		}
		table = tcpEvidenceTable(ipv6, tcpEvidenceRow{port, tcpStateTimeWait}, tcpEvidenceRow{port, tcpStateListen})
		if err := releasedTCPPort(context.Background(), port, query); err == nil {
			t.Fatal("genuine listener was ignored")
		}
	}
}

func TestReleasedTCPPortMissingEvidenceFailsClosed(t *testing.T) {
	for _, family := range []bool{false, true} {
		queryErr := errors.New("injected TCP table failure")
		query := func(_ context.Context, ipv6 bool) ([]byte, error) {
			if ipv6 == family {
				return nil, queryErr
			}
			return tcpEvidenceTable(ipv6), nil
		}
		if err := releasedTCPPort(context.Background(), 12345, query); !errors.Is(err, queryErr) {
			t.Fatal(err)
		}
		for _, table := range [][]byte{nil, {0, 0, 0}, {1, 0, 0, 0}, tcpEvidenceTable(family, tcpEvidenceRow{12345, 0})} {
			query = func(_ context.Context, ipv6 bool) ([]byte, error) {
				if ipv6 == family {
					return table, nil
				}
				return tcpEvidenceTable(ipv6), nil
			}
			if err := releasedTCPPort(context.Background(), 12345, query); err == nil {
				t.Fatal("malformed TCP evidence accepted", family, table)
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := releasedTCPPort(ctx, 12345, func(context.Context, bool) ([]byte, error) { t.Fatal("canceled query ran"); return nil, nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	if err := releasedTCPPort(ctx, 12345, func(_ context.Context, ipv6 bool) ([]byte, error) {
		if ipv6 {
			cancel()
		}
		return tcpEvidenceTable(ipv6), nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatal("late cancellation ignored", err)
	}
}
