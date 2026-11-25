package engine

import (
	"context"
	"io"
	"testing"

	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"

	corev1 "k8s.io/api/core/v1"
	utilexec "k8s.io/utils/exec"
)

func TestExecInVMReturnsExitStatus(t *testing.T) {
	rt := fake.New()
	rt.ExecFn = func(_ context.Context, req guest.ExecReq, _ io.Reader, _, _ io.Writer) (int, error) {
		if len(req.Argv) > 0 && req.Argv[0] == "fail" {
			return 7, nil
		}
		return 0, nil
	}
	e := sidecarEngine(t, rt, sidecar.None{})
	pod := samplePod("ex", "uid-exec-exit")
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "ex", corev1.PodRunning)

	if err := e.ExecInVM(context.Background(), "default", "ex", []string{"true"}, nil); err != nil {
		t.Fatalf("exit 0: %v", err)
	}
	err := e.ExecInVM(context.Background(), "default", "ex", []string{"fail"}, nil)
	assertExecStatus(t, err, 7)

	att := &attachIO{out: &lockedBuffer{}}
	err = e.ExecInVM(context.Background(), "default", "ex", []string{"fail"}, att)
	assertExecStatus(t, err, 7)
}

func assertExecStatus(t *testing.T, err error, code int) {
	t.Helper()
	exitErr, ok := err.(utilexec.ExitError)
	if !ok || !exitErr.Exited() || exitErr.ExitStatus() != code {
		t.Fatalf("err %T %v, want virtual-kubelet exit status %d", err, err, code)
	}
}
