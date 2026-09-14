package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/clankhost/clank-agent/internal/certs"
)

func recoveryTestCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(100),
		Subject:               pkix.Name{CommonName: "recovery test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
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

func recoveryClientCert(
	t *testing.T,
	ca *x509.Certificate,
	caKey *ecdsa.PrivateKey,
	publicKey *ecdsa.PublicKey,
	serverID string,
	notAfter time.Time,
) []byte {
	t.Helper()
	serverURI, err := url.Parse("clank://server/" + serverID)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "recovery agent"},
		NotBefore:    time.Now().Add(-48 * time.Hour),
		NotAfter:     notAfter,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{serverURI},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, publicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func recoveryPrivateKey(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func TestExpiredCertificateRecoversOverHTTPSAndPreservesIdentity(t *testing.T) {
	configDir := t.TempDir()
	serverID := "11111111-1111-1111-1111-111111111111"
	ca, caKey, caPEM := recoveryTestCA(t)
	oldKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"client.crt": recoveryClientCert(t, ca, caKey, &oldKey.PublicKey, serverID, time.Now().Add(-time.Hour)),
		"client.key": recoveryPrivateKey(t, oldKey),
		"ca.crt":     caPEM,
	} {
		if err := os.WriteFile(filepath.Join(configDir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}

	const oldRenewal = "old-renewal-token-with-enough-entropy"
	const newRenewal = "new-renewal-token-with-enough-entropy"
	expiresAt := time.Now().Add(90 * 24 * time.Hour).Truncate(time.Second)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		var request struct {
			ServerID     string `json:"server_id"`
			RenewalToken string `json:"renewal_token"`
			RequestID    string `json:"request_id"`
			CSRPem       string `json:"csr_pem"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.ServerID != serverID || request.RenewalToken != oldRenewal {
			t.Fatal("recovery request did not preserve the enrolled identity")
		}
		csrPEM, err := base64.StdEncoding.DecodeString(request.CSRPem)
		if err != nil {
			t.Fatal(err)
		}
		block, _ := pem.Decode(csrPEM)
		if block == nil {
			t.Fatal("missing CSR PEM")
		}
		csr, err := x509.ParseCertificateRequest(block.Bytes)
		if err != nil || csr.CheckSignature() != nil {
			t.Fatalf("invalid CSR: %v", err)
		}
		certPEM := recoveryClientCert(
			t,
			ca,
			caKey,
			csr.PublicKey.(*ecdsa.PublicKey),
			serverID,
			expiresAt,
		)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"request_id":              request.RequestID,
			"client_cert":             base64.StdEncoding.EncodeToString(certPEM),
			"ca_cert":                 base64.StdEncoding.EncodeToString(caPEM),
			"auth_token":              "unused-in-mtls-mode",
			"renewal_token":           newRenewal,
			"credential_expires_unix": expiresAt.Unix(),
			"auth_generation":         2,
		}); err != nil {
			t.Fatal(err)
		}
	}))
	defer server.Close()

	originalTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	defer func() { http.DefaultTransport = originalTransport }()

	cfg := &Config{
		ServerID:        serverID,
		GRPCEndpoint:    "grpc.example:443",
		CertDir:         configDir,
		AuthMode:        "mtls",
		RenewalToken:    oldRenewal,
		RenewalEndpoint: server.URL,
		AuthGeneration:  1,
		RenewalStatus:   "failed",
	}
	agent := &Agent{
		cfg:       cfg,
		cfgMu:     &sync.RWMutex{},
		cfgDir:    configDir,
		certStore: certs.NewStore(configDir),
	}

	if err := agent.recoverCredentials(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if cfg.ServerID != serverID || cfg.GRPCEndpoint != "grpc.example:443" {
		t.Fatal("recovery changed the server binding")
	}
	if cfg.RenewalToken != newRenewal || cfg.AuthGeneration != 2 || cfg.RenewalStatus != "healthy" {
		t.Fatalf("credential state was not promoted: %+v", cfg)
	}
	activeCertPath, _, _ := agent.certStore.ActivePaths()
	if activeCertPath == filepath.Join(configDir, "client.crt") {
		t.Fatal("legacy expired certificate is still active")
	}
	activePEM, err := os.ReadFile(activeCertPath)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(activePEM)
	activeCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil || time.Until(activeCert.NotAfter) < 89*24*time.Hour {
		t.Fatalf("renewed certificate was not activated: %v", err)
	}
}
