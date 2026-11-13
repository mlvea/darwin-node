package sidecar

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// scriptedDocker records the calls Create makes. A second Create for a
// container this runtime already tracks has to start that container, not
// pull and create another one with the same name.
type scriptedDocker struct {
	running     bool
	inspectErr  error
	failStartID string
	pulls       int
	creates     int
	starts      int
	removes     int
	started     []string
}

func (f *scriptedDocker) ImagePull(context.Context, string, image.PullOptions) (io.ReadCloser, error) {
	f.pulls++
	return io.NopCloser(bytes.NewReader(nil)), nil
}

func (f *scriptedDocker) ContainerCreate(context.Context, *container.Config, *container.HostConfig, *network.NetworkingConfig, *ocispec.Platform, string) (container.CreateResponse, error) {
	f.creates++
	return container.CreateResponse{ID: "new-id"}, nil
}

func (f *scriptedDocker) ContainerStart(_ context.Context, id string, _ container.StartOptions) error {
	f.starts++
	f.started = append(f.started, id)
	if id == f.failStartID {
		return errors.New("cannot start")
	}
	f.running = true
	return nil
}

func (f *scriptedDocker) ContainerRemove(context.Context, string, container.RemoveOptions) error {
	f.removes++
	return nil
}

func (f *scriptedDocker) ContainerInspect(_ context.Context, id string) (types.ContainerJSON, error) {
	if f.inspectErr != nil {
		return types.ContainerJSON{}, f.inspectErr
	}
	return types.ContainerJSON{ContainerJSONBase: &types.ContainerJSONBase{
		ID:    id,
		State: &types.ContainerState{Running: f.running, ExitCode: 1},
	}}, nil
}

func (f *scriptedDocker) ContainerList(context.Context, container.ListOptions) ([]types.Container, error) {
	return nil, errors.New("unused")
}
func (f *scriptedDocker) ContainerLogs(context.Context, string, container.LogsOptions) (io.ReadCloser, error) {
	return nil, errors.New("unused")
}
func (f *scriptedDocker) ContainerExecCreate(context.Context, string, types.ExecConfig) (types.IDResponse, error) {
	return types.IDResponse{}, errors.New("unused")
}
func (f *scriptedDocker) ContainerExecAttach(context.Context, string, types.ExecStartCheck) (types.HijackedResponse, error) {
	return types.HijackedResponse{}, errors.New("unused")
}
func (f *scriptedDocker) ContainerExecInspect(context.Context, string) (types.ContainerExecInspect, error) {
	return types.ContainerExecInspect{}, errors.New("unused")
}

func restartPod() (*corev1.Pod, corev1.Container) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p"}}
	c := corev1.Container{Name: "log", Image: "busybox"}
	return pod, c
}

func trackedDocker(cli *scriptedDocker) *Docker {
	return &Docker{cli: cli, ids: map[string]map[string]string{"ns/p": {"log": "abc"}}}
}

func TestDockerCreateFirstContainer(t *testing.T) {
	cli := &scriptedDocker{}
	d := &Docker{cli: cli, ids: map[string]map[string]string{}}
	pod, c := restartPod()
	c.ImagePullPolicy = corev1.PullNever
	if err := d.Create(context.Background(), pod, c, ""); err != nil {
		t.Fatal(err)
	}
	if cli.pulls != 0 || cli.creates != 1 || cli.starts != 1 || cli.removes != 0 || d.ids["ns/p"]["log"] != "new-id" {
		t.Fatalf("pulls=%d creates=%d starts=%d removes=%d id=%q", cli.pulls, cli.creates, cli.starts, cli.removes, d.ids["ns/p"]["log"])
	}
}

func TestDockerCreateRemovesContainerWhenStartFails(t *testing.T) {
	cli := &scriptedDocker{failStartID: "new-id"}
	d := &Docker{cli: cli, ids: map[string]map[string]string{}}
	pod, c := restartPod()
	c.ImagePullPolicy = corev1.PullNever
	err := d.Create(context.Background(), pod, c, "")
	if err == nil || !strings.Contains(err.Error(), "docker start") {
		t.Fatalf("err %v", err)
	}
	if cli.creates != 1 || cli.removes != 1 {
		t.Fatalf("creates=%d removes=%d, failed start left a container", cli.creates, cli.removes)
	}
	if id := d.ids["ns/p"]["log"]; id != "" {
		t.Fatalf("id %q stored after start failure", id)
	}
}

func TestDockerCreateStartsStoppedContainer(t *testing.T) {
	cli := &scriptedDocker{}
	d := trackedDocker(cli)
	pod, c := restartPod()
	if err := d.Create(context.Background(), pod, c, ""); err != nil {
		t.Fatal(err)
	}
	if cli.pulls != 0 || cli.creates != 0 || cli.starts != 1 || cli.removes != 0 || len(cli.started) != 1 || cli.started[0] != "abc" {
		t.Fatalf("pulls=%d creates=%d starts=%d removes=%d started=%v, exited sidecar was recreated", cli.pulls, cli.creates, cli.starts, cli.removes, cli.started)
	}
	if got := d.ids["ns/p"]["log"]; got != "abc" {
		t.Fatalf("id %q", got)
	}
}

func TestDockerCreateLeavesRunningContainer(t *testing.T) {
	cli := &scriptedDocker{running: true}
	d := trackedDocker(cli)
	pod, c := restartPod()
	if err := d.Create(context.Background(), pod, c, ""); err != nil {
		t.Fatal(err)
	}
	if cli.pulls != 0 || cli.creates != 0 || cli.starts != 0 || cli.removes != 0 {
		t.Fatalf("pulls=%d creates=%d starts=%d removes=%d, running sidecar was touched", cli.pulls, cli.creates, cli.starts, cli.removes)
	}
}

func TestDockerCreateReplacesContainerThatWillNotStart(t *testing.T) {
	cli := &scriptedDocker{failStartID: "abc"}
	d := trackedDocker(cli)
	pod, c := restartPod()
	if err := d.Create(context.Background(), pod, c, ""); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if cli.removes != 1 || cli.creates != 1 || cli.pulls != 1 {
		t.Fatalf("pulls=%d creates=%d removes=%d starts=%v, dead container was not replaced", cli.pulls, cli.creates, cli.removes, cli.started)
	}
	if got := d.ids["ns/p"]["log"]; got != "new-id" {
		t.Fatalf("id %q", got)
	}
}

func TestDockerCreateReplacesContainerMissingFromDaemon(t *testing.T) {
	cli := &scriptedDocker{inspectErr: errors.New("no such container")}
	d := trackedDocker(cli)
	pod, c := restartPod()
	if err := d.Create(context.Background(), pod, c, ""); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if cli.removes != 1 || cli.creates != 1 || cli.starts != 1 || cli.started[0] != "new-id" {
		t.Fatalf("creates=%d removes=%d started=%v", cli.creates, cli.removes, cli.started)
	}
	if got := d.ids["ns/p"]["log"]; got != "new-id" {
		t.Fatalf("id %q", got)
	}
}
