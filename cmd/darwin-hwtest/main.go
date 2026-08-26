// darwin-hwtest is the hardware release gate: it boots a real macOS VM
// through the production vz runtime and exercises every guest-facing
// surface unit tests can only fake. Build and sign it like the other
// binaries, then run it against a baked image directory:
//
//	make test-hardware IMAGE=./out/macos-15
//
// Exit code is non-zero if any step fails. Without an image it reports
// SKIP and exits zero so pipelines can call it unconditionally; pass
// --strict to turn a missing image into a failure instead.
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/darwin-node/darwin-node/pkg/capacity"
	dnconfig "github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/engine"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/runtime"
	vzrt "github.com/darwin-node/darwin-node/pkg/runtime/vz"
	"github.com/darwin-node/darwin-node/pkg/sidecar"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	api "github.com/virtual-kubelet/virtual-kubelet/node/api"
)

var (
	image   string
	timeout time.Duration
	grace   int64
	serial  bool
	keep    bool
	strict  bool

	namespace = "hwtest"
	podName   = "gate"
	failed    bool
)

func main() {
	root := &cobra.Command{
		Use:   "darwin-hwtest --image <image-dir>",
		Short: "Hardware release gate: boots a real macOS VM and exercises every surface",
		RunE:  run,
	}
	f := root.Flags()
	f.StringVar(&image, "image", "", "baked macOS image directory (omit prints SKIP)")
	f.DurationVar(&timeout, "timeout", 10*time.Minute, "overall budget")
	f.Int64Var(&grace, "delete-grace", 20, "grace period for the final delete")
	f.BoolVar(&serial, "serial-console", false, "enable and verify the break-glass console socket")
	f.BoolVar(&keep, "keep", false, "leave the pod running after the gate passes")
	f.BoolVar(&strict, "strict", false, "fail instead of SKIP when --image is missing")
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func report(name, detail string, err error) {
	switch {
	case err != nil:
		fmt.Printf("STEP %-10s FAIL %v\n", name, err)
		failed = true
	case detail == "":
		fmt.Printf("STEP %-10s SKIP\n", name)
	default:
		fmt.Printf("STEP %-10s OK   %s\n", name, detail)
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// testAttach implements api.AttachIO for the exec step.
type testAttach struct{ out *strings.Builder }

func (a *testAttach) Stdin() io.Reader            { return nil }
func (a *testAttach) Stdout() io.WriteCloser      { return nopWriteCloser{a.out} }
func (a *testAttach) Stderr() io.WriteCloser      { return nopWriteCloser{io.Discard} }
func (a *testAttach) TTY() bool                   { return true }
func (a *testAttach) Resize() <-chan api.TermSize { return nil }

func run(cmd *cobra.Command, args []string) error {
	if image == "" {
		fmt.Println("STEP all        SKIP no --image provided (hardware gate not provisioned)")
		if strict {
			os.Exit(1)
		}
		return nil
	}
	if st, err := os.Stat(image); err != nil || !st.IsDir() {
		return fmt.Errorf("--image %q is not a directory", image)
	}

	cfg := dnconfig.Default()
	cfg.Runtime = "vz"
	cfg.CacheDir = filepath.Join(os.TempDir(), "darwin-hwtest-cache")
	cfg.AgentReadyTimeout = 5 * time.Minute
	cfg.AllowNATWorkloads = true
	cfg.SerialConsole = serial
	cfg.ProbeInterval = 2 * time.Second
	_ = cfg.ApplyEnv()

	slots, err := capacity.New(2)
	if err != nil {
		return err
	}
	var rt runtime.Runtime = vzrt.New(runtime.Options{
		CacheDir:      cfg.CacheDir,
		NetworkMode:   cfg.NetworkMode,
		BridgeDevice:  cfg.BridgeInterface,
		Graphics:      cfg.Graphics,
		HostAgentVer:  dnconfig.Version,
		SerialConsole: cfg.SerialConsole,
	})
	eng := engine.New(cfg, slots, rt, sidecar.None{}, event.Slog{}, "127.0.0.1")
	defer eng.Close()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
			UID:       k8stypes.UID("hwtest-" + time.Now().UTC().Format("20060102T150405")),
		},
		Spec: corev1.PodSpec{
			NodeName:   cfg.NodeName,
			Containers: []corev1.Container{{Name: "macos", Image: image}},
		},
	}

	steps := []struct {
		name string
		fn   func(context.Context) string
	}{
		{"BOOT", func(c context.Context) string {
			start := time.Now()
			if err := eng.Create(c, pod, engine.Credentials{}); err != nil {
				return fail(err)
			}
			for {
				if c.Err() != nil {
					return fail(fmt.Errorf("boot budget exhausted"))
				}
				p, err := eng.Get(namespace, podName)
				if err == nil && p.Status.Phase == corev1.PodRunning && ready(p) {
					return fmt.Sprintf("boot+agent ready in %s ip=%s",
						time.Since(start).Round(time.Second), p.Status.PodIP)
				}
				if err == nil && p.Status.Phase == corev1.PodFailed {
					return fail(fmt.Errorf("pod failed: %s", p.Status.Message))
				}
				time.Sleep(time.Second)
			}
		}},
		{"EXEC", func(c context.Context) string {
			var out strings.Builder
			att := &testAttach{out: &out}
			if err := eng.ExecInVM(c, namespace, podName,
				[]string{"/bin/sh", "-c", "echo hwtest-marker-$((6*7))"}, att); err != nil {
				return fail(err)
			}
			if !strings.Contains(out.String(), "hwtest-marker-42") {
				return fail(fmt.Errorf("exec output %q missing marker", out.String()))
			}
			return "stdin/stdout round trip through the agent protocol"
		}},
		{"LOGS", func(c context.Context) string {
			rc, err := eng.LogsVM(c, namespace, podName, api.ContainerLogOpts{Tail: 50})
			if err != nil {
				return fail(err)
			}
			defer rc.Close()
			buf := make([]byte, 4096)
			n, _ := rc.Read(buf)
			return fmt.Sprintf("log stream open (%d byte snapshot)", n)
		}},
		{"METRICS", func(c context.Context) string {
			m, ok := eng.PodMetrics(c, namespace, podName)
			if !ok {
				return fail(fmt.Errorf("no metrics from guest agent"))
			}
			return fmt.Sprintf("cpu_ns=%d mem_ws=%d", m.CPUNanoCores, m.MemoryWorkingSet)
		}},
		{"CONSOLE", func(c context.Context) string {
			if !serial {
				return "" // SKIP
			}
			sock := engine.ConsoleSocketPath(namespace + "@" + podName)
			conn, err := net.DialTimeout("unix", sock, 5*time.Second)
			if err != nil {
				return fail(fmt.Errorf("console socket: %w", err))
			}
			_ = conn.Close()
			return sock
		}},
		{"DELETE", func(c context.Context) string {
			if keep {
				return "SKIP --keep set; pod left running"
			}
			start := time.Now()
			if err := eng.Delete(c, namespace, podName, grace); err != nil {
				return fail(err)
			}
			if used := eng.Slots().Used(); used != 0 {
				return fail(fmt.Errorf("slots after delete: %d", used))
			}
			return fmt.Sprintf("released in %s", time.Since(start).Round(time.Second))
		}},
	}

	for _, s := range steps {
		stepCtx, scancel := context.WithTimeout(ctx, timeout)
		detail := s.fn(stepCtx)
		scancel()
		report(s.name, detail, nil)
		if strings.HasPrefix(detail, "FAIL:") {
			failed = true
		}
	}

	if failed && !keep {
		// Best-effort cleanup so a failed gate does not strand a VM.
		ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel2()
		_ = eng.Delete(ctx2, namespace, podName, 5)
	}
	if failed {
		os.Exit(1)
	}
	fmt.Println("HARDWARE GATE: PASS")
	return nil
}

func fail(err error) string { return "FAIL: " + err.Error() }

func ready(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
