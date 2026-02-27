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

// DeployHandler handles deploy commands from the control plane.
type DeployHandler func(ctx context.Context, stream ConnectStream, cmd *clankv1.DeployCommand)

// ContainerCommandHandler handles container lifecycle commands.
type ContainerCommandHandler func(ctx context.Context, stream ConnectStream, cmd *clankv1.ContainerCommand)

// CommandHandlers groups all command handler functions.
type CommandHandlers struct {
	OnDeploy           DeployHandler
	OnContainerCommand ContainerCommandHandler
}

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
func SendHeartbeat(stream ConnectStream, info *sysinfo.Info, containers []sysinfo.ContainerStatus) error {
	var protoContainers []*clankv1.ContainerStatus
	for _, c := range containers {
		protoContainers = append(protoContainers, &clankv1.ContainerStatus{
			ContainerId: c.ContainerID,
			Name:        c.Name,
			State:       c.State,
			Image:       c.Image,
		})
	}

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
				Containers: protoContainers,
			},
		},
	}
	return stream.Send(msg)
}

// ReceiveCommands listens for ControlMessages and dispatches to handlers.
// Returns when the stream closes or an error occurs.
func ReceiveCommands(ctx context.Context, stream ConnectStream, handlers CommandHandlers) error {
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
			log.Printf("Received deploy command for deployment %s", p.Deploy.GetDeploymentId())
			if handlers.OnDeploy != nil {
				go handlers.OnDeploy(ctx, stream, p.Deploy)
			}

		case *clankv1.ControlMessage_ContainerCmd:
			log.Printf("Received container command %s: %s", p.ContainerCmd.GetCommandId(), p.ContainerCmd.GetAction())
			if handlers.OnContainerCommand != nil {
				go handlers.OnContainerCommand(ctx, stream, p.ContainerCmd)
			}

		default:
			log.Printf("Received unknown control message type: %T", p)
		}
	}
}
