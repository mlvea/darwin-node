package main

import (
	"context"
	"strings"
	"testing"

	"github.com/darwin-node/darwin-node/pkg/guest"
)

type orderStop struct {
	order *[]string
}

func (s orderStop) Sync() error {
	*s.order = append(*s.order, "sync")
	return nil
}

func (s orderStop) Halt(context.Context) error {
	*s.order = append(*s.order, "halt")
	return nil
}

func TestNewHandlerShutdownSyncsThenHalts(t *testing.T) {
	var order []string
	h := newHandler("tok", false, orderStop{order: &order})
	if h.ShutdownFn == nil {
		t.Fatal("Shutdown RPC is not wired")
	}
	if err := h.ShutdownFn(context.Background(), guest.ShutdownReq{Reason: "pod deleted"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "sync,halt" {
		t.Fatalf("shutdown order %v", order)
	}
}
