package simvz

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/pkg/types"
	"github.com/mlvea/darwin-node/pkg/vzcontract"
)

func TestBarrierNamesMatchContract(t *testing.T) {
	pairs := [][2]string{
		{BarrierBeforeGuestDidStop, vzcontract.BarrierBeforeGuestDidStop},
		{BarrierBeforeKVOStopped, vzcontract.BarrierBeforeKVOStopped},
		{BarrierBeforeStopCompletion, vzcontract.BarrierBeforeStopCompletion},
		{BarrierBeforeKVOStarting, vzcontract.BarrierBeforeKVOStarting},
		{BarrierBeforeKVORunning, vzcontract.BarrierBeforeKVORunning},
		{BarrierBeforeStartCompletion, vzcontract.BarrierBeforeStartCompletion},
		{BarrierBeforeKVOError, vzcontract.BarrierBeforeKVOError},
		{BarrierBeforeConnectCompletion, vzcontract.BarrierBeforeConnectCompletion},
		{BarrierBeforeSaveCompletion, vzcontract.BarrierBeforeSaveCompletion},
		{BarrierBeforeRestoreCompletion, vzcontract.BarrierBeforeRestoreCompletion},
		{BarrierBeforeKVOStopping, vzcontract.BarrierBeforeKVOStopping},
		{BarrierBeforeKVOPausing, vzcontract.BarrierBeforeKVOPausing},
		{BarrierBeforeKVOSaving, vzcontract.BarrierBeforeKVOSaving},
		{BarrierBeforeKVOResuming, vzcontract.BarrierBeforeKVOResuming},
		{BarrierBeforeKVORestoring, vzcontract.BarrierBeforeKVORestoring},
		{OrderKVOBeforeCompletion, vzcontract.OrderKVOBeforeCompletion},
		{OrderCompletionBeforeKVO, vzcontract.OrderCompletionBeforeKVO},
		{OrderJitter, vzcontract.OrderJitter},
	}
	for _, pair := range pairs {
		if pair[0] != pair[1] {
			t.Fatalf("barrier/order %q != %q", pair[0], pair[1])
		}
	}
}

func TestVZContract(t *testing.T) {
	vzcontract.Run(t, simFactory)
}

func TestVZTraceReplay(t *testing.T) {
	vzcontract.ReplayDir(t, simFactory, vzcontract.TestdataDir())
}

func simFactory(t *testing.T, cfg vzcontract.Config) vzcontract.Runtime {
	t.Helper()
	var clock Clock
	if cfg.ManualClock {
		clock = NewManualClock()
	}
	rt := New(Options{
		Clock:      clock,
		Order:      cfg.Order,
		OrderSeed:  cfg.OrderSeed,
		Transition: cfg.Transition,
		Faults: Faults{
			HoldAgent:       cfg.HoldAgent,
			ConnectDelay:    cfg.ConnectDelay,
			DatagramENOBUFS: cfg.DatagramENOBUFS,
			Stall:           cfg.Stall,
		},
	})
	if cfg.Order == OrderJitter {
		seed := rt.OrderSeed()
		t.Cleanup(func() {
			if t.Failed() {
				t.Errorf("simvz jitter seed %d", seed)
			}
		})
	}
	return &simRT{t: t, rt: rt}
}

type simRT struct {
	t  *testing.T
	rt *Runtime
}

func (r *simRT) Create(ctx context.Context, spec vzcontract.Spec) (vzcontract.VM, error) {
	m, err := r.rt.Create(ctx, contractSpec(spec))
	if err != nil {
		return nil, err
	}
	mm := m.(*machine)
	r.t.Cleanup(func() {
		_ = mm.Stop(context.Background(), 0)
		mm.shutdown()
	})
	r.t.Cleanup(func() {
		for _, name := range allBarriers() {
			r.rt.ReleaseBarrier(name)
		}
	})
	return &simVM{m: mm}, nil
}

func (r *simRT) Running() int { return r.rt.Running() }

func (r *simRT) ArmBarrier(name string) { r.rt.ArmBarrier(name) }

func (r *simRT) WaitBarrier(ctx context.Context, name string) error {
	return r.rt.WaitBarrier(ctx, name)
}

func (r *simRT) ReleaseBarrier(name string) { r.rt.ReleaseBarrier(name) }

func (r *simRT) OrderSeed() int64 { return r.rt.OrderSeed() }

func (r *simRT) Now() time.Time {
	if r.rt == nil || r.rt.clock == nil {
		return time.Now()
	}
	return r.rt.clock.Now()
}

type simVM struct {
	m *machine
}

func (v *simVM) State() string { return v.m.State() }

func (v *simVM) QuotaHeld() bool { return v.m.QuotaHeld() }

func (v *simVM) Events() []vzcontract.Event {
	raw := v.m.eventsSnapshot()
	out := make([]vzcontract.Event, len(raw))
	for i, ev := range raw {
		out[i] = vzcontract.Event{Kind: ev.Kind, State: ev.State}
		if ev.Err != nil {
			out[i].Err = &vzcontract.ErrInfo{
				Domain: ev.Err.Domain,
				Code:   ev.Err.Code,
				Name:   ev.Err.Name,
				Errno:  ev.Err.Errno,
			}
		}
	}
	return out
}

func (v *simVM) Start(ctx context.Context) error { return v.m.Start(ctx) }

func (v *simVM) Stop(ctx context.Context) error { return v.m.Stop(ctx, 0) }

func (v *simVM) RequestStop(ctx context.Context) error { return v.m.RequestStop(ctx) }

func (v *simVM) Pause(ctx context.Context) error { return v.m.Pause(ctx) }

func (v *simVM) Resume(ctx context.Context) error { return v.m.Resume(ctx) }

func (v *simVM) Save(ctx context.Context, path string) error { return v.m.SaveTo(ctx, path) }

func (v *simVM) Restore(ctx context.Context, path string) error { return v.m.Restore(ctx, path) }

func (v *simVM) Connect(ctx context.Context) (net.Conn, error) {
	return v.m.Connect(ctx)
}

func (v *simVM) RegisterAgent(ctx context.Context) error { return v.m.RegisterAgent(ctx) }

func (v *simVM) HostNIC() *os.File {
	host, _ := v.m.NetworkFiles()
	return host
}

func (v *simVM) WriteDatagram(p []byte) error { return v.m.WriteDatagram(p) }

func (v *simVM) OnQueue(fn func()) { v.m.OnQueue(fn) }

func (v *simVM) Crash() error { return v.m.Crash() }

func (v *simVM) DialAgent(ctx context.Context) error {
	cli, err := v.m.DialAgent(ctx)
	if cli != nil {
		_ = cli.Close()
	}
	return err
}

func contractSpec(s vzcontract.Spec) types.VMSpec {
	if s.Name == "" {
		s.Name = "vm"
	}
	if s.Namespace == "" {
		s.Namespace = "ns"
	}
	if s.UID == "" {
		s.UID = "uid"
	}
	if s.CPU == 0 {
		s.CPU = 2
	}
	if s.MemoryBytes == 0 {
		s.MemoryBytes = 1 << 30
	}
	if s.HardwareModel == "" {
		s.HardwareModel = "hw-model"
	}
	if s.MAC == "" {
		s.MAC = "02:00:00:00:00:11"
	}
	return types.VMSpec{
		ID:                types.MachineID{Namespace: s.Namespace, Name: s.Name, UID: s.UID},
		CPU:               s.CPU,
		MemoryBytes:       s.MemoryBytes,
		MAC:               s.MAC,
		HardwareModelData: s.HardwareModel,
		AgentToken:        "tok",
	}
}
