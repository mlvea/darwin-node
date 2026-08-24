// Package hostport is a userspace proxy from host Port to guest PodIP:containerPort.
package hostport

import (
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
)

// Mapping is one kubelet-style hostPort.
type Mapping struct {
	HostIP        string
	HostPort      int
	PodIP         string
	ContainerPort int
	Protocol      string // tcp
}

// Manager tracks bindings.
type proxy struct {
	ln  net.Listener
	dst atomic.Value // string host:port
}

type Manager struct {
	mu       sync.Mutex
	proxies  map[string]*proxy // key host:port
	reserved map[string]string // key -> pod
}

func New() *Manager {
	return &Manager{proxies: map[string]*proxy{}, reserved: map[string]string{}}
}

func key(host string, port int) string {
	if host == "" {
		host = "0.0.0.0"
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// Reserve fails closed on conflict. Actual listen happens in Bind.
func (m *Manager) Reserve(pod string, maps []Mapping) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mp := range maps {
		if err := checkTCP(mp); err != nil {
			return err
		}
		k := key(mp.HostIP, mp.HostPort)
		if owner, ok := m.reserved[k]; ok && owner != pod {
			return fmt.Errorf("hostPort %s already used by %s", k, owner)
		}
	}
	for _, mp := range maps {
		m.reserved[key(mp.HostIP, mp.HostPort)] = pod
	}
	return nil
}

func (m *Manager) Release(pod string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, owner := range m.reserved {
		if owner == pod {
			delete(m.reserved, k)
			if p, ok := m.proxies[k]; ok {
				_ = p.ln.Close()
				delete(m.proxies, k)
			}
		}
	}
}

func (m *Manager) Owners() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for k, v := range m.reserved {
		out[k] = v
	}
	return out
}

func checkTCP(mp Mapping) error {
	proto := strings.ToUpper(mp.Protocol)
	if proto == "" {
		proto = "TCP"
	}
	if proto != "TCP" {
		return fmt.Errorf("hostPort protocol %s is not supported", mp.Protocol)
	}
	return nil
}

// Bind listens on each mapping and splices TCP to PodIP:containerPort.
// Reservation must already have succeeded at admission; Bind does not Reserve.
func (m *Manager) Bind(pod string, maps []Mapping) error {
	for _, mp := range maps {
		if err := checkTCP(mp); err != nil {
			return err
		}
		addr := key(mp.HostIP, mp.HostPort)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			m.Release(pod)
			return fmt.Errorf("hostPort listen %s: %w", addr, err)
		}
		p := &proxy{ln: ln}
		p.dst.Store(net.JoinHostPort(mp.PodIP, fmt.Sprintf("%d", mp.ContainerPort)))
		m.mu.Lock()
		m.proxies[addr] = p
		m.mu.Unlock()
		go acceptLoop(p)
	}
	return nil
}

// UpdateDest retargets existing listeners for pod after a VM reboot/IP change.
func (m *Manager) UpdateDest(pod string, maps []Mapping) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mp := range maps {
		k := key(mp.HostIP, mp.HostPort)
		if owner := m.reserved[k]; owner != pod {
			continue
		}
		if p, ok := m.proxies[k]; ok {
			p.dst.Store(net.JoinHostPort(mp.PodIP, fmt.Sprintf("%d", mp.ContainerPort)))
		}
	}
}

func acceptLoop(p *proxy) {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		dst, _ := p.dst.Load().(string)
		go splice(c, dst)
	}
}

func splice(c net.Conn, dst string) {
	defer c.Close()
	r, err := net.Dial("tcp", dst)
	if err != nil {
		return
	}
	defer r.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(r, c) }()
	go func() { defer wg.Done(); _, _ = io.Copy(c, r) }()
	wg.Wait()
}
