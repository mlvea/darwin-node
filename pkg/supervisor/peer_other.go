//go:build !linux && !darwin

package supervisor

import (
	"fmt"
	"net"
)

func peerUID(conn *net.UnixConn) (int, error) {
	_ = conn
	return 0, fmt.Errorf("peer credentials are not implemented on this OS")
}
