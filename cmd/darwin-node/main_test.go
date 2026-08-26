package main

import (
	"bytes"
	"testing"
)

func TestEscapeFilterDetaches(t *testing.T) {
	f := newEscapeFilter()
	out, ok := f.feed([]byte("ls\r\n"))
	if !ok || string(out) != "ls\r\n" {
		t.Fatalf("passthrough: %q %v", out, ok)
	}
	// "~." right after newline detaches; the tilde is swallowed first.
	out, ok = f.feed([]byte("~."))
	if ok {
		t.Fatal("escape must detach")
	}
	if len(out) != 0 {
		t.Fatalf("detach must not forward bytes: %q", out)
	}
}

func TestEscapeFlushesHeldTildeOnNonDot(t *testing.T) {
	f := newEscapeFilter()
	out, ok := f.feed([]byte("~x"))
	if !ok {
		t.Fatal("non-dot after tilde must not detach")
	}
	if !bytes.Equal(out, []byte("~x")) {
		t.Fatalf("held tilde must flush: %q", out)
	}
}

func TestEscapeTildeMidLinePassesThrough(t *testing.T) {
	f := newEscapeFilter()
	out, ok := f.feed([]byte("a~b"))
	if !ok || string(out) != "a~b" {
		t.Fatalf("mid-line tilde passes: %q %v", out, ok)
	}
}

func TestEscapeHeldTildeAtEOFStillDetachesLater(t *testing.T) {
	f := newEscapeFilter()
	f.feed([]byte("\n~"))
	out, ok := f.feed([]byte(".rest"))
	if ok {
		t.Fatal("held tilde plus dot must detach")
	}
	if len(out) != 0 {
		t.Fatalf("no bytes may leak past detach: %q", out)
	}
}
