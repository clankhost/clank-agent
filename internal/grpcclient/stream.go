package grpcclient

import (
	"context"
	"fmt"
	"io"
	"log"

	clankv1 "github.com/clankhost/clank-agent/gen/clank/v1"
	"github.com/clankhost/clank-agent/internal/sysinfo"
	"google.golang.org/grpc"
)

// allIPs combines LAN IPs and public IP into a single slice for the
// heartbeat proto. The API separates them on receive (public → server.public_ip,
// private → server.lan_ips). This avoids a proto schema change.
func allIPs(info *sysinfo.Info) []string {
	ips := make([]string, 0, len(info.LANIPs)+1)
	ips = append(ips, info.LANIPs...)
	if info.PublicIP != "" {
		ips = append(ips, info.PublicIP)
	}
	return ips
}

// ConnectStream is a bidirectional stream for the Connect RPC.
type ConnectStream = grpc.BidiStreamingClient[clankv1.AgentMessage, clankv1.ControlMessage]

// DeployHandler handles deploy commands from the control plane.
type DeployHandler func(ctx context.Context, stream ConnectStream, cmd *clankv1.DeployCommand)

// ContainerCommandHandler handles container lifecycle commands.
type ContainerCommandHandler func(ctx context.Context, stream ConnectStream, cmd *clankv1.ContainerCommand)

// TunnelConfigHandler handles tunnel configuration from the control plane.
type TunnelConfigHandler func(ctx context.Context, cfg *clankv1.TunnelConfig)

// UpdateHandler handles self-update commands from the control plane.
type UpdateHandler func(ctx context.Context, stream ConnectStream, cmd *clankv1.UpdateCommand)

// EndpointHandler handles endpoint management commands.
type EndpointHandler func(ctx context.Context, stream ConnectStream, cmd *clankv1.EndpointCommand)

// BackupHandler handles backup commands.
type BackupHandler func(ctx context.Context, stream ConnectStream, cmd *clankv1.BackupCommand)

// PushImageHandler handles push-to-registry commands.
type PushImageHandler func(ctx context.Context, stream ConnectStream, cmd *clankv1.PushImageCommand)

// CommandHandlers groups all command handler functions.
type CommandHandlers struct {
	OnDeploy           DeployHandler
	OnContainerCommand ContainerCommandHandler
	OnTunnelConfig     TunnelConfigHandler
	OnUpdate           UpdateHandler
	OnEndpoint         EndpointHandler
	OnBackup           BackupHandler
	OnPushImage        PushImageHandler
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
					Hostname:              info.Hostname,
					Os:                    info.OS,
					Arch:                  info.Arch,
					CpuCores:              info.CPUCores,
					MemoryBytes:           info.MemoryBytes,
					DockerVersion:         info.DockerVersion,
					AgentVersion:          info.AgentVersion,
					LanIps:                allIPs(info),
					TailscaleIp:           info.TailscaleIP,
					TailscaleHostname:     info.TailscaleHostname,
					TailscaleCliAvailable: info.TailscaleCLIAvailable,
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
				go func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("PANIC in deploy handler: %v", r)
						}
					}()
					handlers.OnDeploy(ctx, stream, p.Deploy)
				}()
			}

		case *clankv1.ControlMessage_ContainerCmd:
			log.Printf("Received container command %s: %s", p.ContainerCmd.GetCommandId(), p.ContainerCmd.GetAction())
			if handlers.OnContainerCommand != nil {
				go func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("PANIC in container command handler: %v", r)
						}
					}()
					handlers.OnContainerCommand(ctx, stream, p.ContainerCmd)
				}()
			}

		case *clankv1.ControlMessage_TunnelConfig:
			log.Printf("Received tunnel config (tunnel %s)", p.TunnelConfig.GetTunnelId())
			if handlers.OnTunnelConfig != nil {
				go func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("PANIC in tunnel config handler: %v", r)
						}
					}()
					handlers.OnTunnelConfig(ctx, p.TunnelConfig)
				}()
			}

		case *clankv1.ControlMessage_Update:
			log.Printf("Received update command: version %s", p.Update.GetVersion())
			if handlers.OnUpdate != nil {
				go func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("PANIC in update handler: %v", r)
						}
					}()
					handlers.OnUpdate(ctx, stream, p.Update)
				}()
			}

		case *clankv1.ControlMessage_EndpointCmd:
			log.Printf("Received endpoint command %s: %s %s", p.EndpointCmd.GetCommandId(), p.EndpointCmd.GetAction(), p.EndpointCmd.GetProvider())
			if handlers.OnEndpoint != nil {
				go func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("PANIC in endpoint handler: %v", r)
						}
					}()
					handlers.OnEndpoint(ctx, stream, p.EndpointCmd)
				}()
			}

		case *clankv1.ControlMessage_BackupCmd:
			log.Printf("Received backup command %s for service %s", p.BackupCmd.GetCommandId(), p.BackupCmd.GetServiceSlug())
			if handlers.OnBackup != nil {
				go func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("PANIC in backup handler: %v", r)
						}
					}()
					handlers.OnBackup(ctx, stream, p.BackupCmd)
				}()
			}

		case *clankv1.ControlMessage_PushImage:
			log.Printf("Received push image command for deployment %s", p.PushImage.GetDeploymentId())
			if handlers.OnPushImage != nil {
				go func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("PANIC in push image handler: %v", r)
						}
					}()
					handlers.OnPushImage(ctx, stream, p.PushImage)
				}()
			}

		default:
			log.Printf("Received unknown control message type: %T", p)
		}
	}
}
