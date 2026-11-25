package simvz

import (
	"math/rand/v2"
	"time"

	"github.com/mlvea/darwin-node/pkg/types"
)

// Faults is the per-test injection plan. The zero value boots a VM that
// reaches Running and keeps its guest. A runtime-wide FailStart budget is
// consumed by successive Start calls, including Starts of later machines,
// so an engine that creates a fresh machine on each retry still observes
// a bounded number of failures.
type Faults struct {
	// FailStart is how many Start calls fail before one is allowed to
	// succeed. Each failure has already opened the NIC socketpair and the
	// vsock listener; both are closed before Start returns.
	FailStart int
	// VsockEOFAfter closes the host's view of the vsock stream after this
	// many bytes. Zero disables the fault. A value inside the first frame
	// (the 4-byte length plus a short body) is a mid-frame EOF.
	VsockEOFAfter int
	// Stall is how long the host's first write of a vsock connection waits
	// before the handshake is delivered. Logical time: a ScaledClock
	// shortens the sleep. Longer than the guest idle timeout, the agent
	// closes the session (it has seen no frame since accept) while the
	// write is still parked.
	Stall time.Duration
	// CloneDelay is the logical pause inside the disk clone, before the
	// guest is started.
	CloneDelay time.Duration
	// CrashGuest kills the guest after the VM has been created and, when
	// CrashAfter is positive, after that logical delay. The VM stays
	// Running with the agent gone, which is a dead guest rather than a
	// stopped machine. CrashAfter of zero kills before Start returns.
	CrashGuest bool
	CrashAfter time.Duration
}

// FaultsFromSeed is a deterministic plan. The same seed always returns
// the same Faults. Options uses it only when Seed is set and Faults is
// the zero value; an explicit Faults value wins.
func FaultsFromSeed(seed int64) Faults {
	// The second PCG word is a fixed domain separator so this stream is
	// not the default stream of a zero Options.
	r := rand.New(rand.NewPCG(uint64(seed), 0x53494D565A))
	f := Faults{
		FailStart:     r.IntN(3),
		VsockEOFAfter: r.IntN(32),
		Stall:         time.Duration(r.IntN(40)) * time.Second,
		CloneDelay:    time.Duration(r.IntN(6)) * time.Second,
	}
	if r.IntN(2) == 1 {
		f.CrashGuest = true
		f.CrashAfter = time.Duration(r.IntN(5)) * time.Second
	}
	return f
}

// Options configures one simulator runtime.
type Options struct {
	// Seed derives Faults when Faults is the zero value. Zero means the
	// explicit Faults (or no faults) are used as given.
	Seed int64
	// Dir is the parent of per-VM clone directories. Empty uses the
	// process temp dir. Vsock paths stay on /tmp so the socket name fits
	// in sockaddr_un.
	Dir string
	// Clock scales fault sleeps. Nil is the wall clock.
	Clock Clock
	// GuestBinary is a darwin-guest-agent executable. Empty serves the
	// protocol in-process on the vsock unix socket.
	GuestBinary string
	Faults      Faults
	// MaxRunning caps concurrent VMs in Starting, Running, Pausing,
	// Paused, Saving, or Stopping. Zero uses Apple's cap of 2. Values
	// above 2 are clamped: the simulator does not outrun the EULA.
	MaxRunning int
	// Transition is the logical pause spent in Starting, Pausing, Saving,
	// and Stopping so a test can observe the intermediate state.
	Transition time.Duration
}

func (o Options) maxRunning() int {
	n := o.MaxRunning
	if n <= 0 || n > types.AppleMaxConcurrentVMs {
		return types.AppleMaxConcurrentVMs
	}
	return n
}

func (o Options) resolvedFaults() Faults {
	if o.Seed != 0 && o.Faults == (Faults{}) {
		return FaultsFromSeed(o.Seed)
	}
	return o.Faults
}
