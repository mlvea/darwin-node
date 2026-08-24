package sidecar

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	dockercl "github.com/moby/moby/client"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
)

const namePrefix = "darwin-node"

// Docker runs hybrid sidecars on a local Docker daemon (Colima, Desktop, ...).
type Docker struct {
	cli *dockercl.Client
	mu  sync.Mutex
	ids map[string]map[string]string // podKey -> containerName -> docker ID
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
	d := &Docker{cli: cli, ids: map[string]map[string]string{}}
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

func volumeBinds(pod *corev1.Pod, c corev1.Container, volRoot string) []string {
	var binds []string
	for _, m := range c.VolumeMounts {
		host := ""
		if pod != nil {
			for _, v := range pod.Spec.Volumes {
				if v.Name != m.Name {
					continue
				}
				if v.HostPath != nil {
					host = v.HostPath.Path
				} else if volRoot != "" {
					host = filepath.Join(volRoot, m.Name)
				}
			}
		}
		if host == "" {
			continue
		}
		mode := "rw"
		if m.ReadOnly {
			mode = "ro"
		}
		binds = append(binds, host+":"+m.MountPath+":"+mode)
	}
	return binds
}

func dockerResources(c corev1.Container) container.Resources {
	var res container.Resources
	if q, ok := c.Resources.Requests[corev1.ResourceCPU]; ok && !q.IsZero() {
		res.NanoCPUs = q.MilliValue() * 1_000_000
	}
	if q, ok := c.Resources.Requests[corev1.ResourceMemory]; ok && !q.IsZero() {
		if v, ok := q.AsInt64(); ok {
			res.Memory = v
		}
	}
	return res
}

func dockerHostConfig(pod *corev1.Pod, c corev1.Container, volRoot string) *container.HostConfig {
	return &container.HostConfig{
		Binds:     volumeBinds(pod, c, volRoot),
		Resources: dockerResources(c),
	}
}

func (d *Docker) Create(ctx context.Context, pod *corev1.Pod, c corev1.Container, volRoot string) error {
	if c.ImagePullPolicy != corev1.PullNever {
		rc, err := d.cli.ImagePull(ctx, c.Image, image.PullOptions{})
		if err := drainImagePull(rc, err); err != nil {
			return fmt.Errorf("docker pull %s: %w", c.Image, err)
		}
	}
	cfg := &container.Config{
		Image:      c.Image,
		Env:        envList(c.Env),
		WorkingDir: c.WorkingDir,
		Tty:        c.TTY,
	}
	if len(c.Command) > 0 {
		cfg.Entrypoint = c.Command
	}
	if len(c.Args) > 0 {
		cfg.Cmd = c.Args
	}
	host := dockerHostConfig(pod, c, volRoot)
	name := dockerName(pod.Namespace, pod.Name, c.Name)
	res, err := d.cli.ContainerCreate(ctx, cfg, host, nil, nil, name)
	if err != nil {
		return fmt.Errorf("docker create %s: %w", c.Name, err)
	}
	if err := d.cli.ContainerStart(ctx, res.ID, container.StartOptions{}); err != nil {
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

func (d *Docker) RemovePod(ctx context.Context, ns, name string, _ int64) error {
	d.mu.Lock()
	ids := d.ids[podKey(ns, name)]
	delete(d.ids, podKey(ns, name))
	d.mu.Unlock()
	var first error
	for _, id := range ids {
		if err := d.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true, RemoveVolumes: true}); err != nil && first == nil {
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

func (d *Docker) List(ctx context.Context, ns, name string) ([]Status, error) {
	d.mu.Lock()
	ids := d.ids[podKey(ns, name)]
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
	return d.cli.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     opts.Follow,
		Tail:       fmt.Sprintf("%d", opts.Tail),
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
	defer hr.Close()
	if attach != nil && attach.Stdout() != nil {
		_, _ = io.Copy(attach.Stdout(), hr.Reader)
	}
	return nil
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
	return nil
}

func envList(vars []corev1.EnvVar) []string {
	out := make([]string, 0, len(vars))
	for _, e := range vars {
		out = append(out, e.Name+"="+e.Value)
	}
	return out
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
