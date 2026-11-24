package guest

import (
	"context"
	"errors"
	"testing"
)

type orderStop struct {
	order *[]string
	syncE error
	haltE error
}

func (s orderStop) Sync() error {
	*s.order = append(*s.order, "sync")
	return s.syncE
}

func (s orderStop) Halt(context.Context) error {
	*s.order = append(*s.order, "halt")
	return s.haltE
}

func TestShutdownSyncsThenHalts(t *testing.T) {
	var order []string
	err := ShutdownFunc(orderStop{order: &order})(context.Background(), ShutdownReq{Reason: "stop"})
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "sync" || order[1] != "halt" {
		t.Fatalf("order %v", order)
	}
}

func TestShutdownHaltsAfterSyncError(t *testing.T) {
	var order []string
	syncErr := errors.New("sync failed")
	haltErr := errors.New("halt failed")
	err := ShutdownFunc(orderStop{order: &order, syncE: syncErr, haltE: haltErr})(context.Background(), ShutdownReq{})
	if !errors.Is(err, syncErr) || !errors.Is(err, haltErr) {
		t.Fatalf("err %v", err)
	}
	if len(order) != 2 || order[0] != "sync" || order[1] != "halt" {
		t.Fatalf("halt skipped after sync error: %v", order)
	}
}
