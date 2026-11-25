package engine

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mlvea/darwin-node/pkg/guest"
)

func TestProbeMessageTruncatesGuestText(t *testing.T) {
	raw := strings.Repeat("x", 2000) + "\n\x00\x1b" + "tail"
	got := probeMsg(guest.ProbeRes{OK: false, Message: raw}, nil)
	if n := utf8.RuneCountInString(got); n > 1024 {
		t.Fatalf("guest probe message is %d chars; PodStatus and events must stay within 1024", n)
	}
	if strings.ContainsAny(got, "\n\x00\x1b") {
		t.Fatalf("non-printable guest text kept: %q", got)
	}
	if !strings.HasPrefix(got, "xxx") || strings.Contains(got, "tail") {
		t.Fatalf("message %q was not the printable prefix", got)
	}
}
