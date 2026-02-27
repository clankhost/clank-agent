package grpcclient

import (
	"context"
	"fmt"
	"io"
	"log"

	clankv1 "github.com/anaremore/clank/apps/agent/gen/clank/v1"
	"github.com/anaremore/clank/apps/agent/internal/sysinfo"
	"google.golang.org/grpc"
)

// ConnectStream is a bidirectional stream for the Connect RPC.
type ConnectStream = grpc.BidiStreamingClient[clankv1.AgentMessage, clankv1.ControlMessage]

// OpenConnectStream opens the AgentControlService.Connect bidi stream.
func OpenConnectStream(ctx context.Context, conn *grpc.ClientConn) (ConnectStream, error) {
	client := clankv1.NewAgentControlServiceClient(conn)
	stream, err := client.Connect(ctx)
	if err != nil {
		return nil, fmt.Errorf("opening Connect stream: %w", err)
	}
	return stream, nil
}

// SendHeartbeat sends a heartbeat message on the stream.
func SendHeartbeat(stream ConnectStream, info *sysinfo.Info) error {
	msg := &clankv1.AgentMessage{
		Payload: &clankv1.AgentMessage_Heartbeat{
			Heartbeat: &clankv1.Heartbeat{
				SystemInfo: &clankv1.SystemInfo{
					Hostname:      info.Hostname,
					Os:            info.OS,
					Arch:          info.Arch,
					CpuCores:      info.CPUCores,
					MemoryBytes:   info.MemoryBytes,
					DockerVersion: info.DockerVersion,
					AgentVersion:  info.AgentVersion,
				},
			},
		},
	}
	return stream.Send(msg)
}

// ReceiveCommands listens for ControlMessages and logs them.
// Returns when the stream closes or an error occurs.
func ReceiveCommands(stream ConnectStream) error {
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("receiving command: %w", err)
		}

		switch p := msg.GetPayload().(type) {
		case *clankv1.ControlMessage_Ping:
			log.Println("Received ping")
		case *clankv1.ControlMessage_Deploy:
			log.Printf("Received deploy command for deployment %s (stub)", p.Deploy.GetDeploymentId())
		default:
			log.Printf("Received unknown control message type: %T", p)
		}
	}
}
