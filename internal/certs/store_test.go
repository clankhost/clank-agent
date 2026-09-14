package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func issueClient(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, publicKey *ecdsa.PublicKey, serverID string) []byte {
	t.Helper()
	uri, _ := url.Parse("clank://server/" + serverID)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "agent"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(90 * 24 * time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, URIs: []*url.URL{uri},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, publicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func encodeKey(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func TestRotationActivationAndRollbackUseCompleteBundles(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	ca, caKey, caPEM := testCA(t)
	oldKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err := store.Save(issueClient(t, ca, caKey, &oldKey.PublicKey, "server-1"), encodeKey(t, oldKey), caPEM); err != nil {
		t.Fatal(err)
	}
	oldCertPath, _, _ := store.ActivePaths()

	csrPEM, err := store.PrepareRotation("rotation-test", "server-1")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(csrPEM)
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	newCert := issueClient(t, ca, caKey, csr.PublicKey.(*ecdsa.PublicKey), "server-1")
	if err := store.StageRotation("rotation-test", "server-1", newCert, caPEM); err != nil {
		t.Fatal(err)
	}
	activation, err := store.ActivateRotation("rotation-test")
	if err != nil {
		t.Fatal(err)
	}
	newCertPath, _, _ := store.ActivePaths()
	if newCertPath == oldCertPath {
		t.Fatal("rotation did not switch active bundle")
	}
	if err := store.Rollback(activation); err != nil {
		t.Fatal(err)
	}
	rolledBackPath, _, _ := store.ActivePaths()
	if rolledBackPath != oldCertPath {
		t.Fatalf("rollback selected %q, want %q", rolledBackPath, oldCertPath)
	}
}

func TestStageRotationRejectsWrongIdentityWithoutChangingActiveBundle(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	ca, caKey, caPEM := testCA(t)
	oldKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err := store.Save(issueClient(t, ca, caKey, &oldKey.PublicKey, "server-1"), encodeKey(t, oldKey), caPEM); err != nil {
		t.Fatal(err)
	}
	oldCertPath, _, _ := store.ActivePaths()
	csrPEM, err := store.PrepareRotation("rotation-wrong", "server-1")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(csrPEM)
	csr, _ := x509.ParseCertificateRequest(block.Bytes)
	wrongCert := issueClient(t, ca, caKey, csr.PublicKey.(*ecdsa.PublicKey), "server-2")
	if err := store.StageRotation("rotation-wrong", "server-1", wrongCert, caPEM); err == nil {
		t.Fatal("expected wrong identity to be rejected")
	}
	active, _, _ := store.ActivePaths()
	if active != oldCertPath {
		t.Fatal("failed staging changed active credentials")
	}
	if _, err := os.Stat(filepath.Join(dir, currentFile)); err != nil {
		t.Fatal("active pointer was lost")
	}
}

func TestActivationWriteFailureKeepsPreviousBundle(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	ca, caKey, caPEM := testCA(t)
	oldKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err := store.Save(issueClient(t, ca, caKey, &oldKey.PublicKey, "server-1"), encodeKey(t, oldKey), caPEM); err != nil {
		t.Fatal(err)
	}
	oldCertPath, _, _ := store.ActivePaths()

	csrPEM, err := store.PrepareRotation("rotation-write-failure", "server-1")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(csrPEM)
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	newCert := issueClient(t, ca, caKey, csr.PublicKey.(*ecdsa.PublicKey), "server-1")
	if err := store.StageRotation("rotation-write-failure", "server-1", newCert, caPEM); err != nil {
		t.Fatal(err)
	}
	store.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if filepath.Base(path) == currentFile {
			return errors.New("injected pointer failure")
		}
		return atomicWrite(path, data, mode)
	}

	if _, err := store.ActivateRotation("rotation-write-failure"); err == nil {
		t.Fatal("expected activation to fail")
	}
	activeCertPath, _, _ := store.ActivePaths()
	if activeCertPath != oldCertPath {
		t.Fatalf("activation failure selected %q, want %q", activeCertPath, oldCertPath)
	}
}
