//go:build darwin && arm64

// Package vz wraps Virtualization.framework via Code-Hex/vz.
// Device attach order (bootloader, graphics, NAT/bridged, virtio-fs,
// keyboard/pointing) is derived from Agoda macOS-vz-kubelet pkg/vm/config
// (Apache-2.0) and the Code-Hex/vz macOS examples (MIT).
package vz

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/Code-Hex/vz/v3"
	"github.com/darwin-node/darwin-node/internal/netutil"
	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/runtime"
	"github.com/darwin-node/darwin-node/pkg/types"
)

// Runtime is the production Virtualization.framework backend.
type Runtime struct {
	Opts runtime.Options
}

func New(opts runtime.Options) *Runtime { return &Runtime{Opts: opts} }

func (r *Runtime) Name() types.RuntimeName { return types.RuntimeVZ }

func (r *Runtime) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
	if spec.CPU == 0 {
		spec.CPU = 2
	}
	if spec.MemoryBytes == 0 {
		spec.MemoryBytes = 4 << 30
	}
	var err error
	if spec.CPU, err = clampCPU(spec.CPU); err != nil {
		return nil, err
	}
	if spec.MemoryBytes, err = clampMemory(spec.MemoryBytes); err != nil {
		return nil, err
	}
	cfg, err := buildConfig(spec, r.Opts)
	if err != nil {
		return nil, err
	}
	ok, err := cfg.Validate()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("invalid virtual machine configuration")
	}
	vm, err := vz.NewVirtualMachine(cfg)
	if err != nil {
		return nil, err
	}
	return &machine{id: spec.ID, spec: spec, vm: vm, token: spec.AgentToken}, nil
}

type machine struct {
	id    types.MachineID
	spec  types.VMSpec
	vm    *vz.VirtualMachine
	token string

	started  *time.Time
	finished *time.Time
	ip       string
	listener net.Listener
}

func (m *machine) ID() types.MachineID { return m.id }

func (m *machine) Start(ctx context.Context) error {
	if devs := m.vm.SocketDevices(); len(devs) > 0 {
		if ln, err := devs[0].Listen(uint32(types.GuestVsockPort)); err == nil {
			m.listener = ln
		}
	}
	if err := m.vm.Start(); err != nil {
		return err
	}
	now := time.Now()
	m.started = &now
	return nil
}

func (m *machine) Stop(ctx context.Context, graceful time.Duration) error {
	_ = ctx
	if graceful > 0 {
		_, _ = m.vm.RequestStop()
		t := time.NewTimer(graceful)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
		}
	}
	if m.vm.State() != vz.VirtualMachineStateStopped {
		if err := m.vm.Stop(); err != nil {
			return err
		}
	}
	if m.listener != nil {
		_ = m.listener.Close()
		m.listener = nil
	}
	now := time.Now()
	m.finished = &now
	return nil
}

func (m *machine) Status() types.VMStatus {
	st := types.VMStatus{IP: m.ip, MAC: m.spec.MAC, StartedAt: m.started, FinishedAt: m.finished, Transport: types.TransportVsock}
	if m.vm == nil {
		st.State = types.VMPending
		return st
	}
	switch m.vm.State() {
	case vz.VirtualMachineStateStarting:
		st.State = types.VMStarting
	case vz.VirtualMachineStateRunning:
		st.State = types.VMRunning
	case vz.VirtualMachineStateStopping:
		st.State = types.VMStopping
	case vz.VirtualMachineStateStopped:
		st.State = types.VMStopped
	default:
		st.State = types.VMFailed
	}
	return st
}

func (m *machine) DialAgent(ctx context.Context) (*guest.Client, error) {
	dialCtx, cancel := agentDialContext(ctx)
	defer cancel()
	if m.listener != nil {
		conn, err := acceptAgent(dialCtx, m.listener)
		if err == nil {
			return guest.Dial(dialCtx, conn, m.token, "darwin-node")
		}
		if dialCtx.Err() != nil {
			m.listener = nil
		}
	}
	if ip := m.ip; ip == "" {
		if found, err := netutil.IPForMAC(m.spec.MAC); err == nil {
			m.ip = found
			ip = found
		}
	}
	if m.ip != "" {
		d := net.Dialer{}
		addr := net.JoinHostPort(m.ip, fmt.Sprintf("%d", types.GuestTCPPort))
		if conn, err := d.DialContext(dialCtx, "tcp", addr); err == nil {
			return guest.Dial(dialCtx, conn, m.token, "darwin-node")
		}
	}
	return nil, fmt.Errorf("guest agent unreachable over vsock or tcp")
}

func (m *machine) Logs() io.ReadCloser {
	r, w := io.Pipe()
	go func() {
		defer w.Close()
		_, _ = io.WriteString(w, "vz runtime: use guest agent for logs\n")
	}()
	return r
}

func buildConfig(spec types.VMSpec, opts runtime.Options) (*vz.VirtualMachineConfiguration, error) {
	boot, err := vz.NewMacOSBootLoader()
	if err != nil {
		return nil, err
	}
	cfg, err := vz.NewVirtualMachineConfiguration(boot, spec.CPU, spec.MemoryBytes)
	if err != nil {
		return nil, err
	}

	if err := attachGraphics(cfg, spec.Graphics); err != nil {
		return nil, err
	}
	if err := attachNetwork(cfg, spec); err != nil {
		return nil, err
	}
	if err := attachSocket(cfg); err != nil {
		return nil, err
	}
	if err := attachShares(cfg, spec.Shares); err != nil {
		return nil, err
	}
	if err := attachPlatformAndDisk(cfg, spec); err != nil {
		return nil, err
	}
	_ = opts
	_ = os.DevNull
	return cfg, nil
}

func attachGraphics(cfg *vz.VirtualMachineConfiguration, g types.Graphics) error {
	if !g.Enabled {
		return nil
	}
	if g.Width == 0 {
		g = types.DefaultGraphics()
	}
	dev, err := vz.NewMacGraphicsDeviceConfiguration()
	if err != nil {
		return err
	}
	disp, err := vz.NewMacGraphicsDisplayConfiguration(int64(g.Width), int64(g.Height), int64(g.PPI))
	if err != nil {
		return err
	}
	dev.SetDisplays(disp)
	cfg.SetGraphicsDevicesVirtualMachineConfiguration([]vz.GraphicsDeviceConfiguration{dev})
	return nil
}

func attachNetwork(cfg *vz.VirtualMachineConfiguration, spec types.VMSpec) error {
	var attachment vz.NetworkDeviceAttachment
	var err error
	if spec.NetworkMode == types.NetworkBridged && spec.BridgeDevice != "" {
		var found vz.BridgedNetwork
		for _, b := range vz.NetworkInterfaces() {
			if b.Identifier() == spec.BridgeDevice {
				found = b
				break
			}
		}
		if found == nil {
			return fmt.Errorf("bridge interface %s not found", spec.BridgeDevice)
		}
		attachment, err = vz.NewBridgedNetworkDeviceAttachment(found)
	} else {
		attachment, err = vz.NewNATNetworkDeviceAttachment()
	}
	if err != nil {
		return err
	}
	netDev, err := vz.NewVirtioNetworkDeviceConfiguration(attachment)
	if err != nil {
		return err
	}
	if spec.MAC != "" {
		hw, err := parseHardware(spec.MAC)
		if err == nil && hw != nil {
			mac, mErr := vz.NewMACAddress(hw)
			if mErr == nil {
				netDev.SetMACAddress(mac)
			}
		}
	}
	cfg.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{netDev})
	return nil
}

func attachSocket(cfg *vz.VirtualMachineConfiguration) error {
	sock, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return err
	}
	cfg.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{sock})
	return nil
}

func attachShares(cfg *vz.VirtualMachineConfiguration, shares []types.Share) error {
	if len(shares) == 0 {
		return nil
	}
	tag, err := vz.MacOSGuestAutomountTag()
	if err != nil {
		return err
	}
	dirs := make(map[string]*vz.SharedDirectory, len(shares))
	for _, s := range shares {
		d, err := vz.NewSharedDirectory(s.HostPath, s.ReadOnly)
		if err != nil {
			return err
		}
		dirs[s.Name] = d
	}
	share, err := vz.NewMultipleDirectoryShare(dirs)
	if err != nil {
		return err
	}
	fs, err := vz.NewVirtioFileSystemDeviceConfiguration(tag)
	if err != nil {
		return err
	}
	fs.SetDirectoryShare(share)
	cfg.SetDirectorySharingDevicesVirtualMachineConfiguration([]vz.DirectorySharingDeviceConfiguration{fs})
	return nil
}
