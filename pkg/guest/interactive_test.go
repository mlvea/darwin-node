package guest

import (
	"bytes"
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// lockedBuffer is an io.Writer safe for concurrent use.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// serveTestHandler runs Serve over a net.Pipe and returns the host-side client.
func serveTestHandler(t *testing.T, h Handler) (*Client, *LogBuffer) {
	t.Helper()
	h.Init()
	hostC, agentC := net.Pipe()
	go func() { _ = Serve(context.Background(), agentC, h) }()
	cli, err := Dial(context.Background(), hostC, h.CurrentToken(), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli, h.LogBuffer
}

func TestExecInteractiveEchoesStdin(t *testing.T) {
	out := &lockedBuffer{}
	h := Handler{
		Token: "tok",
		ExecFn: func(ctx context.Context, req ExecReq, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
			if _, err := io.Copy(stdout, stdin); err != nil && err != io.EOF {
				return 1, err
			}
			return 0, nil
		},
	}
	h.LogBuffer = NewLogBuffer(64)
	cli, _ := serveTestHandler(t, h)

	stdinR, stdinW := io.Pipe()
	done := make(chan int, 1)
	go func() {
		code, err := cli.ExecInteractive(context.Background(),
			ExecReq{Argv: []string{"cat"}, Stdin: true}, stdinR, nil,
			out, io.Discard)
		if err != nil {
			t.Errorf("exec: %v", err)
		}
		done <- code
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && out.String() == "" {
		if _, err := stdinW.Write([]byte("hello-stdin\n")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := out.String(); !bytes.Contains([]byte(got), []byte("hello-stdin")) {
		t.Fatalf("stdin not echoed to stdout: %q", got)
	}
	_ = stdinW.Close()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("exec did not finish after stdin EOF")
	}
}

func TestLogsFollowStreamsAppends(t *testing.T) {
	lb := NewLogBuffer(256)
	lb.Write([]byte("first\n"))
	cli, lb2 := serveTestHandler(t, Handler{Token: "tok", LogBuffer: lb})
	_ = lb2

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	go func() { errCh <- cli.LogsFollow(ctx, LogsReq{TailLines: 10}, pw) }()

	readLine := func() string {
		buf := make([]byte, 128)
		n, err := pr.Read(buf)
		if err != nil && n == 0 {
			return ""
		}
		return string(buf[:n])
	}
	if got := readLine(); got != "first\n" {
		t.Fatalf("snapshot line %q", got)
	}

	lb.Write([]byte("second\n"))
	deadline := time.Now().Add(5 * time.Second)
	got := ""
	for time.Now().Before(deadline) {
		got += readLineNonblock(pr, 50*time.Millisecond)
		if got == "second\n" {
			break
		}
	}
	if got != "second\n" {
		t.Fatalf("follow did not deliver append: %q", got)
	}
	cancel()
	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("LogsFollow did not return after cancel")
	}
}

func readLineNonblock(r io.Reader, wait time.Duration) string {
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	buf := make([]byte, 128)
	go func() {
		n, err := r.Read(buf)
		ch <- result{n, err}
	}()
	select {
	case res := <-ch:
		if res.n == 0 {
			return ""
		}
		return string(buf[:res.n])
	case <-time.After(wait):
		return ""
	}
}

func TestUpstreamRouterForwardAndDrop(t *testing.T) {
	u := newUpstreamRouter()
	ch := u.register("42")
	env := Envelope{ID: "42", Kind: KindStream, Method: MethodExec, Payload: mustJSON(ExecStdin{Data: []byte("x")})}
	for i := 0; i < 200; i++ {
		u.forward(env)
	}
	drained := 0
drain:
	for {
		select {
		case <-ch:
			drained++
		default:
			break drain
		}
	}
	if drained == 0 || drained > cap(ch)+1 {
		t.Fatalf("forward delivered %d frames (cap=%d)", drained, cap(ch))
	}
	u.unregister("42")
	before := len(ch)
	u.forward(env)
	if len(ch) != before {
		t.Fatal("unregistered id still receives frames")
	}
}

func TestExecStdinReaderDecodesAndEOFs(t *testing.T) {
	ch := make(chan Envelope, 4)
	r := newExecStdinReader(ch, true)
	ch <- Envelope{Payload: mustJSON(ExecStdin{Data: []byte("ab")})}
	ch <- Envelope{Payload: mustJSON(TtyResize{Cols: 80, Rows: 24})}
	ch <- Envelope{Payload: mustJSON(ExecStdin{Data: []byte("cd")})}
	ch <- Envelope{Payload: mustJSON(ExecStdin{EOF: true})}

	buf := make([]byte, 8)
	n, err := r.Read(buf)
	if err != nil || string(buf[:n]) != "ab" {
		t.Fatalf("first read got %q %v", buf[:n], err)
	}
	n, err = r.Read(buf)
	if err != nil || string(buf[:n]) != "cd" {
		t.Fatalf("second read got %q %v", buf[:n], err)
	}
	if _, err := r.Read(buf); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
}
