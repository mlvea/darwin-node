//go:build darwin && arm64

package vzcontract

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/Code-Hex/vz/v3"
	"github.com/mlvea/darwin-node/pkg/runtime"
	vzruntime "github.com/mlvea/darwin-node/pkg/runtime/vz"
	"github.com/mlvea/darwin-node/pkg/types"
)

// errHardwarePending is returned when a test is asked to boot a real VM.
// Compiling this file is the CI gate. Execution needs a Mac Mini, a signed
// helper, and a restore bundle this tree does not ship.
var errHardwarePending = fmt.Errorf("hardware-pending: Virtualization.framework execution is not wired")

// Record appends state_change lines from vm's KVO channel until ctx is
// cancelled. Promotion into testdata/vz_traces is a manual edit: label
// source "hardware", attach the op that produced the lines, and drop the
// derived note. Recording itself is hardware-pending.
func Record(ctx context.Context, vm *vz.VirtualMachine, w io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if vm == nil || w == nil {
		return fmt.Errorf("hardware-pending: nil recorder target")
	}
	for _, dev := range vm.SocketDevices() {
		_ = dev
	}
	ch := vm.StateChangedNotify()
	seq := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case st, ok := <-ch:
			if !ok {
				return io.EOF
			}
			seq++
			name := strings.TrimPrefix(st.String(), "VirtualMachineState")
			ev := []Event{{Kind: "state_change", State: name}}
			if err := Append(w, Line{
				Seq:    seq,
				Op:     "observe",
				Source: "hardware",
				Events: &ev,
			}); err != nil {
				return err
			}
		}
	}
}

// dialPort is the hardware ConnectToPort shape. Code-Hex blocks on the
// completion channel. Apple's comment says a guest that is not listening
// makes the call do nothing, so a future harness must bound it. The
// contract's fail-fast ECONNREFUSED behaviour is a derived hypothesis
// until that harness records one.
func dialPort(dev *vz.VirtioSocketDevice, port uint32) (net.Conn, error) {
	if dev == nil {
		return nil, errHardwarePending
	}
	c, err := dev.Connect(port)
	if err != nil || c == nil {
		return nil, err
	}
	return c, nil
}

func hardwareFactory(t interface{ Helper() }, _ Config) Runtime {
	if t != nil {
		t.Helper()
	}
	_ = vzruntime.New(runtime.Options{}).Name()
	_ = types.RuntimeVZ
	return &hwRuntime{record: Record}
}

type hwRuntime struct {
	record func(context.Context, *vz.VirtualMachine, io.Writer) error
}

func (r *hwRuntime) Create(context.Context, Spec) (VM, error) {
	if r.record == nil {
		return nil, errHardwarePending
	}
	return &hwVM{}, errHardwarePending
}

func (r *hwRuntime) Running() int { return 0 }

func (r *hwRuntime) ArmBarrier(string) {}

func (r *hwRuntime) WaitBarrier(context.Context, string) error { return errHardwarePending }

func (r *hwRuntime) ReleaseBarrier(string) {}

func (r *hwRuntime) OrderSeed() int64 { return 0 }

func (r *hwRuntime) Now() time.Time { return time.Now() }

type hwVM struct {
	dev *vz.VirtioSocketDevice
}

func (v *hwVM) State() string { return "Stopped" }

func (v *hwVM) Events() []Event { return nil }

func (v *hwVM) QuotaHeld() bool { return false }

func (v *hwVM) Start(context.Context) error { return errHardwarePending }

func (v *hwVM) Stop(context.Context) error { return errHardwarePending }

func (v *hwVM) RequestStop(context.Context) error { return errHardwarePending }

func (v *hwVM) Pause(context.Context) error { return errHardwarePending }

func (v *hwVM) Resume(context.Context) error { return errHardwarePending }

func (v *hwVM) Save(context.Context, string) error { return errHardwarePending }

func (v *hwVM) Restore(context.Context, string) error { return errHardwarePending }

func (v *hwVM) Connect(context.Context) (net.Conn, error) {
	return dialPort(v.dev, uint32(types.GuestVsockPort))
}

func (v *hwVM) RegisterAgent(context.Context) error { return errHardwarePending }

func (v *hwVM) HostNIC() *os.File { return nil }

func (v *hwVM) WriteDatagram([]byte) error { return errHardwarePending }

func (v *hwVM) OnQueue(fn func()) {
	if fn != nil {
		fn()
	}
}

func (v *hwVM) Crash() error { return errHardwarePending }

func (v *hwVM) DialAgent(context.Context) error { return errHardwarePending }
