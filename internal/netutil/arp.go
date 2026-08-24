package netutil

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// IPForMAC looks up an IPv4 in the host ARP table. Last-resort IP discovery
// when the guest agent has not reported NetInfo yet.
// Derived from Agoda macOS-vz-kubelet internal/netutil (Apache-2.0).
func IPForMAC(mac string) (string, error) {
	want := NormalizeMAC(mac)
	out, err := exec.Command("arp", "-an").Output()
	if err != nil {
		return "", err
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(strings.ToLower(line), strings.ToLower(want)) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) > 1 {
			return strings.Trim(fields[1], "()"), nil
		}
	}
	return "", fmt.Errorf("mac %s not in arp table", mac)
}
