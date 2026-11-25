package vzcontract

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func isTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func testVsock(t *testing.T, factory Factory) {
	t.Run("pause_save_stop_restore", func(t *testing.T) {
		prepare(t)
		rt := factory(t, Config{})
		spec := specNamed("vs")
		vm := mustCreate(t, rt, spec)
		mustStart(t, vm)
		conn, err := vm.Connect(bg())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if conn != nil {
				_ = conn.Close()
			}
		})
		if _, err := conn.Write([]byte("hi")); err != nil {
			t.Fatalf("running write: %v", err)
		}
		if err := vm.Pause(bg()); err != nil {
			t.Fatal(err)
		}
		if vm.State() != "Paused" || !vm.QuotaHeld() {
			t.Fatalf("paused %s held %v", vm.State(), vm.QuotaHeld())
		}
		// The fd stays open. A short write fits in the buffer.
		_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
		if _, err := conn.Write([]byte("x")); err != nil {
			t.Fatalf("paused write: %v", err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		_, err = conn.Read(make([]byte, 8))
		if err == nil || errors.Is(err, io.EOF) || !isTimeout(err) {
			t.Fatalf("paused read: %v", err)
		}
		_ = conn.SetWriteDeadline(time.Now().Add(250 * time.Millisecond))
		wrote, werr := fillUntilErr(conn)
		if wrote == 0 || werr == nil || !isTimeout(werr) {
			t.Fatalf("paused fill wrote %d err %v", wrote, werr)
		}

		path := filepath.Join(t.TempDir(), "vm.json")
		if err := vm.Save(bg(), path); err != nil {
			t.Fatal(err)
		}
		if vm.State() != "Paused" || !vm.QuotaHeld() || rt.Running() != 1 {
			t.Fatalf("after save %s held %v running %d", vm.State(), vm.QuotaHeld(), rt.Running())
		}
		_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		_, err = conn.Read(make([]byte, 8))
		if err == nil || errors.Is(err, io.EOF) || !isTimeout(err) {
			t.Fatalf("read across save: %v", err)
		}

		if err := vm.Stop(bg()); err != nil {
			t.Fatal(err)
		}
		assertConnDead(t, conn)

		vm2 := mustCreate(t, rt, spec)
		if err := vm2.Restore(bg(), path); err != nil {
			t.Fatal(err)
		}
		if vm2.State() != "Paused" || !vm2.QuotaHeld() {
			t.Fatalf("restored %s held %v", vm2.State(), vm2.QuotaHeld())
		}
		conn2, err := vm2.Connect(bg())
		if err != nil {
			t.Fatalf("fresh connect: %v", err)
		}
		t.Cleanup(func() { _ = conn2.Close() })
		_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
		if _, err := conn.Write([]byte{2}); err == nil {
			t.Fatal("old connection survived restore")
		}
	})

	t.Run("crash", func(t *testing.T) {
		prepare(t)
		rt := factory(t, Config{})
		vm := mustCreate(t, rt, specNamed("crash"))
		mustStart(t, vm)
		conn, err := vm.Connect(bg())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		if _, err := conn.Write([]byte("hi")); err != nil {
			t.Fatal(err)
		}
		if err := vm.Crash(); err != nil {
			t.Fatal(err)
		}
		if vm.State() != "Running" || !vm.QuotaHeld() || rt.Running() != 1 {
			t.Fatalf("crash state %s held %v running %d", vm.State(), vm.QuotaHeld(), rt.Running())
		}
		assertConnDead(t, conn)
		if _, err := vm.Connect(bg()); !errors.Is(err, syscall.ECONNREFUSED) {
			t.Fatalf("connect after crash: %v", err)
		}
		if err := vm.RegisterAgent(bg()); err != nil {
			t.Fatal(err)
		}
		fresh, err := vm.Connect(bg())
		if err != nil {
			t.Fatalf("fresh connect after crash: %v", err)
		}
		_ = fresh.Close()
	})
}

func fillUntilErr(conn net.Conn) (int, error) {
	buf := make([]byte, 1024)
	wrote := 0
	for i := 0; i < 8192; i++ {
		n, err := conn.Write(buf)
		wrote += n
		if err != nil {
			return wrote, err
		}
	}
	return wrote, nil
}

func assertConnDead(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, err := conn.Read(make([]byte, 8))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("read after death: %v", err)
	}
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	_, err = conn.Write([]byte{1})
	if err == nil || (!errors.Is(err, syscall.EPIPE) && !errors.Is(err, syscall.ECONNRESET) && !errors.Is(err, io.ErrClosedPipe)) {
		t.Fatalf("write after death: %v", err)
	}
}
