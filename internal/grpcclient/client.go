package grpcclient

import (
	"crypto/tls"
	"fmt"

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

// DialInsecure connects to the gRPC endpoint with server-only TLS
// (no client cert). Used during enrollment before the agent has credentials.
func DialInsecure(endpoint string) (*grpc.ClientConn, error) {
	tlsCfg := &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // Enrollment uses token auth; agent has no CA cert yet.
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
