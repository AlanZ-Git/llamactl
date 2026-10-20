//go:build !windows

package instance

import (
	"context"
	"net"
	"strconv"
)

func releasedPortState(ctx context.Context, host string, port int) error {
	lc := net.ListenConfig{}
	ln, err := lc.Listen(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	return ln.Close()
}
