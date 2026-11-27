package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/mlvea/darwin-node/pkg/guest"

	corev1 "k8s.io/api/core/v1"
)

const imageEntrypointUnavailable = "the VM image entrypoint is not available to the host"

// validateContainerWorkload rejects VM container fields the guest cannot
// apply. command replaces the image entrypoint. args, env, and workingDir
// are applied with that command. The host cannot see the image entrypoint,
// so those fields without command would be silently dropped.
func validateContainerWorkload(c corev1.Container) error {
	if len(c.EnvFrom) > 0 {
		return fmt.Errorf("envFrom is not supported; virtual-kubelet did not expand it into env")
	}
	for _, ev := range c.Env {
		if ev.Name == "" || strings.ContainsAny(ev.Name, "=\x00") {
			return fmt.Errorf("env %q: invalid name", ev.Name)
		}
		if strings.ContainsRune(ev.Value, 0) {
			return fmt.Errorf("env %s: value contains NUL", ev.Name)
		}
		if ev.ValueFrom != nil && ev.Value == "" {
			return fmt.Errorf("env %s: valueFrom was not resolved", ev.Name)
		}
	}
	if len(c.Command) == 0 {
		if len(c.Args) > 0 {
			return fmt.Errorf("args require command; %s", imageEntrypointUnavailable)
		}
		if len(c.Env) > 0 {
			return fmt.Errorf("env requires command; %s", imageEntrypointUnavailable)
		}
		if c.WorkingDir != "" {
			return fmt.Errorf("workingDir requires command; %s", imageEntrypointUnavailable)
		}
		return nil
	}
	if c.Command[0] == "" {
		return fmt.Errorf("command is empty")
	}
	return nil
}

// containerWorkload is the Workload RPC for container 0. A nil request
// means the pod left the image entrypoint in place.
func containerWorkload(c corev1.Container) (*guest.WorkloadReq, error) {
	if err := validateContainerWorkload(c); err != nil {
		return nil, fmt.Errorf("container %q: %w", c.Name, err)
	}
	if len(c.Command) == 0 {
		return nil, nil
	}
	argv := make([]string, 0, len(c.Command)+len(c.Args))
	argv = append(argv, c.Command...)
	argv = append(argv, c.Args...)
	var env map[string]string
	if len(c.Env) > 0 {
		env = make(map[string]string, len(c.Env))
		for _, ev := range c.Env {
			env[ev.Name] = ev.Value
		}
	}
	return &guest.WorkloadReq{Argv: argv, Env: env, Dir: c.WorkingDir}, nil
}

// applyContainerWorkload installs the per-job keychain and then sends the
// VM container command. Pods with no command, args, or env still install
// the keychain. They do not call the Workload RPC, so an older agent that
// is allowed to skip the keychain keeps running the image entrypoint.
func (e *Engine) applyContainerWorkload(ctx context.Context, cli *guest.Client, pod *corev1.Pod) error {
	if cli == nil || pod == nil || len(pod.Spec.Containers) == 0 {
		return nil
	}
	c := pod.Spec.Containers[0]
	req, err := containerWorkload(c)
	if err != nil {
		return err
	}
	if err := e.prepareEphemeralKeychain(ctx, cli, pod); err != nil {
		return err
	}
	if req == nil {
		return nil
	}
	if err := cli.StartWorkload(ctx, *req); err != nil {
		return fmt.Errorf("container %q: command was not applied: %w", c.Name, err)
	}
	return nil
}
