package grpcclient

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// Dial connects to the gRPC endpoint with the given transport credentials.
func Dial(endpoint string, creds credentials.TransportCredentials) (*grpc.ClientConn, error) {
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", endpoint, err)
	}
	return conn, nil
}

// DialEnrollment connects to the gRPC endpoint for enrollment. If
// caFingerprint is provided (format "sha256:<hex>"), the server certificate's
// SHA-256 fingerprint is verified against it — preventing MITM during the
// enrollment window when the agent has no CA cert yet.
//
// If caFingerprint is empty, falls back to InsecureSkipVerify (backward compat).
func DialEnrollment(endpoint, caFingerprint string) (*grpc.ClientConn, error) {
	tlsCfg := &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // Enrollment: we verify fingerprint below if provided.
	}

	if caFingerprint != "" {
		expectedHex := strings.TrimPrefix(strings.ToLower(caFingerprint), "sha256:")
		tlsCfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*tls.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("server presented no certificate")
			}
			digest := sha256.Sum256(rawCerts[0])
			actualHex := hex.EncodeToString(digest[:])
			if actualHex != expectedHex {
				return fmt.Errorf(
					"server certificate fingerprint mismatch:\n  expected: sha256:%s\n  actual:   sha256:%s",
					expectedHex, actualHex,
				)
			}
			return nil
		}
	}

	creds := credentials.NewTLS(tlsCfg)
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", endpoint, err)
	}
	return conn, nil
}

// DialPlaintext connects without TLS. Only for local development.
func DialPlaintext(endpoint string) (*grpc.ClientConn, error) {
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", endpoint, err)
	}
	return conn, nil
}
