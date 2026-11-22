//go:build !windows

package main

import (
	"context"
	"fmt"
	"net"
)

func dialDockerPipe(ctx context.Context, addr string) (net.Conn, error) {
	return nil, fmt.Errorf("Windows named pipes are unsupported on this platform")
}
