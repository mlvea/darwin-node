//go:build !darwin

package guest

import (
	"fmt"
	"net"
)

func DialHostVsock(port uint32) (net.Conn, error) {
	return nil, fmt.Errorf("vsock is only available on darwin guests")
}
