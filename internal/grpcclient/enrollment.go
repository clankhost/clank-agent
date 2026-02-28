package grpcclient

import (
	"context"
	"fmt"
	"time"

	clankv1 "github.com/anaremore/clank/apps/agent/gen/clank/v1"
	"github.com/anaremore/clank/apps/agent/internal/sysinfo"
	"google.golang.org/grpc"
)

// EnrollResponse is an alias for the proto enrollment response.
type EnrollResponse = clankv1.EnrollResponse

// Enroll calls the AgentEnrollmentService.Enroll RPC via direct connection.
// If caFingerprint is provided (format "sha256:<hex>"), the server certificate
// is verified against it to prevent MITM attacks during enrollment.
func Enroll(endpoint, token, caFingerprint string, info *sysinfo.Info) (*EnrollResponse, error) {
	conn, err := DialEnrollment(endpoint, caFingerprint)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	return callEnrollRPC(conn, token, info)
}

// EnrollTunnel calls the AgentEnrollmentService.Enroll RPC via Cloudflare
// Tunnel. Uses standard TLS (system CA pool) since CF terminates TLS at the
// edge and presents its own certificate.
func EnrollTunnel(endpoint, token string, info *sysinfo.Info) (*EnrollResponse, error) {
	conn, err := DialTunnel(endpoint)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	return callEnrollRPC(conn, token, info)
}

func callEnrollRPC(conn *grpc.ClientConn, token string, info *sysinfo.Info) (*EnrollResponse, error) {
	client := clankv1.NewAgentEnrollmentServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.Enroll(ctx, &clankv1.EnrollRequest{
		Token: token,
		SystemInfo: &clankv1.SystemInfo{
			Hostname:      info.Hostname,
			Os:            info.OS,
			Arch:          info.Arch,
			CpuCores:      info.CPUCores,
			MemoryBytes:   info.MemoryBytes,
			DockerVersion: info.DockerVersion,
			AgentVersion:  info.AgentVersion,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("enroll RPC: %w", err)
	}

	return resp, nil
}
