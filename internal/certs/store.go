package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"google.golang.org/grpc/credentials"
)

const (
	clientCertFile = "client.crt"
	clientKeyFile  = "client.key"
	caCertFile     = "ca.crt"
	bundlesDir     = "credential-bundles"
	currentFile    = "current-credentials"
	csrFile        = "rotation.csr"
)

var bundleIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Store manages versioned certificate bundles on disk. Existing installations
// with only the three legacy files continue to work until their first rotation.
type Store struct {
	dir       string
	writeFile func(string, []byte, os.FileMode) error
}

// Activation records the previous pointer for rollback after a failed cutover.
type Activation struct {
	PreviousBundle string
	HadPointer     bool
}

func NewStore(dir string) *Store { return &Store{dir: dir, writeFile: atomicWrite} }

// Save validates and atomically activates an enrollment credential bundle.
func (s *Store) Save(clientCert, clientKey, caCert []byte) error {
	if err := validateBundle(clientCert, clientKey, caCert, ""); err != nil {
		return err
	}
	id, err := randomBundleID("enrollment-")
	if err != nil {
		return err
	}
	if err := s.writeCompleteBundle(id, clientCert, clientKey, caCert); err != nil {
		return err
	}
	if err := s.writeFile(filepath.Join(s.dir, currentFile), []byte(id+"\n"), 0600); err != nil {
		_ = os.RemoveAll(s.bundlePath(id))
		return fmt.Errorf("activating enrollment credentials: %w", err)
	}
	return nil
}

// PrepareRotation creates or reloads a locally generated P-256 key and CSR.
// The private key never leaves the agent.
func (s *Store) PrepareRotation(requestID, serverID string) ([]byte, error) {
	if !bundleIDPattern.MatchString(requestID) {
		return nil, fmt.Errorf("invalid rotation request ID")
	}
	bundlePath := s.bundlePath(requestID)
	if existing, err := os.ReadFile(filepath.Join(bundlePath, csrFile)); err == nil {
		return existing, nil
	}
	if err := os.MkdirAll(bundlePath, 0700); err != nil {
		return nil, fmt.Errorf("creating rotation bundle: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating rotation key: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encoding rotation key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	serverURI, _ := url.Parse("clank://server/" + serverID)
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "clank-agent-" + serverID},
		URIs:    []*url.URL{serverURI},
	}, key)
	if err != nil {
		return nil, fmt.Errorf("creating certificate request: %w", err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
	if err := s.writeFile(filepath.Join(bundlePath, clientKeyFile), keyPEM, 0600); err != nil {
		return nil, fmt.Errorf("saving rotation key: %w", err)
	}
	if err := s.writeFile(filepath.Join(bundlePath, csrFile), csrPEM, 0600); err != nil {
		return nil, fmt.Errorf("saving certificate request: %w", err)
	}
	return csrPEM, nil
}

// StageRotation validates the signed certificate against the local key, CA,
// and expected server identity before writing it.
func (s *Store) StageRotation(requestID, serverID string, clientCert, caCert []byte) error {
	if !bundleIDPattern.MatchString(requestID) {
		return fmt.Errorf("invalid rotation request ID")
	}
	key, err := os.ReadFile(filepath.Join(s.bundlePath(requestID), clientKeyFile))
	if err != nil {
		return fmt.Errorf("reading pending rotation key: %w", err)
	}
	if err := validateBundle(clientCert, key, caCert, serverID); err != nil {
		return err
	}
	if err := s.writeFile(filepath.Join(s.bundlePath(requestID), clientCertFile), clientCert, 0600); err != nil {
		return fmt.Errorf("staging client certificate: %w", err)
	}
	if err := s.writeFile(filepath.Join(s.bundlePath(requestID), caCertFile), caCert, 0600); err != nil {
		return fmt.Errorf("staging CA certificate: %w", err)
	}
	return nil
}

// ActivateRotation switches the active-bundle pointer atomically.
func (s *Store) ActivateRotation(requestID string) (Activation, error) {
	if !bundleIDPattern.MatchString(requestID) {
		return Activation{}, fmt.Errorf("invalid rotation request ID")
	}
	if !completeBundle(s.bundlePath(requestID)) {
		return Activation{}, fmt.Errorf("rotation bundle is incomplete")
	}
	previous, hadPointer := s.readPointer()
	if err := s.writeFile(filepath.Join(s.dir, currentFile), []byte(requestID+"\n"), 0600); err != nil {
		return Activation{}, fmt.Errorf("activating rotation bundle: %w", err)
	}
	return Activation{PreviousBundle: previous, HadPointer: hadPointer}, nil
}

func (s *Store) Rollback(activation Activation) error {
	path := filepath.Join(s.dir, currentFile)
	if !activation.HadPointer {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("restoring legacy credentials: %w", err)
		}
		return nil
	}
	if !bundleIDPattern.MatchString(activation.PreviousBundle) {
		return fmt.Errorf("previous credential pointer is invalid")
	}
	return s.writeFile(path, []byte(activation.PreviousBundle+"\n"), 0600)
}

func (s *Store) DiscardRotation(requestID string) error {
	if !bundleIDPattern.MatchString(requestID) {
		return fmt.Errorf("invalid rotation request ID")
	}
	active, _ := s.readPointer()
	if active == requestID {
		return fmt.Errorf("cannot discard active credential bundle")
	}
	return os.RemoveAll(s.bundlePath(requestID))
}

// CurrentPointer returns the managed bundle pointer, or false for a legacy
// three-file installation.
func (s *Store) CurrentPointer() (string, bool) { return s.readPointer() }

func (s *Store) Exists() bool {
	cert, key, ca := s.ActivePaths()
	return regularFile(cert) && regularFile(key) && regularFile(ca)
}

// ActivePaths resolves the pointer and falls back to the legacy paths.
func (s *Store) ActivePaths() (string, string, string) {
	if id, ok := s.readPointer(); ok {
		path := s.bundlePath(id)
		if completeBundle(path) {
			return filepath.Join(path, clientCertFile), filepath.Join(path, clientKeyFile), filepath.Join(path, caCertFile)
		}
	}
	return filepath.Join(s.dir, clientCertFile), filepath.Join(s.dir, clientKeyFile), filepath.Join(s.dir, caCertFile)
}

func (s *Store) TransportCredentials() (credentials.TransportCredentials, error) {
	certPath, keyPath, caPath := s.ActivePaths()
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("loading client cert/key: %w", err)
	}
	caCert, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("reading CA cert: %w", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to parse CA certificate")
	}
	return credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: caPool, MinVersion: tls.VersionTLS12}), nil
}

func (s *Store) bundlePath(id string) string { return filepath.Join(s.dir, bundlesDir, id) }

func (s *Store) readPointer() (string, bool) {
	data, err := os.ReadFile(filepath.Join(s.dir, currentFile))
	if err != nil {
		return "", false
	}
	id := strings.TrimSpace(string(data))
	if !bundleIDPattern.MatchString(id) {
		return "", false
	}
	return id, true
}

func (s *Store) writeCompleteBundle(id string, cert, key, ca []byte) error {
	path := s.bundlePath(id)
	if err := os.MkdirAll(path, 0700); err != nil {
		return fmt.Errorf("creating credential bundle: %w", err)
	}
	for name, data := range map[string][]byte{clientCertFile: cert, clientKeyFile: key, caCertFile: ca} {
		if err := s.writeFile(filepath.Join(path, name), data, 0600); err != nil {
			_ = os.RemoveAll(path)
			return fmt.Errorf("writing %s: %w", name, err)
		}
	}
	return nil
}

func validateBundle(certPEM, keyPEM, caPEM []byte, serverID string) error {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("credential certificate/key mismatch: %w", err)
	}
	if len(pair.Certificate) == 0 {
		return fmt.Errorf("credential certificate is empty")
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return fmt.Errorf("parsing client certificate: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return fmt.Errorf("parsing CA certificate")
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return fmt.Errorf("verifying client certificate: %w", err)
	}
	if time.Until(cert.NotAfter) < 24*time.Hour {
		return fmt.Errorf("new client certificate expires too soon")
	}
	if serverID != "" {
		expected := "clank://server/" + serverID
		matched := false
		for _, uri := range cert.URIs {
			if uri.String() == expected {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("client certificate has wrong server identity")
		}
	}
	return nil
}

func completeBundle(path string) bool {
	return regularFile(filepath.Join(path, clientCertFile)) && regularFile(filepath.Join(path, clientKeyFile)) && regularFile(filepath.Join(path, caCertFile))
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func randomBundleID(prefix string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating credential bundle ID: %w", err)
	}
	return prefix + hex.EncodeToString(b), nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".clank-credential-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
			return err
		}
		if retryErr := os.Rename(tmpPath, path); retryErr != nil {
			return retryErr
		}
	}
	ok = true
	return nil
}
