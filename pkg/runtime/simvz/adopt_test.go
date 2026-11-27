package simvz

import (
	"strings"
	"testing"

	"github.com/mlvea/darwin-node/pkg/guest"
)

func TestGuestAgentEnvForcesSoftwarePlatform(t *testing.T) {
	want := guest.EnvGuestSoftwarePlatform + "=1"
	found := false
	for _, e := range guestAgentEnv() {
		if e == want {
			found = true
		}
		if strings.HasPrefix(e, guest.EnvGuestSoftwarePlatform+"=") && e != want {
			t.Fatalf("software platform env %q", e)
		}
	}
	if !found {
		t.Fatal("spawned guest agent would use the host OS platform")
	}
}
