package main

import (
	"runtime"
	"testing"

	"github.com/mlvea/darwin-node/pkg/guest"
)

func TestNewHandlerWiresKeychainPlatform(t *testing.T) {
	h := newHandler("tok", false, nil)
	if h.KeychainPlatform == nil {
		t.Fatal("keychain platform not wired")
	}
	// The darwin platform changes the Mac it runs on. Linux CI exercises
	// the in-memory image.
	if runtime.GOOS == "darwin" {
		return
	}
	res, err := guest.InstallEphemeralKeychain(guest.EphemeralKeychainReq{PodUID: "uid"}, h.KeychainPlatform)
	if err != nil || !res.OK {
		t.Fatalf("software keychain: %v %+v", err, res)
	}
	if err := guest.CheckEphemeralKeychain("uid", res); err != nil {
		t.Fatal(err)
	}
}
