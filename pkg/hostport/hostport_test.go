package hostport

import (
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

func TestConflict(t *testing.T) {
	m := New()
	if err := m.Reserve("a", []Mapping{{HostPort: 8080, ContainerPort: 80}}); err != nil {
		t.Fatal(err)
	}
	if err := m.Reserve("b", []Mapping{{HostPort: 8080, ContainerPort: 80}}); err == nil {
		t.Fatal("expected conflict")
	}
	m.Release("a")
	if err := m.Reserve("b", []Mapping{{HostPort: 8080, ContainerPort: 80}}); err != nil {
		t.Fatal(err)
	}
}

func TestReserveRejectsUDP(t *testing.T) {
	m := New()
	if err := m.Reserve("a", []Mapping{{HostPort: 8080, ContainerPort: 80, Protocol: "UDP"}}); err == nil {
		t.Fatal("expected UDP error")
	}
}

func TestBindTCPAndRejectUDP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, c)
	}()
	t.Cleanup(func() { _ = ln.Close() })

	hostLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hostPort := hostLn.Addr().(*net.TCPAddr).Port
	_ = hostLn.Close()

	m := New()
	maps := []Mapping{{HostIP: "127.0.0.1", HostPort: hostPort, PodIP: "127.0.0.1", ContainerPort: port, Protocol: "TCP"}}
	if err := m.Reserve("p", maps); err != nil {
		t.Fatal(err)
	}
	if err := m.Bind("p", maps); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Release("p") })

	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(hostPort)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2)
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	n, err := c.Read(buf)
	if err != nil || string(buf[:n]) != "hi" {
		t.Fatalf("echo %q %v", buf[:n], err)
	}

	if err := m.Bind("q", []Mapping{{HostPort: hostPort + 1, ContainerPort: 9, Protocol: "UDP"}}); err == nil {
		t.Fatal("expected UDP bind error")
	}
}

func TestUpdateDestRetargets(t *testing.T) {
	echo := func() (int, net.Listener) {
		t.Helper()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func(c net.Conn) {
					defer c.Close()
					_, _ = io.Copy(c, c)
				}(c)
			}
		}()
		return ln.Addr().(*net.TCPAddr).Port, ln
	}
	p1, _ := echo()
	p2, _ := echo()

	hostLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hostPort := hostLn.Addr().(*net.TCPAddr).Port
	_ = hostLn.Close()

	m := New()
	maps := []Mapping{{HostIP: "127.0.0.1", HostPort: hostPort, PodIP: "127.0.0.1", ContainerPort: p1, Protocol: "TCP"}}
	if err := m.Reserve("p", maps); err != nil {
		t.Fatal(err)
	}
	if err := m.Bind("p", maps); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Release("p") })

	dial := func() {
		t.Helper()
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(hostPort)), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(time.Second))
		if _, err := c.Write([]byte("ok")); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 2)
		if n, err := c.Read(buf); err != nil || string(buf[:n]) != "ok" {
			t.Fatalf("echo %q %v", buf[:n], err)
		}
	}
	dial()
	m.UpdateDest("p", []Mapping{{HostIP: "127.0.0.1", HostPort: hostPort, PodIP: "127.0.0.1", ContainerPort: p2, Protocol: "TCP"}})
	dial()
}
