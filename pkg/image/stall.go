package image

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"oras.land/oras-go/v2/registry/remote/retry"
)

// stallError is a read that made no progress. It is not a net.Error, so the
// ORAS retry policy does not treat it as a dial timeout and try again.
type stallError struct {
	d time.Duration
}

func (e *stallError) Error() string {
	return fmt.Sprintf("registry read stalled for %s", e.d)
}

func (m *Manager) httpClient() *http.Client {
	return &http.Client{Transport: retry.NewTransport(newStallTransport(m.readStall()))}
}

func newStallTransport(stall time.Duration) *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{}
	}
	t := base.Clone()
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if stall <= 0 {
			return c, nil
		}
		return &stallConn{Conn: c, stall: stall}, nil
	}
	return t
}

type stallConn struct {
	net.Conn
	stall time.Duration
}

func (c *stallConn) Read(p []byte) (int, error) {
	if c.stall > 0 {
		_ = c.Conn.SetReadDeadline(time.Now().Add(c.stall))
	}
	n, err := c.Conn.Read(p)
	if err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			return n, &stallError{d: c.stall}
		}
	}
	return n, err
}
