package certs

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"

	"google.golang.org/grpc/credentials"
)

const (
	clientCertFile = "client.crt"
	clientKeyFile  = "client.key"
	caCertFile     = "ca.crt"
)

// Store manages certificate files on disk.
type Store struct {
	dir string
}

// NewStore creates a store rooted at the given directory.
func NewStore(dir string) *Store {
	return &Store{dir: dir}
}

// Save writes the certificate bundle to disk.
func (s *Store) Save(clientCert, clientKey, caCert []byte) error {
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return fmt.Errorf("creating cert dir: %w", err)
	}
	files := map[string][]byte{
		clientCertFile: clientCert,
		clientKeyFile:  clientKey,
		caCertFile:     caCert,
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(s.dir, name), data, 0600); err != nil {
			return fmt.Errorf("writing %s: %w", name, err)
		}
	}
	return nil
}

// Exists returns true if the cert files are present.
func (s *Store) Exists() bool {
	for _, name := range []string{clientCertFile, clientKeyFile, caCertFile} {
		if _, err := os.Stat(filepath.Join(s.dir, name)); err != nil {
			return false
		}
	}
	return true
}

// TransportCredentials returns gRPC mTLS transport credentials.
func (s *Store) TransportCredentials() (credentials.TransportCredentials, error) {
	cert, err := tls.LoadX509KeyPair(
		filepath.Join(s.dir, clientCertFile),
		filepath.Join(s.dir, clientKeyFile),
	)
	if err != nil {
		return nil, fmt.Errorf("loading client cert/key: %w", err)
	}

	caCert, err := os.ReadFile(filepath.Join(s.dir, caCertFile))
	if err != nil {
		return nil, fmt.Errorf("reading CA cert: %w", err)
	}

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to parse CA certificate")
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
	}

	return credentials.NewTLS(tlsCfg), nil
}
