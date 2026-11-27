package supervisor

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/pkg/guest"
	"github.com/mlvea/darwin-node/pkg/types"
)

func TestStopHangBudgetIncludesGrace(t *testing.T) {
	if got := stopHangBudget(0, 0); got != 10*time.Second {
		t.Fatalf("supervisor hang budget %s, want 10s", got)
	}
	if got := stopHangBudget(3*time.Second, 0); got != 13*time.Second {
		t.Fatalf("supervisor hang budget with grace %s, want 13s", got)
	}
	if got := stopHangBudget(3*time.Second, 40*time.Millisecond); got != 3*time.Second+40*time.Millisecond {
		t.Fatalf("override budget %s", got)
	}
}

func TestFastStopDoesNotExit(t *testing.T) {
	s := &Server{
		vm: &blockingMachine{immediate: true},
		Exit: func(int) {
			t.Fatal("fast stop exited the process")
		},
	}
	stopped, err := s.stopVM(context.Background(), time.Second)
	if err != nil || !stopped {
		t.Fatalf("stopped %v err %v", stopped, err)
	}
}

func TestStopDeadlineExitsInsteadOfHanging(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	closeRelease := func() { once.Do(func() { close(release) }) }
	var code int
	s := &Server{
		vm:           &blockingMachine{release: release},
		StopDeadline: 40 * time.Millisecond,
		Exit: func(c int) {
			code = c
			closeRelease()
		},
	}
	var stopErr error
	finished := make(chan struct{})
	go func() {
		_, stopErr = s.stopVM(context.Background(), 0)
		close(finished)
	}()
	t.Cleanup(func() {
		closeRelease()
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			t.Errorf("stopVM did not return")
		}
	})
	select {
	case <-finished:
		if code != 1 || !errors.Is(stopErr, errStopDeadline) {
			t.Fatalf("exit code %d err %v, want exit 1 and deadline error", code, stopErr)
		}
	case <-time.After(700 * time.Millisecond):
		t.Fatal("vz stop deadline was not enforced")
	}
}

type blockingMachine struct {
	release   <-chan struct{}
	immediate bool
}

func (m *blockingMachine) ID() types.MachineID { return types.MachineID{UID: "block"} }

func (m *blockingMachine) Start(context.Context) error { return nil }

func (m *blockingMachine) Stop(ctx context.Context, _ time.Duration) error {
	if m.immediate {
		return nil
	}
	select {
	case <-m.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *blockingMachine) Status() types.VMStatus {
	return types.VMStatus{State: types.VMRunning}
}

func (m *blockingMachine) DialAgent(context.Context) (*guest.Client, error) {
	return nil, errors.New("no agent")
}

func (m *blockingMachine) Logs() io.ReadCloser {
	return io.NopCloser(strings.NewReader(""))
}
