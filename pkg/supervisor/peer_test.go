//go:build linux || darwin

package supervisor

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPeerUIDOfAcceptedConn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	got := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			got <- err
			return
		}
		defer func() { _ = c.Close() }()
		uid, err := peerUID(c.(*net.UnixConn))
		if err != nil {
			got <- err
			return
		}
		if uid != os.Getuid() {
			got <- errors.New("uid mismatch")
			return
		}
		got <- nil
	}()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := <-got; err != nil {
		t.Fatal(err)
	}
}

func TestPeerCredENOTCONN(t *testing.T) {
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := os.NewFile(uintptr(fd), "unconnected")
	if f == nil {
		_ = unix.Close(fd)
		t.Fatal("NewFile")
	}
	defer func() { _ = f.Close() }()
	nc, err := net.FileConn(f)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = nc.Close() }()
	_, err = peerUID(nc.(*net.UnixConn))
	if !errors.Is(err, errPeerGone) {
		t.Fatalf("peerUID %v, want ENOTCONN tolerated", err)
	}
}
