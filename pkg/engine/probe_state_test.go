package engine

import "testing"

func TestProbeStreakSuccessAndFailureThresholds(t *testing.T) {
	var s probeStreak
	if passed, failed := s.observe(true, "", 3, 3); passed || failed || s.passed {
		t.Fatalf("first success published %+v", s)
	}
	s.observe(true, "", 3, 3)
	passed, failed := s.observe(true, "", 3, 3)
	if !passed || failed || !s.passed || s.fails != 0 {
		t.Fatalf("third success %+v passed=%v failed=%v", s, passed, failed)
	}
	passed, failed = s.observe(false, "no", 3, 3)
	if !passed || failed || !s.passed || s.fails != 1 || s.oks != 0 {
		t.Fatalf("one failure cleared the streak %+v", s)
	}
	s.observe(false, "no", 3, 3)
	passed, failed = s.observe(false, "no", 3, 3)
	if passed || !failed || s.passed || s.msg != "no" {
		t.Fatalf("third failure %+v passed=%v failed=%v", s, passed, failed)
	}
	s.observe(true, "", 3, 3)
	s.observe(true, "", 3, 3)
	passed, failed = s.observe(true, "", 3, 3)
	if !passed || failed || !s.passed || s.msg != "" {
		t.Fatalf("did not recover %+v", s)
	}
}

func TestProbeStreakSuccessClearsFailureStreak(t *testing.T) {
	var s probeStreak
	s.observe(false, "x", 1, 3)
	s.observe(false, "x", 1, 3)
	if _, failed := s.observe(true, "", 1, 3); failed || s.fails != 0 || !s.passed {
		t.Fatalf("success did not clear %+v", s)
	}
	if _, failed := s.observe(false, "x", 1, 3); failed || s.fails != 1 {
		t.Fatalf("cleared streak tripped %+v", s)
	}
	s.observe(false, "x", 1, 3)
	if _, failed := s.observe(false, "x", 1, 3); !failed {
		t.Fatal("failureThreshold did not trip after the clear")
	}
}
