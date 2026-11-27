package nodecert

import (
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport"
)

// rotatingTransport presents the certificate returned by get.
//
// client-go's cert-rotating dialer closes every tracked connection when
// the certificate changes (connrotation.Dialer.CloseAll). That aborts
// requests that have already been written, including pod status updates.
// This transport instead gives requests that start after the change a new
// http.Transport, and closes the previous transport's idle connections
// only after its in-flight RoundTrips have returned.
type rotatingTransport struct {
	base *rest.Config
	get  func() (*tls.Certificate, error)

	mu  sync.Mutex
	cur *trackedRT
	fp  string
}

type trackedRT struct {
	rt http.RoundTripper
	n  int
}

// NewRotatingTransport wraps base so each new request presents the
// certificate from get. base's client certificate and bearer token are
// not used; Host, CA, and server name are.
func NewRotatingTransport(base *rest.Config, get func() (*tls.Certificate, error)) (http.RoundTripper, error) {
	if base == nil {
		return nil, errors.New("rest config is required")
	}
	if get == nil {
		return nil, errors.New("client certificate function is required")
	}
	return &rotatingTransport{base: rest.CopyConfig(base), get: get}, nil
}

func (r *rotatingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	h, err := r.acquire()
	if err != nil {
		return nil, err
	}
	defer r.release(h)
	return h.rt.RoundTrip(req)
}

func (r *rotatingTransport) acquire() (*trackedRT, error) {
	cert, err := r.get()
	if err != nil {
		return nil, err
	}
	if cert == nil || len(cert.Certificate) == 0 {
		return nil, errors.New("no client certificate")
	}
	fp := fingerprint(cert)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur != nil && r.fp == fp {
		r.cur.n++
		return r.cur, nil
	}
	rt, err := r.build()
	if err != nil {
		return nil, err
	}
	next := &trackedRT{rt: rt, n: 1}
	r.cur = next
	r.fp = fp
	return next, nil
}

func (r *rotatingTransport) release(h *trackedRT) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h.n--
	if h != r.cur && h.n == 0 {
		utilnet.CloseIdleConnectionsFor(h.rt)
	}
}

func (r *rotatingTransport) build() (http.RoundTripper, error) {
	cfg := rest.CopyConfig(r.base)
	cfg.CertFile = ""
	cfg.KeyFile = ""
	cfg.CertData = nil
	cfg.KeyData = nil
	tcfg, err := cfg.TransportConfig()
	if err != nil {
		return nil, err
	}
	// A file path would select client-go's reload dialer, which closes
	// live connections. The callback is invoked on each new handshake.
	tcfg.TLS.CertFile = ""
	tcfg.TLS.KeyFile = ""
	tcfg.TLS.CertData = nil
	tcfg.TLS.KeyData = nil
	tcfg.TLS.ReloadTLSFiles = false
	tcfg.TLS.GetCertHolder = &transport.GetCertHolder{GetCert: r.get}
	tlsCfg, err := transport.TLSConfigFor(tcfg)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	base := utilnet.SetTransportDefaults(&http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig:     tlsCfg,
		ForceAttemptHTTP2:   true,
	})
	// Build the transport directly. transport.New caches one Transport
	// per TLS config, and a shared pool would keep HTTP/2 streams on
	// the connection that still holds the previous certificate.
	return transport.HTTPWrappersForConfig(tcfg, base)
}

func fingerprint(cert *tls.Certificate) string {
	sum := sha256.Sum256(cert.Certificate[0])
	return string(sum[:])
}
