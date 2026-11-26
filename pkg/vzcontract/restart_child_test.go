package vzcontract

import (
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if ChildEnabled() {
		fmt.Fprintln(os.Stderr, "hardware-pending: supervisor child has no Virtualization.framework backend")
		os.Exit(2)
	}
	os.Exit(m.Run())
}
