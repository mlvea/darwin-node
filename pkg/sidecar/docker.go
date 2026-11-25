package sidecar

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	dockercl "github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
)

// dockerAPI is the daemon surface Docker uses. *dockercl.Client implements it.
// Tests substitute a script so a restart can be proved without a daemon.
type dockerAPI interface {
	ImagePull(ctx context.Context, ref string, options image.PullOptions) (io.ReadCloser, error)
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, containerID string, options container.StartOptions) error
	ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error
	ContainerInspect(ctx context.Context, containerID string) (types.ContainerJSON, error)
	ContainerList(ctx context.Context, options container.ListOptions) ([]types.Container, error)
	ContainerLogs(ctx context.Context, container string, options container.LogsOptions) (io.ReadCloser, error)
	ContainerExecCreate(ctx context.Context, container string, config types.ExecConfig) (types.IDResponse, error)
	ContainerExecAttach(ctx context.Context, execID string, config types.ExecStartCheck) (types.HijackedResponse, error)
	ContainerExecInspect(ctx context.Context, execID string) (types.ContainerExecInspect, error)
	ContainerKill(ctx context.Context, containerID, signal string) error
	NetworkCreate(ctx context.Context, name string, options types.NetworkCreate) (types.NetworkCreateResponse, error)
	NetworkRemove(ctx context.Context, networkID string) error
	NetworkList(ctx context.Context, options types.NetworkListOptions) ([]types.NetworkResource, error)
}

var _ dockerAPI = (*dockercl.Client)(nil)

const namePrefix = "darwin-node"

// Docker runs hybrid sidecars on a local Docker daemon (Colima, Desktop, ...).
type Docker struct {
	cli   dockerAPI
	mu    sync.Mutex
	ids   map[string]map[string]string // podKey -> containerName -> docker ID
	nets  map[string]string            // podKey -> network name
	saved map[string]CreateOptions     // podKey -> last Create options
}

// NewDocker returns a sidecar runtime or an error if the daemon is unreachable.
func NewDocker(ctx context.Context) (*Docker, error) {
	opts := []dockercl.Opt{dockercl.FromEnv, dockercl.WithAPIVersionNegotiation()}
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		opts = append(opts, dockercl.WithHost(h))
	}
	cli, err := dockercl.NewClientWithOpts(opts...)
	if err != nil {
		return nil, err
	}
	if _, err := cli.Ping(ctx); err != nil {
		_ = cli.Close()
		return nil, err
	}
	d := &Docker{
		cli:   cli,
		ids:   map[string]map[string]string{},
		nets:  map[string]string{},
		saved: map[string]CreateOptions{},
	}
	_ = d.reapDangling(ctx)
	return d, nil
}

func dockerName(ns, pod, containerName string) string {
	return fmt.Sprintf("%s_%s_%s_%s", namePrefix, ns, pod, containerName)
}

func podKey(ns, name string) string { return ns + "/" + name }

func drainImagePull(r io.ReadCloser, err error) error {
	if err != nil {
		return err
	}
	if r == nil {
		return nil
	}
	defer r.Close()
	_, copyErr := io.Copy(io.Discard, r)
	return copyErr
}

func prepareContainer(pod *corev1.Pod, c corev1.Container, volRoot string, opts CreateOptions) (*container.Config, *container.HostConfig, error) {
	if err := ValidateContainerIsolation(pod, c); err != nil {
		return nil, nil, err
	}
	binds, err := volumeBinds(pod, c, volRoot, opts.AllowedHostPaths)
	if err != nil {
		return nil, nil, err
	}
	res, err := dockerResources(c)
	if err != nil {
		return nil, nil, err
	}
	env, err := resolveEnv(pod, c, opts)
	if err != nil {
		return nil, nil, err
	}
	cfg := &container.Config{
		Image:      c.Image,
		Env:        env,
		User:       userString(pod, c),
		WorkingDir: c.WorkingDir,
		Tty:        c.TTY,
	}
	if len(c.Command) > 0 {
		cfg.Entrypoint = c.Command
	}
	if len(c.Args) > 0 {
		cfg.Cmd = c.Args
	}
	host := &container.HostConfig{
		Binds:          binds,
		Resources:      res,
		ReadonlyRootfs: readOnlyRoot(c),
		Privileged:     false,
		SecurityOpt:    []string{"no-new-privileges:true"},
	}
	for _, cap := range droppedCaps(c) {
		host.CapDrop = append(host.CapDrop, string(cap))
	}
	return cfg, host, nil
}

func (d *Docker) remember(ctx context.Context, ns, name string) CreateOptions {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.saved == nil {
		d.saved = map[string]CreateOptions{}
	}
	key := podKey(ns, name)
	if opts, ok := CreateOptionsFrom(ctx); ok {
		d.saved[key] = opts
		return opts
	}
	return d.saved[key]
}

func (d *Docker) Create(ctx context.Context, pod *corev1.Pod, c corev1.Container, volRoot string) error {
	if pod == nil {
		return fmt.Errorf("nil pod")
	}
	opts := d.remember(ctx, pod.Namespace, pod.Name)
	cfg, host, err := prepareContainer(pod, c, volRoot, opts)
	if err != nil {
		return err
	}
	// A second Create is a restart. Start the stopped container in place so
	// the daemon does not reject the name. A start that fails, or a container
	// the daemon no longer has, is removed and created again.
	if id, ok := d.trackedID(pod.Namespace, pod.Name, c.Name); ok {
		if err := d.restartTracked(ctx, id); err == nil {
			return nil
		}
		// Keep going with the tracked id removed. A failed recreate still
		// has a container RemovePod can delete once the replacement is stored.
		_ = d.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true, RemoveVolumes: true})
	}
	netName, err := d.ensurePodNetwork(ctx, pod)
	if err != nil {
		return err
	}
	host.NetworkMode = container.NetworkMode(netName)
	netCfg := &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
		netName: {},
	}}
	if c.ImagePullPolicy != corev1.PullNever {
		auth, err := registryAuthHeader(c.Image, opts.Pull)
		if err != nil {
			return fmt.Errorf("docker pull %s: %w", c.Image, err)
		}
		rc, pullErr := d.cli.ImagePull(ctx, c.Image, image.PullOptions{RegistryAuth: auth})
		if err := drainImagePull(rc, pullErr); err != nil {
			return fmt.Errorf("docker pull %s: %w", c.Image, err)
		}
	}
	name := dockerName(pod.Namespace, pod.Name, c.Name)
	res, err := d.cli.ContainerCreate(ctx, cfg, host, netCfg, nil, name)
	if err != nil {
		return fmt.Errorf("docker create %s: %w", c.Name, err)
	}
	if err := d.cli.ContainerStart(ctx, res.ID, container.StartOptions{}); err != nil {
		_ = d.cli.ContainerRemove(ctx, res.ID, container.RemoveOptions{Force: true, RemoveVolumes: true})
		return fmt.Errorf("docker start %s: %w", c.Name, err)
	}
	d.mu.Lock()
	if d.ids[podKey(pod.Namespace, pod.Name)] == nil {
		d.ids[podKey(pod.Namespace, pod.Name)] = map[string]string{}
	}
	d.ids[podKey(pod.Namespace, pod.Name)][c.Name] = res.ID
	d.mu.Unlock()
	return nil
}

func (d *Docker) ensurePodNetwork(ctx context.Context, pod *corev1.Pod) (string, error) {
	key := podKey(pod.Namespace, pod.Name)
	d.mu.Lock()
	if d.nets == nil {
		d.nets = map[string]string{}
	}
	if name := d.nets[key]; name != "" {
		d.mu.Unlock()
		return name, nil
	}
	d.mu.Unlock()
	name := podNetworkName(pod)
	_, err := d.cli.NetworkCreate(ctx, name, types.NetworkCreate{
		Driver: "bridge",
		Labels: map[string]string{networkLabel: networkLabelPod},
	})
	if err != nil && !networkExists(err) {
		return "", fmt.Errorf("docker network %s: %w", name, err)
	}
	d.mu.Lock()
	if cur := d.nets[key]; cur != "" {
		d.mu.Unlock()
		return cur, nil
	}
	d.nets[key] = name
	d.mu.Unlock()
	return name, nil
}

func networkExists(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "already exists")
}

func (d *Docker) trackedID(ns, name, containerName string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	id := d.ids[podKey(ns, name)][containerName]
	return id, id != ""
}

func (d *Docker) restartTracked(ctx context.Context, id string) error {
	ins, err := d.cli.ContainerInspect(ctx, id)
	if err != nil {
		return err
	}
	if ins.State != nil && ins.State.Running {
		return nil
	}
	return d.cli.ContainerStart(ctx, id, container.StartOptions{})
}

// StopContainer kills one sidecar. The daemon keeps the container so the
// next List reports its exit code and Create can start it again.
func (d *Docker) StopContainer(ctx context.Context, ns, name, containerName string) error {
	id, err := d.lookup(ns, name, containerName)
	if err != nil {
		return err
	}
	if err := d.cli.ContainerKill(ctx, id, "SIGKILL"); err != nil {
		return fmt.Errorf("docker kill %s: %w", containerName, err)
	}
	return nil
}

func (d *Docker) RemovePod(ctx context.Context, ns, name string, _ int64) error {
	key := podKey(ns, name)
	d.mu.Lock()
	ids := copyContainerIDs(d.ids[key])
	delete(d.ids, key)
	netName := ""
	if d.nets != nil {
		netName = d.nets[key]
		delete(d.nets, key)
	}
	if d.saved != nil {
		delete(d.saved, key)
	}
	d.mu.Unlock()
	var first error
	for _, id := range ids {
		if err := d.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true, RemoveVolumes: true}); err != nil && first == nil {
			first = err
		}
	}
	if netName != "" {
		if err := d.cli.NetworkRemove(ctx, netName); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (d *Docker) Get(ctx context.Context, ns, name, containerName string) (Status, error) {
	id, err := d.lookup(ns, name, containerName)
	if err != nil {
		return Status{}, err
	}
	ins, err := d.cli.ContainerInspect(ctx, id)
	if err != nil {
		return Status{}, err
	}
	return inspectJSON(containerName, ins), nil
}

// Endpoint is the container address on its pod network. Loopback and
// link-local inspect results are refused so a probe cannot dial the node.
func (d *Docker) Endpoint(ctx context.Context, ns, name, containerName string) (string, error) {
	id, err := d.lookup(ns, name, containerName)
	if err != nil {
		return "", err
	}
	ins, err := d.cli.ContainerInspect(ctx, id)
	if err != nil {
		return "", err
	}
	d.mu.Lock()
	netName := ""
	if d.nets != nil {
		netName = d.nets[podKey(ns, name)]
	}
	d.mu.Unlock()
	ip := containerIP(ins.NetworkSettings, netName)
	if ip == "" {
		return "", fmt.Errorf("sidecar %s has no routable address", containerName)
	}
	return ip, nil
}

// copyContainerIDs detaches the id map so List and RemovePod can inspect
// containers without holding Docker.mu. Create writes that map.
func copyContainerIDs(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func (d *Docker) List(ctx context.Context, ns, name string) ([]Status, error) {
	d.mu.Lock()
	ids := copyContainerIDs(d.ids[podKey(ns, name)])
	d.mu.Unlock()
	var out []Status
	for cname, id := range ids {
		ins, err := d.cli.ContainerInspect(ctx, id)
		if err != nil {
			out = append(out, Status{Name: cname, State: "unknown", Error: err.Error()})
			continue
		}
		out = append(out, inspectJSON(cname, ins))
	}
	return out, nil
}

func (d *Docker) Logs(ctx context.Context, ns, name, containerName string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	id, err := d.lookup(ns, name, containerName)
	if err != nil {
		return nil, err
	}
	tail := "all"
	if opts.Tail > 0 {
		tail = strconv.Itoa(opts.Tail)
	}
	return d.cli.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     opts.Follow,
		Tail:       tail,
		Timestamps: opts.Timestamps,
	})
}

func (d *Docker) Exec(ctx context.Context, ns, name, containerName string, cmd []string, attach api.AttachIO) error {
	id, err := d.lookup(ns, name, containerName)
	if err != nil {
		return err
	}
	execID, err := d.cli.ContainerExecCreate(ctx, id, types.ExecConfig{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
		AttachStdin:  attach != nil && attach.Stdin() != nil,
		Tty:          attach != nil && attach.TTY(),
	})
	if err != nil {
		return err
	}
	hr, err := d.cli.ContainerExecAttach(ctx, execID.ID, types.ExecStartCheck{Tty: attach != nil && attach.TTY()})
	if err != nil {
		return err
	}
	var once sync.Once
	closeStream := func() {
		once.Do(func() {
			if hr.Conn != nil {
				hr.Close()
			}
		})
	}
	done := make(chan struct{})
	var watch sync.WaitGroup
	watch.Add(1)
	go func() {
		defer watch.Done()
		select {
		case <-ctx.Done():
			closeStream()
		case <-done:
		}
	}()
	defer func() {
		close(done)
		watch.Wait()
		closeStream()
	}()

	var stdinWG sync.WaitGroup
	if attach != nil && attach.Stdin() != nil && hr.Conn != nil {
		stdinWG.Add(1)
		go func() {
			defer stdinWG.Done()
			_, _ = io.Copy(hr.Conn, attach.Stdin())
			if ctx.Err() == nil {
				_ = hr.CloseWrite()
			}
		}()
	}

	var copyErr error
	if hr.Reader != nil {
		dst := io.Writer(io.Discard)
		if attach != nil && attach.Stdout() != nil {
			dst = attach.Stdout()
		}
		_, copyErr = io.Copy(dst, hr.Reader)
	}
	if ctx.Err() != nil {
		closeStream()
		stdinWG.Wait()
		return ctx.Err()
	}
	stdinWG.Wait()

	ins, err := d.cli.ContainerExecInspect(ctx, execID.ID)
	if err != nil {
		return fmt.Errorf("exec inspect: %w", err)
	}
	if ins.Running {
		return fmt.Errorf("exec still running")
	}
	if ins.ExitCode != 0 {
		return fmt.Errorf("exit code %d", ins.ExitCode)
	}
	return copyErr
}

func (d *Docker) lookup(ns, name, containerName string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	id := d.ids[podKey(ns, name)][containerName]
	if id == "" {
		return "", fmt.Errorf("sidecar %s not found", containerName)
	}
	return id, nil
}

func (d *Docker) reapDangling(ctx context.Context) error {
	list, err := d.cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return err
	}
	for _, c := range list {
		for _, n := range c.Names {
			if strings.HasPrefix(strings.TrimPrefix(n, "/"), namePrefix+"_") {
				_ = d.cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true, RemoveVolumes: true})
			}
		}
	}
	nets, err := d.cli.NetworkList(ctx, types.NetworkListOptions{})
	if err != nil {
		return err
	}
	for _, n := range nets {
		if !strings.HasPrefix(n.Name, namePrefix+"-") {
			continue
		}
		if n.Labels[networkLabel] != networkLabelPod {
			continue
		}
		_ = d.cli.NetworkRemove(ctx, n.ID)
	}
	return nil
}

func inspectJSON(name string, ins types.ContainerJSON) Status {
	st := Status{Name: name, ID: ins.ID}
	if ins.State == nil {
		st.State = "unknown"
		return st
	}
	if ins.State.Running {
		st.State = "running"
	} else {
		st.State = "terminated"
		st.ExitCode = ins.State.ExitCode
		st.Error = ins.State.Error
	}
	if t, err := time.Parse(time.RFC3339Nano, ins.State.StartedAt); err == nil {
		st.StartedAt = t
	}
	return st
}
