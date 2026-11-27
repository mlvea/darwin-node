package nodecert

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

const testCertLife = 1000 * time.Second

var testCSRGVR = schema.GroupVersionResource{Group: "certificates.k8s.io", Version: "v1", Resource: "certificatesigningrequests"}

func withClock(t *testing.T, fn func(t *testing.T)) {
	t.Helper()
	prev := utilruntime.ErrorHandlers
	utilruntime.ErrorHandlers = []utilruntime.ErrorHandler{
		func(context.Context, error, string, ...interface{}) {},
	}
	t.Cleanup(func() { utilruntime.ErrorHandlers = prev })
	synctest.Test(t, fn)
}

func TestServingCertificateIssued(t *testing.T) {
	withClock(t, func(t *testing.T) {
		dir := t.TempDir()
		cs := fake.NewSimpleClientset()
		signer := newCSRSigner(t, cs, testCertLife, certificatesv1.KubeletServingSignerName)
		rot := startServing(t, dir, cs)
		defer stopRotation(rot)
		synctest.Wait()

		got := rot.Current()
		if got == nil || got.Leaf == nil {
			t.Fatalf("no serving certificate, creates %d", signer.creates.Load())
		}
		if got.Leaf.Subject.CommonName != "system:node:mini" {
			t.Errorf("cn %q", got.Leaf.Subject.CommonName)
		}
		if !hasOrg(got.Leaf, "system:nodes") {
			t.Errorf("orgs %v", got.Leaf.Subject.Organization)
		}
		if !hasDNS(got.Leaf, "mini") {
			t.Errorf("dns %v", got.Leaf.DNSNames)
		}
		if !hasIP(got.Leaf, "10.0.0.8") {
			t.Errorf("ips %v", got.Leaf.IPAddresses)
		}
		if signer.creates.Load() != 1 {
			t.Errorf("creates %d, want 1", signer.creates.Load())
		}
		if signer.arrivedApproved.Load() {
			t.Error("serving CSR was already approved when it was created")
		}
		if signer.signerMismatch.Load() {
			t.Errorf("signer %q", signer.sawSigner.Load())
		}
		assertStoreLayout(t, dir, ServerPairPrefix)
	})
}

func TestClientCertificateIssued(t *testing.T) {
	withClock(t, func(t *testing.T) {
		dir := t.TempDir()
		cs := fake.NewSimpleClientset()
		signer := newCSRSigner(t, cs, testCertLife, certificatesv1.KubeAPIServerClientKubeletSignerName)
		rot := startClient(t, dir, cs)
		defer stopRotation(rot)
		synctest.Wait()

		got := rot.Current()
		if got == nil || got.Leaf == nil {
			t.Fatalf("no client certificate, creates %d", signer.creates.Load())
		}
		if got.Leaf.Subject.CommonName != "system:node:mini" {
			t.Errorf("cn %q", got.Leaf.Subject.CommonName)
		}
		if !hasOrg(got.Leaf, "system:nodes") {
			t.Errorf("orgs %v", got.Leaf.Subject.Organization)
		}
		if signer.signerMismatch.Load() {
			t.Errorf("signer %q", signer.sawSigner.Load())
		}
		var clientAuth bool
		for _, u := range got.Leaf.ExtKeyUsage {
			if u == x509.ExtKeyUsageClientAuth {
				clientAuth = true
			}
		}
		if !clientAuth {
			t.Errorf("eku %v, want client auth", got.Leaf.ExtKeyUsage)
		}
		assertStoreLayout(t, dir, ClientPairPrefix)
	})
}

func TestRenewalBetween70And90Percent(t *testing.T) {
	withClock(t, func(t *testing.T) {
		dir := t.TempDir()
		cs := fake.NewSimpleClientset()
		signer := newCSRSigner(t, cs, testCertLife, certificatesv1.KubeletServingSignerName)
		rot := startServing(t, dir, cs)
		defer stopRotation(rot)
		synctest.Wait()
		first := rot.Current()
		if first == nil || first.Leaf == nil {
			t.Fatal("no initial certificate")
		}
		serial := first.Leaf.SerialNumber.String()
		elapsed := advanceUntilCreates(t, signer, signer.creates.Load())
		if elapsed < 700*time.Second || elapsed > 910*time.Second {
			t.Fatalf("renewed at %s, want between 700s and 900s of a %s lifetime", elapsed, testCertLife)
		}
		synctest.Wait()
		next := rot.Current()
		if next == nil || next.Leaf == nil || next.Leaf.SerialNumber.String() == serial {
			t.Fatal("renewal did not replace the certificate")
		}
	})
}

func TestDeniedCSRKeepsCertificateAndBacksOff(t *testing.T) {
	withClock(t, func(t *testing.T) {
		dir := t.TempDir()
		cs := fake.NewSimpleClientset()
		signer := newCSRSigner(t, cs, testCertLife, certificatesv1.KubeletServingSignerName)
		rot := startServing(t, dir, cs)
		defer stopRotation(rot)
		synctest.Wait()
		first := rot.Current()
		if first == nil || first.Leaf == nil {
			t.Fatal("no initial certificate")
		}
		serial := first.Leaf.SerialNumber.String()
		before := pemBytes(t, dir, ServerPairPrefix)
		signer.deny.Store(true)
		base := signer.creates.Load()
		elapsed := advanceUntilCreates(t, signer, base)
		if elapsed < 700*time.Second || elapsed > 910*time.Second {
			t.Fatalf("denied renewal at %s, want the 70-90%% window", elapsed)
		}
		if got := signer.creates.Load(); got != base+1 {
			t.Fatalf("creates %d, want the first denied attempt only (%d)", got, base+1)
		}
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()
		if got := signer.creates.Load(); got != base+1 {
			t.Fatalf("retried after 500ms, creates %d", got)
		}
		time.Sleep(3 * time.Second)
		synctest.Wait()
		if got := signer.creates.Load(); got < base+2 {
			t.Fatalf("no backoff retry, creates %d", got)
		}
		cur := rot.Current()
		if cur == nil || cur.Leaf == nil || cur.Leaf.SerialNumber.String() != serial {
			t.Fatal("denied CSR replaced the certificate")
		}
		if after := pemBytes(t, dir, ServerPairPrefix); string(after) != string(before) {
			t.Fatal("denied CSR changed the cert on disk")
		}
	})
}

func TestOutageAtExpiryKeepsCertificate(t *testing.T) {
	withClock(t, func(t *testing.T) {
		dir := t.TempDir()
		cs := fake.NewSimpleClientset()
		signer := newCSRSigner(t, cs, testCertLife, certificatesv1.KubeAPIServerClientKubeletSignerName)
		rot := startClient(t, dir, cs)
		defer stopRotation(rot)
		synctest.Wait()
		first := rot.Current()
		if first == nil || first.Leaf == nil {
			t.Fatal("no initial certificate")
		}
		serial := first.Leaf.SerialNumber.String()
		if !rot.ServerHealthy() {
			t.Fatal("server unhealthy after issuance")
		}
		signer.down.Store(true)
		base := signer.creates.Load()
		elapsed := advanceUntilCreates(t, signer, base)
		if elapsed < 700*time.Second || elapsed > 910*time.Second {
			t.Fatalf("outage renewal at %s, want the 70-90%% window", elapsed)
		}
		synctest.Wait()
		if rot.ServerHealthy() {
			t.Fatal("server reported healthy while the apiserver was down")
		}
		cur := rot.Current()
		if cur == nil || cur.Leaf.SerialNumber.String() != serial {
			t.Fatal("outage before expiry dropped the certificate")
		}
		remain := testCertLife - elapsed + 5*time.Second
		time.Sleep(remain)
		synctest.Wait()
		if rot.Current() != nil {
			t.Fatal("expired certificate is still Current")
		}
		kept, err := rot.Retained()
		if err != nil || kept == nil || kept.Leaf == nil || kept.Leaf.SerialNumber.String() != serial {
			t.Fatalf("retained certificate after expiry: %v", err)
		}
		signer.down.Store(false)
		deadline := time.Now().Add(45 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(time.Second)
			synctest.Wait()
			if c := rot.Current(); c != nil && c.Leaf != nil && c.Leaf.SerialNumber.String() != serial {
				return
			}
		}
		t.Fatal("no certificate after the apiserver recovered")
	})
}

func startServing(t *testing.T, dir string, cs kubernetes.Interface) *Rotation {
	t.Helper()
	rot, err := StartServing(ServingOptions{
		Context:  context.Background(),
		NodeName: "mini",
		CertDir:  dir,
		Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeHostName, Address: "mini"},
			{Type: corev1.NodeInternalIP, Address: "10.0.0.8"},
		},
		ClientsetFn: func(*tls.Certificate) (kubernetes.Interface, error) { return cs, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return rot
}

func startClient(t *testing.T, dir string, cs kubernetes.Interface) *Rotation {
	t.Helper()
	rot, err := StartClient(ClientOptions{
		Context:     context.Background(),
		NodeName:    "mini",
		CertDir:     dir,
		ClientsetFn: func(*tls.Certificate) (kubernetes.Interface, error) { return cs, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return rot
}

// advanceUntilCreates sleeps until the signer has observed one new CSR.
// The clock jumps in one-second steps after 690s so the 70% floor is not
// crossed by a coarse step, and the call returns on the first new create
// rather than after later backoff retries.
func advanceUntilCreates(t *testing.T, signer *csrSigner, base int32) time.Duration {
	t.Helper()
	elapsed := time.Duration(0)
	for elapsed < 690*time.Second {
		time.Sleep(10 * time.Second)
		elapsed += 10 * time.Second
		synctest.Wait()
		if signer.creates.Load() != base {
			t.Fatalf("CSR at %s, before 70%% of the lifetime", elapsed)
		}
	}
	for elapsed < 950*time.Second {
		time.Sleep(time.Second)
		elapsed += time.Second
		synctest.Wait()
		if signer.creates.Load() != base {
			return elapsed
		}
	}
	t.Fatalf("no CSR by %s", elapsed)
	return elapsed
}

func stopRotation(r *Rotation) {
	r.Stop()
	// The manager's deadline sleep does not watch cancellation. Advance
	// past the next lifetime so the goroutine leaves the bubble.
	time.Sleep(2 * time.Hour)
	synctest.Wait()
}

func assertStoreLayout(t *testing.T, dir, prefix string) {
	t.Helper()
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("cert dir mode %o, want 0700", fi.Mode().Perm())
	}
	link := filepath.Join(dir, prefix+"-current.pem")
	lst, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if lst.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink", link)
	}
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	full := target
	if !filepath.IsAbs(full) {
		full = filepath.Join(dir, full)
	}
	rel, err := filepath.Rel(dir, filepath.Clean(full))
	if err != nil || rel == ".." || (len(rel) >= 2 && rel[0] == '.' && rel[1] == '.') {
		t.Fatalf("symlink target %s is outside %s", target, dir)
	}
	st, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("private key mode %o, want 0600", st.Mode().Perm())
	}
}

func pemBytes(t *testing.T, dir, prefix string) []byte {
	t.Helper()
	link := filepath.Join(dir, prefix+"-current.pem")
	b, err := os.ReadFile(link)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func hasOrg(c *x509.Certificate, org string) bool {
	for _, o := range c.Subject.Organization {
		if o == org {
			return true
		}
	}
	return false
}

func hasDNS(c *x509.Certificate, name string) bool {
	for _, n := range c.DNSNames {
		if n == name {
			return true
		}
	}
	return false
}

func hasIP(c *x509.Certificate, ip string) bool {
	want := net.ParseIP(ip)
	for _, got := range c.IPAddresses {
		if got.Equal(want) {
			return true
		}
	}
	return false
}

type csrSigner struct {
	cs              *fake.Clientset
	ca              *x509.Certificate
	caKey           *ecdsa.PrivateKey
	lifetime        time.Duration
	wantSigner      string
	creates         atomic.Int32
	deny            atomic.Bool
	down            atomic.Bool
	arrivedApproved atomic.Bool
	signerMismatch  atomic.Bool
	sawSigner       atomic.Value
}

func newCSRSigner(t *testing.T, cs *fake.Clientset, life time.Duration, want string) *csrSigner {
	t.Helper()
	ca, key := testCA(t)
	s := &csrSigner{cs: cs, ca: ca, caKey: key, lifetime: life, wantSigner: want}
	s.sawSigner.Store("")
	cs.PrependReactor("create", "certificatesigningrequests", s.react)
	return s
}

func (s *csrSigner) react(action clienttesting.Action) (bool, runtime.Object, error) {
	n := s.creates.Add(1)
	if s.down.Load() {
		return true, nil, apierrors.NewServiceUnavailable("apiserver down")
	}
	csr := action.(clienttesting.CreateAction).GetObject().(*certificatesv1.CertificateSigningRequest)
	if len(csr.Status.Conditions) != 0 || len(csr.Status.Certificate) != 0 {
		s.arrivedApproved.Store(true)
	}
	s.sawSigner.Store(csr.Spec.SignerName)
	if csr.Spec.SignerName != s.wantSigner {
		s.signerMismatch.Store(true)
	}
	csr.Name = fmt.Sprintf("csr-%d", n)
	csr.UID = types.UID(csr.Name)
	for i := int32(1); i < n; i++ {
		_ = s.cs.Tracker().Delete(testCSRGVR, "", fmt.Sprintf("csr-%d", i))
	}
	if s.deny.Load() {
		csr.Status.Conditions = []certificatesv1.CertificateSigningRequestCondition{{
			Type:   certificatesv1.CertificateDenied,
			Status: corev1.ConditionTrue,
			Reason: "Denied",
		}}
		return false, nil, nil
	}
	block, _ := pem.Decode(csr.Spec.Request)
	if block == nil {
		return true, nil, fmt.Errorf("csr is not PEM")
	}
	req, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return true, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return true, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      req.Subject,
		DNSNames:     req.DNSNames,
		IPAddresses:  req.IPAddresses,
		NotBefore:    now,
		NotAfter:     now.Add(s.lifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  ekus(csr.Spec.Usages),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, s.ca, req.PublicKey, s.caKey)
	if err != nil {
		return true, nil, err
	}
	csr.Status.Certificate = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	csr.Status.Conditions = []certificatesv1.CertificateSigningRequestCondition{{
		Type:   certificatesv1.CertificateApproved,
		Status: corev1.ConditionTrue,
	}}
	return false, nil, nil
}

func ekus(usages []certificatesv1.KeyUsage) []x509.ExtKeyUsage {
	var out []x509.ExtKeyUsage
	for _, u := range usages {
		switch u {
		case certificatesv1.UsageServerAuth:
			out = append(out, x509.ExtKeyUsageServerAuth)
		case certificatesv1.UsageClientAuth:
			out = append(out, x509.ExtKeyUsageClientAuth)
		}
	}
	return out
}

func testCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return parsed, key
}
