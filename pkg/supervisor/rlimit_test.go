//go:build linux || darwin

package supervisor

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestRaiseNOFILE(t *testing.T) {
	var orig unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &orig); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = unix.Setrlimit(unix.RLIMIT_NOFILE, &orig)
	})
	low := orig
	low.Cur = 512
	if low.Max < low.Cur {
		low.Cur = low.Max
	}
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &low); err != nil {
		t.Fatal(err)
	}
	if err := RaiseNOFILE(); err != nil {
		t.Fatal(err)
	}
	var got unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &got); err != nil {
		t.Fatal(err)
	}
	want := uint64(65536)
	if got.Max < want {
		want = got.Max
	}
	if got.Cur != want {
		t.Fatalf("RLIMIT_NOFILE cur %d, want %d (max %d)", got.Cur, want, got.Max)
	}
}
