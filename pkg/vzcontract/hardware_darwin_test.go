//go:build darwin && arm64

package vzcontract

import (
	"os"
	"testing"

	"github.com/Code-Hex/vz/v3"
	"github.com/mlvea/darwin-node/pkg/runtime"
	vzruntime "github.com/mlvea/darwin-node/pkg/runtime/vz"
	"github.com/mlvea/darwin-node/pkg/types"
)

func TestVZErrorCodeConstants(t *testing.T) {
	if vz.ErrorVirtualMachineLimitExceeded != 6 {
		t.Fatalf("limit code %d", vz.ErrorVirtualMachineLimitExceeded)
	}
	if vz.ErrorOperationCancelled != 9 {
		t.Fatalf("cancelled code %d", vz.ErrorOperationCancelled)
	}
	if vz.ErrorInvalidVirtualMachineState != 3 {
		t.Fatalf("invalid state code %d", vz.ErrorInvalidVirtualMachineState)
	}
	if vz.ErrorRestore != 12 {
		t.Fatalf("restore code %d", vz.ErrorRestore)
	}
	if vzruntime.New(runtime.Options{}).Name() != types.RuntimeVZ {
		t.Fatal("vz runtime name")
	}
}

func TestRealVZContract(t *testing.T) {
	if os.Getenv("DARWIN_NODE_VZ_HARDWARE") != "1" {
		t.Skip("hardware-pending: real Virtualization.framework execution needs a Mac Mini; this test only has to compile in CI")
	}
	Run(t, func(t *testing.T, cfg Config) Runtime {
		return hardwareFactory(t, cfg)
	})
}
