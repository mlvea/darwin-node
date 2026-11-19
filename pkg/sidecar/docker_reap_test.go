package sidecar

import (
	"context"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
)

// listDocker is a daemon that still has containers from a process that
// was killed. reapDangling must remove only darwin-node's names.
type listDocker struct {
	scriptedDocker
	listed  []types.Container
	removed []string
}

func (f *listDocker) ContainerList(context.Context, container.ListOptions) ([]types.Container, error) {
	return f.listed, nil
}

func (f *listDocker) ContainerRemove(_ context.Context, id string, _ container.RemoveOptions) error {
	f.mu.Lock()
	f.removed = append(f.removed, id)
	f.removes++
	f.mu.Unlock()
	return nil
}

func TestReapDanglingRemovesCrashedContainers(t *testing.T) {
	cli := &listDocker{listed: []types.Container{
		{ID: "orphan", Names: []string{"/darwin-node_default_old_log"}},
		{ID: "plain", Names: []string{"darwin-node_ns_p_side"}},
		{ID: "user", Names: []string{"/nginx"}},
		{ID: "prefix", Names: []string{"/darwin-node"}},
	}}
	d := &Docker{cli: cli, ids: map[string]map[string]string{}}
	if err := d.reapDangling(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, id := range cli.removed {
		got[id] = true
	}
	if !got["orphan"] || !got["plain"] {
		t.Fatalf("removed %v, crashed darwin-node containers stayed", cli.removed)
	}
	if got["user"] || got["prefix"] {
		t.Fatalf("removed %v, reap deleted a container it does not own", cli.removed)
	}
}
