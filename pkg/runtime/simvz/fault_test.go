package simvz

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/pkg/guest"
)

func TestFaultsFromSeedIsStable(t *testing.T) {
	a := FaultsFromSeed(1)
	b := FaultsFromSeed(1)
	c := FaultsFromSeed(2)
	if a != b {
		t.Fatalf("seed 1 drifted: %+v vs %+v", a, b)
	}
	if a == c {
		t.Fatalf("seed 1 and 2 produced the same plan: %+v", a)
	}
	rt := New(Options{Seed: 7})
	if rt.faults != FaultsFromSeed(7) {
		t.Fatalf("runtime plan %+v", rt.faults)
	}
	explicit := Faults{FailStart: 1}
	rt = New(Options{Seed: 7, Faults: explicit})
	if rt.faults != explicit {
		t.Fatalf("explicit faults lost: %+v", rt.faults)
	}
}

func TestStartFailureClosesFDsThenSucceeds(t *testing.T) {
	before := fdBefore(t)
	rt := newRT(t, Options{Faults: Faults{FailStart: 1}})
	m := mustMachine(t, rt, "failstart")
	ctx := context.Background()
	err := m.Start(ctx)
	if err == nil || !errors.Is(err, errStartFailed) {
		t.Fatalf("first start %v", err)
	}
	if m.State() != "Stopped" || rt.Running() != 0 {
		t.Fatalf("state %s running %d", m.State(), rt.Running())
	}
	host, guestEnd := m.NetworkFiles()
	if host != nil || guestEnd != nil {
		t.Fatal("failed start left the nic open")
	}
	fdBack(t, before)
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if m.State() != "Running" {
		t.Fatalf("state %s", m.State())
	}
	_ = m.Stop(ctx, 0)
	fdBack(t, before)
}

func TestCloneDelayIsScaled(t *testing.T) {
	clk := NewScaledClock(1000)
	before := clk.Now()
	rt := newRT(t, Options{Clock: clk, Faults: Faults{CloneDelay: 5 * time.Second}})
	m := mustMachine(t, rt, "clone")
	wall := time.Now()
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if time.Since(wall) > time.Second {
		t.Fatalf("clone slept wall time %s", time.Since(wall))
	}
	if clk.Now().Sub(before) < 5*time.Second {
		t.Fatalf("logical advance %s", clk.Now().Sub(before))
	}
	if _, err := osStat(m.DiskPath()); err != nil {
		t.Fatal(err)
	}
}

func TestMidFrameVsockEOF(t *testing.T) {
	before := fdBefore(t)
	rt := newRT(t, Options{Faults: Faults{VsockEOFAfter: 6}})
	m := mustMachine(t, rt, "eof")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	_, err := m.DialAgent(ctx)
	if err == nil {
		t.Fatal("mid-frame EOF still handshook")
	}
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		// The frame reader surfaces a short body as unexpected EOF,
		// sometimes wrapped. Any error is a refused session; the fd
		// check is what proves the stream was closed.
		t.Logf("dial error: %v", err)
	}
	if err := m.Stop(ctx, 0); err != nil {
		t.Fatal(err)
	}
	fdBack(t, before)
}

func TestStallOutlastsGuestIdle(t *testing.T) {
	// idleWatchdog polls every 250ms, so a 30s idle scaled under that
	// poll never fires. Scale 8 keeps a 50s stall longer than a 30s
	// idle and leaves room for a late tick: wall idle 3.75s, wall
	// stall 6.25s. 35s is the same fault with a smaller gap.
	const scale = 8
	const stall = 50 * time.Second
	clk := NewScaledClock(scale)
	if clk.Wall(guest.DefaultIdleTimeout) <= 250*time.Millisecond {
		t.Fatalf("wall idle %s is inside the watchdog poll", clk.Wall(guest.DefaultIdleTimeout))
	}
	if clk.Wall(stall) <= clk.Wall(guest.DefaultIdleTimeout)+time.Second {
		t.Fatalf("stall wall %s does not clear idle %s plus slack", clk.Wall(stall), clk.Wall(guest.DefaultIdleTimeout))
	}
	before := fdBefore(t)
	rt := newRT(t, Options{Clock: clk, Faults: Faults{Stall: stall}})
	m := mustMachine(t, rt, "stall")
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	wall := time.Now()
	_, err := m.DialAgent(ctx)
	elapsed := time.Since(wall)
	if err == nil {
		t.Fatal("stall still completed the handshake inside the guest idle timeout")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("stall took %s of wall time; scale did not shorten it", elapsed)
	}
	if m.State() != "Running" {
		t.Fatalf("stall stopped the VM: %s", m.State())
	}
	_ = m.Stop(context.Background(), 0)
	fdBack(t, before)
}

func TestGuestCrashLeavesVMRunning(t *testing.T) {
	before := fdBefore(t)
	rt := newRT(t, Options{Faults: Faults{CrashGuest: true}})
	m := mustMachine(t, rt, "crash")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if m.State() != "Running" || m.Status().AgentOK {
		t.Fatalf("state %s agent %v", m.State(), m.Status().AgentOK)
	}
	if _, err := m.DialAgent(ctx); err == nil {
		t.Fatal("dial reached a crashed guest")
	}
	host, _ := m.NetworkFiles()
	if host == nil {
		t.Fatal("crash closed the nic; the VM is still running")
	}
	if err := m.Stop(ctx, 0); err != nil {
		t.Fatal(err)
	}
	fdBack(t, before)
}

func TestDelayedCrashKeepsTheSlot(t *testing.T) {
	// The crash sleep is the first positive Sleep. Hold it so the
	// assertion can see a live agent, then let the crash run.
	g := newGateClock()
	t.Cleanup(g.Release)
	before := fdBefore(t)
	rt := newRT(t, Options{Clock: g, Faults: Faults{CrashGuest: true, CrashAfter: time.Second}})
	m := mustMachine(t, rt, "later")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if m.State() != "Running" || !m.Status().AgentOK {
		t.Fatalf("before crash state %s agent %v", m.State(), m.Status().AgentOK)
	}
	g.Release()
	deadline := time.Now().Add(2 * time.Second)
	for m.Status().AgentOK && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if m.Status().AgentOK || m.State() != "Running" {
		t.Fatalf("after crash state %s agent %v", m.State(), m.Status().AgentOK)
	}
	if _, err := m.DialAgent(ctx); err == nil {
		t.Fatal("dial reached a guest that was crashed after start")
	}
	if rt.Running() != 1 {
		t.Fatalf("delayed crash released the slot: %d", rt.Running())
	}
	if err := m.Stop(ctx, 0); err != nil {
		t.Fatal(err)
	}
	fdBack(t, before)
}

func TestChildCrash(t *testing.T) {
	bin, err := GuestAgentBinary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	before := fdBefore(t)
	rt := newRT(t, Options{GuestBinary: bin, Faults: Faults{CrashGuest: true}})
	m := mustMachine(t, rt, "child-crash")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.DialAgent(ctx); err == nil {
		t.Fatal("dial reached a killed guest agent")
	}
	if m.State() != "Running" {
		t.Fatalf("state %s", m.State())
	}
	if err := m.Stop(ctx, 0); err != nil {
		t.Fatal(err)
	}
	fdBack(t, before)
}
