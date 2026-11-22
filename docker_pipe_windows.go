//go:build windows

package main

import (
	"context"
	"net"

	"github.com/Microsoft/go-winio"
)

func dialDockerPipe(ctx context.Context, addr string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, addr)
}
