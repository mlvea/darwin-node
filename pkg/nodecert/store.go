package nodecert

import (
	"crypto/tls"
	"fmt"

	"k8s.io/client-go/util/certificate"
)

// secureStore is a certificate.FileStore that keeps the directory at 0700
// and key material at 0600 after every write. client-go's file store
// creates the directory with 0755 when it is missing and writes the PEM
// at 0600. PrepareDir runs first; this wrapper puts the mode back if a
// write widens it, and refuses a symlink that leaves the directory.
type secureStore struct {
	inner certificate.FileStore
	dir   string
}

func newSecureStore(dir, pairPrefix, certFile, keyFile string) (secureStore, error) {
	if err := PrepareDir(dir); err != nil {
		return secureStore{}, err
	}
	inner, err := certificate.NewFileStore(pairPrefix, dir, dir, certFile, keyFile)
	if err != nil {
		return secureStore{}, err
	}
	s := secureStore{inner: inner, dir: dir}
	if err := s.tighten(); err != nil {
		return secureStore{}, err
	}
	return s, nil
}

func (s secureStore) Current() (*tls.Certificate, error) {
	return s.inner.Current()
}

func (s secureStore) Update(certPEM, keyPEM []byte) (*tls.Certificate, error) {
	out, err := s.inner.Update(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	if err := s.tighten(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s secureStore) tighten() error {
	if err := linksInside(s.dir); err != nil {
		return err
	}
	fd, err := unixOpenDir(s.dir)
	if err != nil {
		return err
	}
	defer unixClose(fd)
	if err := fchmod(fd, 0o700); err != nil {
		return fmt.Errorf("chmod cert dir: %w", err)
	}
	return tightenFiles(s.dir)
}
