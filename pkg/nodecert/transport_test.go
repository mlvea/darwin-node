package nodecert

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/rest"
)

func TestClientRotationDoesNotDropInflightPodStatus(t *testing.T) {
	ca, caKey := testCA(t)
	oldCert := mintClient(t, ca, caKey, big.NewInt(11))
	newCert := mintClient(t, ca, caKey, big.NewInt(22))
	pool := x509.NewCertPool()
	pool.AddCert(ca)

	var mu sync.Mutex
	var seen []string
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		serial := ""
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			serial = r.TLS.PeerCertificates[0].SerialNumber.String()
		}
		mu.Lock()
		seen = append(seen, serial+":"+string(body))
		mu.Unlock()
		if string(body) == "status-update" {
			once.Do(func() { close(entered) })
			<-release
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	srv.TLS = &tls.Config{
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  pool,
		MinVersion: tls.VersionTLS12,
	}
	srv.StartTLS()
	defer srv.Close()
	defer unblock()

	current := oldCert
	var swap sync.Mutex
	rt, err := NewRotatingTransport(&rest.Config{
		Host:            srv.URL,
		TLSClientConfig: rest.TLSClientConfig{Insecure: true},
	}, func() (*tls.Certificate, error) {
		swap.Lock()
		defer swap.Unlock()
		return current, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: rt}

	firstDone := make(chan error, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPatch, srv.URL+"/pods/status", bytes.NewBufferString("status-update"))
		if err != nil {
			firstDone <- err
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			firstDone <- err
			return
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			firstDone <- statusErr(resp.StatusCode)
			return
		}
		firstDone <- nil
	}()

	select {
	case <-entered:
	case err := <-firstDone:
		t.Fatalf("first request finished before the handler blocked it: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("first request did not reach the apiserver")
	}

	swap.Lock()
	current = newCert
	swap.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, srv.URL+"/pods/status", bytes.NewBufferString("next-update"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request after rotation: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d", resp.StatusCode)
	}

	unblock()
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("in-flight request: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request did not finish")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("requests seen %v", seen)
	}
	if seen[0] != oldCert.Leaf.SerialNumber.String()+":status-update" {
		t.Fatalf("in-flight request presented %s", seen[0])
	}
	if seen[1] != newCert.Leaf.SerialNumber.String()+":next-update" {
		t.Fatalf("rotated request presented %s", seen[1])
	}
}

type statusErr int

func (s statusErr) Error() string { return http.StatusText(int(s)) }

func mintClient(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, serial *big.Int) *tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "system:node:mini", Organization: []string{"system:nodes"}},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}
