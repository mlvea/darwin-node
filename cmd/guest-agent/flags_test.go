package main

import "testing"

func TestNoAgentTCPFallbackFlag(t *testing.T) {
	cmd := newCommand()
	if cmd.Flags().Lookup("agent-tcp-fallback") != nil || cmd.PersistentFlags().Lookup("agent-tcp-fallback") != nil {
		t.Fatal("plaintext TCP fallback is still a flag")
	}
	if cmd.PersistentFlags().Lookup("listen") != nil {
		t.Fatal("TCP listen address is still a flag")
	}
}
