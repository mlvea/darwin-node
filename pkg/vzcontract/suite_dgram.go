package vzcontract

import (
	"errors"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

const (
	dgramSndGoal = 256 * 1024
	dgramRcvGoal = 1024 * 1024
)

func testDgram(t *testing.T, factory Factory) {
	t.Run("buffers_and_enobufs", func(t *testing.T) {
		prepare(t)
		rt := factory(t, Config{DatagramENOBUFS: 3})
		vm := mustCreate(t, rt, specNamed("nic"))
		mustStart(t, vm)
		host := vm.HostNIC()
		if host == nil {
			t.Fatal("no host datagram fd")
		}
		baseSnd, baseRcv := controlDgramBufs(t)
		gotSnd, gotRcv := sockBufs(t, int(host.Fd()))
		// The kernel may clamp a raise and report twice the clamped
		// value. Either the attached socket is strictly larger than an
		// unconfigured one, or it reached the goal.
		if gotSnd <= baseSnd && gotSnd < dgramSndGoal {
			t.Fatalf("SO_SNDBUF %d, control %d, goal %d", gotSnd, baseSnd, dgramSndGoal)
		}
		if gotRcv <= baseRcv && gotRcv < dgramRcvGoal {
			t.Fatalf("SO_RCVBUF %d, control %d, goal %d", gotRcv, baseRcv, dgramRcvGoal)
		}
		for i := 0; i < 3; i++ {
			err := vm.WriteDatagram([]byte("x"))
			if !errors.Is(err, syscall.ENOBUFS) {
				t.Fatalf("injected write %d: %v", i, err)
			}
		}
		if err := vm.WriteDatagram([]byte("x")); err != nil {
			t.Fatalf("write after burst: %v", err)
		}
	})

	t.Run("full_buffer", func(t *testing.T) {
		prepare(t)
		rt := factory(t, Config{})
		vm := mustCreate(t, rt, specNamed("full"))
		mustStart(t, vm)
		host := vm.HostNIC()
		if host == nil {
			t.Fatal("no host datagram fd")
		}
		fd := int(host.Fd())
		if err := unix.SetNonblock(fd, true); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = unix.SetNonblock(fd, false) })
		buf := make([]byte, 1024)
		wrote := 0
		var werr error
		for i := 0; i < 8192; i++ {
			n, err := unix.Write(fd, buf)
			if n > 0 {
				wrote += n
			}
			if err != nil {
				werr = err
				break
			}
		}
		if wrote == 0 || wrote > 8<<20 || werr == nil {
			t.Fatalf("dgram fill wrote %d err %v", wrote, werr)
		}
		if !errors.Is(werr, syscall.EAGAIN) && !errors.Is(werr, syscall.EWOULDBLOCK) && !errors.Is(werr, syscall.ENOBUFS) {
			t.Fatalf("full dgram: %v", werr)
		}
	})
}

func controlDgramBufs(t *testing.T) (snd, rcv int) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
	})
	return sockBufs(t, fds[0])
}

func sockBufs(t *testing.T, fd int) (snd, rcv int) {
	t.Helper()
	snd, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF)
	if err != nil {
		t.Fatal(err)
	}
	rcv, err = unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF)
	if err != nil {
		t.Fatal(err)
	}
	return snd, rcv
}
