package main

import (
	"fmt"
	"io"
	"net"
	"os"

	dnconfig "github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/engine"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func cmdConsole(cfg *dnconfig.Config) *cobra.Command {
	var ns, name string
	c := &cobra.Command{
		Use:   "console",
		Short: "Attach to a pod VM's break-glass serial console (node must run with --serial-console)",
		Long: "Bridges this terminal to the VM serial console over the node's local\n" +
			"unix socket. Works even when the guest agent and SSH are both down.\n" +
			"Detach with ~. (tilde-dot), like a serial console.",
		RunE: func(cmd *cobra.Command, args []string) error {
			sock := engine.ConsoleSocketPath(ns, name)
			conn, err := net.Dial("unix", sock)
			if err != nil {
				return fmt.Errorf("console socket: %w (is the pod running with --serial-console?)", err)
			}
			defer conn.Close()
			fmt.Fprintf(os.Stderr, "connected to %s/%s console; detach with ~.\n", ns, name)
			return bridgeStdio(conn)
		},
	}
	c.Flags().StringVarP(&ns, "namespace", "n", "default", "pod namespace")
	c.Flags().StringVarP(&name, "name", "p", "", "pod name")
	_ = c.MarkFlagRequired("name")
	return c
}

// bridgeStdio puts the local terminal into raw mode and copies both
// directions until the remote side closes. Raw mode means ^C and other
// control bytes reach the guest line discipline untouched. Typing the
// escape sequence ~. (tilde then period) detaches, telnet-style.
func bridgeStdio(conn net.Conn) error {
	stdinFd := int(os.Stdin.Fd())
	restore := func() {}
	if term.IsTerminal(stdinFd) {
		old, err := term.MakeRaw(stdinFd)
		if err == nil {
			restore = func() { _ = term.Restore(stdinFd, old) }
		}
	}
	defer restore()

	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		ef := newEscapeFilter()
		buf := make([]byte, 512)
		for {
			n, rerr := os.Stdin.Read(buf)
			if n > 0 {
				out, ok := ef.feed(buf[:n])
				if !ok {
					fmt.Fprintln(os.Stderr, "\n~ detached")
					return
				}
				if len(out) > 0 {
					if _, werr := conn.Write(out); werr != nil {
						return
					}
				}
			}
			if rerr != nil {
				return
			}
		}
	}()
	go func() {
		_, _ = io.Copy(os.Stdout, conn)
		done <- struct{}{}
	}()
	<-done
	return nil
}

// escapeFilter forwards stdin bytes but swallows the two-byte detach
// sequence "~." (only when tilde follows a newline or start of input).
// It returns ok=false to signal detachment.
type escapeFilter struct {
	sawTilde    bool
	atLineStart bool
}

func newEscapeFilter() *escapeFilter { return &escapeFilter{atLineStart: true} }

func (f *escapeFilter) feed(p []byte) (out []byte, ok bool) {
	for _, b := range p {
		switch {
		case f.sawTilde:
			f.sawTilde = false
			if b == '.' {
				return out, false // detach requested
			}
			out = append(out, '~', b) // not an escape: flush both bytes
		case b == '~' && f.atLineStart:
			f.sawTilde = true // hold the byte until we see what follows
		default:
			out = append(out, b)
		}
		if b == '\r' || b == '\n' {
			f.atLineStart = true
		} else if b != '~' {
			f.atLineStart = false
		}
	}
	return out, true
}
