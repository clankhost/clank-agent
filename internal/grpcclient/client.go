package grpcclient

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
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
		tlsCfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
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

// tunnelTarget ensures the endpoint has a port (default 443 for TLS).
func tunnelTarget(endpoint string) string {
	if !strings.Contains(endpoint, ":") {
		return endpoint + ":443"
	}
	return endpoint
}

// tunnelHost extracts the hostname (without port) from an endpoint string.
func tunnelHost(endpoint string) string {
	if idx := strings.Index(endpoint, ":"); idx > 0 {
		return endpoint[:idx]
	}
	return endpoint
}

// DialTunnel connects via standard TLS (system CA pool) for tunnel-mode
// enrollment through Cloudflare Tunnel.
func DialTunnel(endpoint string) (*grpc.ClientConn, error) {
	target := tunnelTarget(endpoint)
	host := tunnelHost(endpoint)
	creds := credentials.NewTLS(&tls.Config{ServerName: host})
	conn, err := grpc.NewClient(
		"dns:///"+target,
		grpc.WithTransportCredentials(creds),
		grpc.WithAuthority(host),
	)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", target, err)
	}
	return conn, nil
}

// DialTunnelWithAuth connects via standard TLS with a JWT bearer token
// injected into every RPC call. Used by tunnel-mode agents for the
// control connection (no mTLS, Cloudflare terminates TLS).
func DialTunnelWithAuth(endpoint, authToken string) (*grpc.ClientConn, error) {
	target := tunnelTarget(endpoint)
	host := tunnelHost(endpoint)
	creds := credentials.NewTLS(&tls.Config{ServerName: host})
	conn, err := grpc.NewClient(
		"dns:///"+target,
		grpc.WithTransportCredentials(creds),
		grpc.WithAuthority(host),
		grpc.WithPerRPCCredentials(&jwtCredentials{token: authToken}),
	)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", target, err)
	}
	return conn, nil
}

// jwtCredentials implements grpc.PerRPCCredentials to inject a JWT bearer
// token into the "authorization" metadata of every RPC call.
type jwtCredentials struct {
	token string
}

func (j *jwtCredentials) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	return map[string]string{
		"authorization": "Bearer " + j.token,
	}, nil
}

func (j *jwtCredentials) RequireTransportSecurity() bool {
	return true
}

// DialPlaintext connects without TLS. Only for local development.
func DialPlaintext(endpoint string) (*grpc.ClientConn, error) {
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", endpoint, err)
	}
	return conn, nil
}
