package vzcontract

import (
	"context"
	"net"
	"os"
	"testing"
	"time"
)

// Spec is the identity Restore compares. Empty fields are filled by the
// factory with one stable default so two creates of the same Spec match.
type Spec struct {
	Name          string
	Namespace     string
	UID           string
	HardwareModel string
	MAC           string
	CPU           uint
	MemoryBytes   uint64
}

// Config is the per-runtime injection the suite asks the factory for.
// ManualClock makes Stall and ConnectDelay advance logical time without
// sleeping the wall clock. Transition is a logical pause inside the
// intermediate states; zero is instant.
type Config struct {
	Order           string
	OrderSeed       int64
	HoldAgent       bool
	ConnectDelay    time.Duration
	DatagramENOBUFS int
	Stall           time.Duration
	ManualClock     bool
	Transition      time.Duration
}

// VM is one machine. State names match Virtualization.framework
// (Stopped, Starting, Running, Pausing, Paused, Saving, Stopping,
// Resuming, Restoring, Error).
type VM interface {
	State() string
	Events() []Event
	QuotaHeld() bool
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	RequestStop(ctx context.Context) error
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
	Save(ctx context.Context, path string) error
	Restore(ctx context.Context, path string) error
	Connect(ctx context.Context) (net.Conn, error)
	RegisterAgent(ctx context.Context) error
	HostNIC() *os.File
	WriteDatagram(p []byte) error
	OnQueue(fn func())
	Crash() error
	DialAgent(ctx context.Context) error
}

// Runtime is the factory product. Barrier names are the BarrierBefore*
// constants. An unknown or unarmed barrier returns immediately from the
// actor side; WaitBarrier on one that was not armed returns an error.
type Runtime interface {
	Create(ctx context.Context, spec Spec) (VM, error)
	Running() int
	ArmBarrier(name string)
	WaitBarrier(ctx context.Context, name string) error
	ReleaseBarrier(name string)
	OrderSeed() int64
	Now() time.Time
}

// Factory builds a runtime for one subtest. The factory owns cleanup of
// every VM it creates.
type Factory func(t *testing.T, cfg Config) Runtime
