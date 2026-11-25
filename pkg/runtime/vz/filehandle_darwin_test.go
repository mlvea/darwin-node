//go:build darwin && arm64

package vz

import (
	"net"
	"os"
	"reflect"
	"testing"
	"unsafe"

	vzapi "github.com/Code-Hex/vz/v3"
	"golang.org/x/sys/unix"
)

func TestVirtioSocketConnectionRawConnField(t *testing.T) {
	f, ok := reflect.TypeOf(vzapi.VirtioSocketConnection{}).FieldByName("rawConn")
	if !ok {
		t.Fatal("VirtioSocketConnection.rawConn is gone")
	}
	want := reflect.TypeOf((*net.Conn)(nil)).Elem()
	if f.Type != want {
		t.Fatalf("rawConn type %s, want %s", f.Type, want)
	}
}

func TestFileFromVirtioRejectsEmptyConn(t *testing.T) {
	if _, err := fileFromVirtio(nil); err == nil {
		t.Fatal("nil conn")
	}
	if _, err := fileFromVirtio(&vzapi.VirtioSocketConnection{}); err == nil {
		t.Fatal("zero conn")
	}
}

func TestFileFromVirtioDupsTheSocket(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fds[1]) }()
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	local := os.NewFile(uintptr(fds[0]), "vsock-local")
	if local == nil {
		_ = unix.Close(fds[0])
		t.Fatal("NewFile")
	}
	nc, err := net.FileConn(local)
	_ = local.Close()
	if err != nil {
		t.Fatal(err)
	}
	conn := &vzapi.VirtioSocketConnection{}
	setUnexported(t, conn, "rawConn", nc)
	got, err := fileFromVirtio(conn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = got.Close() }()
	if _, err := got.Write([]byte("vsock-fd")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	n, err := unix.Read(fds[1], buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "vsock-fd" {
		t.Fatalf("peer read %q", buf[:n])
	}
}

func setUnexported(t *testing.T, target any, name string, value any) {
	t.Helper()
	field := reflect.ValueOf(target).Elem().FieldByName(name)
	if !field.IsValid() || !field.CanAddr() {
		t.Fatalf("field %s", name)
	}
	dst := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem()
	src := reflect.ValueOf(value)
	if !src.Type().AssignableTo(dst.Type()) {
		t.Fatalf("%s is not assignable to %s", src.Type(), dst.Type())
	}
	dst.Set(src)
}
