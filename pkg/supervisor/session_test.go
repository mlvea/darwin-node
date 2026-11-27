package supervisor

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/pkg/types"
	"golang.org/x/sys/unix"
)

// TestAdoptGuestDataDoesNotSpin is the death watch's idle contract.
// Bytes the guest has already written stay queued for the agent, so the
// watch must not treat them as a reason to poll again. A level-triggered
// peek burns a core for the life of the VM. Closing the supervisor
// afterwards must still mark the slot lost.
func TestAdoptGuestDataDoesNotSpin(t *testing.T) {
	before := fdCount()
	t.Cleanup(func() {
		if runtime.GOOS != "linux" {
			return
		}
		if got := fdCount(); got != before {
			t.Errorf("fd leak: before %d after %d\n%s", before, got, strings.Join(fdList(), "\n"))
		}
	})
	h := newHarness(t, time.Hour, nil)
	ctx := context.Background()
	c := h.dial()
	insp, err := c.StartVM(ctx, StartRequest{
		Spec:        spec(),
		BootID:      "boot-spin",
		HMACToken:   "token-spin",
		ImageDigest: "sha256:abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	lease := Lease{
		PodUID:                insp.PodUID,
		PodName:               insp.PodName,
		Namespace:             insp.Namespace,
		BootID:                insp.BootID,
		ImageDigest:           insp.ImageDigest,
		SupervisorPID:         insp.PID,
		SupervisorStartTimeNS: insp.StartTimeNS,
		HMACToken:             insp.HMACToken,
	}
	if err := AnnotateProcess(&lease); err != nil {
		t.Fatal(err)
	}
	if err := WriteLease(h.dir, 0, lease); err != nil {
		t.Fatal(err)
	}
	sess, err := Adopt(ctx, AdoptRequest{
		Socket:        SocketPath(h.dir, 0),
		BinaryVersion: "agent-spin",
		Slot:          0,
		Lease:         lease,
		Pod: PodPresence{
			Namespace: insp.Namespace,
			Name:      insp.PodName,
			UID:       insp.PodUID,
			Found:     true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	netPeer, peer, err := h.rt.ChannelPeers(spec().ID.UID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = netPeer.Close()
		_ = peer.Close()
	})
	if _, err := peer.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}

	var usage unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &usage); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	used, err := cpuSince(usage)
	if err != nil {
		t.Fatal(err)
	}
	if used > 50*time.Millisecond {
		t.Fatalf("death watch used %s of CPU while a guest byte was queued", used)
	}
	if sess.State() != SlotRunning || !sess.Held() {
		t.Fatalf("queued guest data marked the slot %s held %v", sess.State(), sess.Held())
	}

	h.cancel()
	wait, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	if err := sess.WaitLost(wait); err != nil {
		t.Fatalf("supervisor stop was not visible after guest data: %v (state %s)", err, sess.State())
	}
	if time.Since(started) > 2*time.Second {
		t.Fatalf("supervisor stop took %s", time.Since(started))
	}
	if sess.State() != SlotLost || sess.Status().State != types.VMFailed {
		t.Fatalf("after stop state %s status %+v", sess.State(), sess.Status())
	}
}

func cpuSince(before unix.Rusage) (time.Duration, error) {
	var after unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &after); err != nil {
		return 0, err
	}
	used := timevalDuration(after.Utime) - timevalDuration(before.Utime) +
		timevalDuration(after.Stime) - timevalDuration(before.Stime)
	return used, nil
}

func timevalDuration(tv unix.Timeval) time.Duration {
	return time.Duration(tv.Nano())
}
