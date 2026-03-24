# Clank Agent

[![CI](https://github.com/clankhost/clank-agent/actions/workflows/ci.yml/badge.svg)](https://github.com/clankhost/clank-agent/actions/workflows/ci.yml)

Lightweight agent for [Clank](https://clank.host), a self-hosted platform for deploying containerized applications. The agent runs on your servers and manages Docker containers, networking, builds, and deployments.

## Overview

The Clank Agent is a single Go binary that runs on each server in your fleet. It connects outbound to the Clank control plane via gRPC, requiring no inbound ports or firewall changes. The agent handles:

- Container lifecycle management (deploy, start, stop, restart)
- Docker image builds from source
- Health check monitoring
- Log and metric streaming
- Traefik reverse proxy configuration
- SSL/TLS certificate management
- Automatic self-updates
- Backup creation and management

## Install

Register a server in the Clank dashboard or CLI, then run the install command provided:

```sh
curl -fsSL https://clank.host/install.sh | sh -s -- --token <enrollment-token> --api <control-plane-url>
```

Or via the CLI:

```sh
clank servers add "my-server"
# Follow the install command in the output
```

## Requirements

- Linux (amd64 or arm64)
- Docker Engine 20.10+
- Outbound HTTPS access to the control plane

## Architecture

```
┌─────────────────────────────────┐
│         Control Plane           │
│  (API + Worker + gRPC Server)   │
└──────────────┬──────────────────┘
               │ gRPC (outbound)
┌──────────────┴──────────────────┐
│          Clank Agent            │
│  ┌───────────┐  ┌────────────┐  │
│  │  Builder  │  │  Deployer  │  │
│  └───────────┘  └────────────┘  │
│  ┌───────────┐  ┌────────────┐  │
│  │   Logs    │  │  Metrics   │  │
│  └───────────┘  └────────────┘  │
│  ┌───────────┐  ┌────────────┐  │
│  │ Endpoints │  │  Backups   │  │
│  └───────────┘  └────────────┘  │
└──────────────┬──────────────────┘
               │
┌──────────────┴──────────────────┐
│       Docker Engine             │
│  (containers, images, volumes)  │
└─────────────────────────────────┘
```

## Commands

```sh
# Run the agent (typically managed by systemd)
clank-agent run

# Check agent status
clank-agent status

# Enroll with a control plane
clank-agent enroll --token <token> --api <url>

# Diagnostics
clank-agent doctor

# Uninstall
clank-agent uninstall
```

## Configuration

The agent reads configuration from `/etc/clank-agent/config.yaml` (or `~/.clank-agent/config.yaml`). Configuration is set automatically during enrollment.

## Systemd Service

The install script creates a systemd service at `/etc/systemd/system/clank-agent.service`. Manage it with:

```sh
sudo systemctl status clank-agent
sudo systemctl restart clank-agent
sudo journalctl -u clank-agent -f
```

## Build from Source

```sh
git clone https://github.com/clankhost/clank-agent.git
cd clank-agent
go build -o clank-agent .
```

## Documentation

Full documentation is available at [docs.clank.host](https://docs.clank.host).

## License

Apache License 2.0. See [LICENSE](LICENSE) for details.
