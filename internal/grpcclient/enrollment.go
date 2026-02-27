package grpcclient

import (
	"context"
	"fmt"
	"time"

	clankv1 "github.com/anaremore/clank/apps/agent/gen/clank/v1"
	"github.com/anaremore/clank/apps/agent/internal/sysinfo"
)

// Enroll calls the AgentEnrollmentService.Enroll RPC.
// Uses InsecureSkipVerify since the agent has no CA cert yet.
func Enroll(endpoint, token string, info *sysinfo.Info) (*clankv1.EnrollResponse, error) {
	conn, err := DialInsecure(endpoint)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

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
