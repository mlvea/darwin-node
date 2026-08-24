//go:build darwin && arm64

package vz

import "net"

func parseHardware(s string) (net.HardwareAddr, error) {
	return net.ParseMAC(s)
}
