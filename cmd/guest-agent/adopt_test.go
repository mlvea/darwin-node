package main

import (
	"runtime"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/pkg/guest"
)

func TestNewHandlerWiresAdoptPlatform(t *testing.T) {
	h := newHandler("tok", false, nil)
	if h.AdoptPlatform == nil {
		t.Fatal("adopt platform not wired")
	}
	// The darwin platform reseeds the Mac it runs on. Only the in-guest
	// process should call it. Linux CI exercises the stand-in.
	if runtime.GOOS == "darwin" {
		return
	}
	req, err := guest.NewAdoptRequest("uid", "pod", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	res, err := guest.PerformAdopt(req, h.AdoptPlatform)
	if err != nil || !res.ReseedOK {
		t.Fatalf("software adopt: %v %+v", err, res)
	}
}
