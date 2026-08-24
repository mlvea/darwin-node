package netutil

import "testing"

func TestRandomMAC(t *testing.T) {
	a, err := RandomMAC()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 6 {
		t.Fatalf("len %d", len(a))
	}
	if a[0]&0x01 != 0 {
		t.Fatal("multicast bit set")
	}
	if a[0]&0x02 == 0 {
		t.Fatal("locally administered bit not set")
	}
}

func TestNormalizeMAC(t *testing.T) {
	if got := NormalizeMAC("0A:1B:0C:00:EE:FF"); got != "a:1b:c:0:ee:ff" {
		t.Fatalf("got %s", got)
	}
}
