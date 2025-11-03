# tkn-runner

> Run Tekton Pipelines locally without Kubernetes

**tkn-runner** is a lightweight tool that lets you run [Tekton Pipelines](https://tekton.dev) on your local machine using Podman, without requiring a full Kubernetes cluster. It combines a minimal k3s control plane with Virtual Kubelet to execute Tekton TaskRuns and PipelineRuns using local container runtimes.

## Why tkn-runner?

Tekton Pipelines is a powerful Kubernetes-native CI/CD framework, but it requires a full Kubernetes cluster to run. Setting up Kubernetes locally (minikube, kind, etc.) can be resource-intensive and complex for development and testing.

**tkn-runner** solves this by:
- Providing a single binary that bundles everything you need
- Using k3s (lightweight Kubernetes) for the control plane
- Running containers via Podman instead of requiring worker nodes
- Keeping resource usage minimal while maintaining full Tekton compatibility

## Features

- **Full Tekton Support**: Run TaskRuns and PipelineRuns with unmodified Tekton code
- **Lightweight**: Single binary with minimal resource footprint
- **No Kubernetes Cluster Required**: Uses k3s control plane + Virtual Kubelet + Podman
- **Cross-Platform**: macOS (Intel & Apple Silicon) and Linux support
- **Service Accounts**: Full support for Kubernetes service account tokens
- **Volumes**: emptyDir, downwardAPI, serviceAccount volumes
- **Results**: Extract and use task results
- **Standard Tools**: Use `kubectl` and `tkn` CLI as normal

## Architecture

```
┌─────────────────────────────────────────────────┐
│                 tkn-runner                      │
├─────────────────────────────────────────────────┤
│                                                 │
│  ┌──────────────┐      ┌──────────────────┐   │
│  │ k3s Control  │      │ Virtual Kubelet  │   │
│  │   Plane      │─────▶│    Provider      │   │
│  │              │      │                  │   │
│  │ • API Server │      │ • Pod Intercept  │   │
│  │ • etcd       │      │ • K8s→Podman     │   │
│  │ • Scheduler  │      │ • Status Sync    │   │
│  └──────────────┘      └──────────────────┘   │
│         │                       │              │
│         ▼                       ▼              │
│  ┌──────────────┐      ┌──────────────────┐   │
│  │   Tekton     │      │     Podman       │   │
│  │  Controller  │      │   Containers     │   │
│  └──────────────┘      └──────────────────┘   │
│                                                 │
└─────────────────────────────────────────────────┘
```

**How it works:**
1. k3s provides the Kubernetes API and runs the Tekton Pipeline Controller
2. Virtual Kubelet registers a fake node and intercepts Pod scheduling
3. When Tekton creates Pods, Virtual Kubelet translates them to Podman containers
4. Containers execute on your host, and status syncs back to Tekton

## Prerequisites

- **Podman**: Container runtime (install via `brew install podman` on macOS or your package manager on Linux)
- **Go 1.25+**: For building from source
- **kubectl** (optional): For interacting with the cluster
- **tkn CLI** (optional): For Tekton-specific commands

## Installation

### Build from Source

```bash
git clone https://github.com/waveywaves/tkn-runner.git
cd tkn-runner
go build -o tkn-runner ./cmd/tkn-runner
```

## Quick Start

### 1. Start tkn-runner

```bash
./tkn-runner start
```

This will:
- Start the k3s control plane
- Install Tekton CRDs and controllers
- Start the Virtual Kubelet provider
- Print the kubeconfig location

### 2. Set your kubeconfig

```bash
export KUBECONFIG=~/.tkn-runner/k3s.yaml
```

### 3. Run a TaskRun

Create a simple TaskRun:

```yaml
# hello-world.yaml
apiVersion: tekton.dev/v1beta1
kind: TaskRun
metadata:
  name: hello-world-taskrun
spec:
  taskSpec:
    steps:
      - name: hello
        image: ubuntu
        script: |
          #!/bin/bash
          echo "Hello World from Tekton TaskRun!"
```

Apply and watch:

```bash
kubectl apply -f hello-world.yaml

# Check status
kubectl get taskrun

# View logs
tkn taskrun logs hello-world-taskrun -f
```

### 4. Clean up

```bash
./tkn-runner delete
```

This removes all tkn-runner resources including k3s data, Podman networks, and containers.

## Usage

### Available Commands

```bash
# Start the tkn-runner environment
./tkn-runner start

# Delete all tkn-runner resources
./tkn-runner delete
```

### Working with TaskRuns

Once tkn-runner is running, you can use standard Kubernetes and Tekton tools:

```bash
# Apply TaskRuns or PipelineRuns
kubectl apply -f taskrun.yaml

# List TaskRuns
kubectl get taskrun
tkn taskrun list

# View TaskRun details
tkn taskrun describe hello-world-taskrun

# Stream logs
tkn taskrun logs hello-world-taskrun -f

# Delete a TaskRun
kubectl delete taskrun hello-world-taskrun
```

## Examples

More examples are available in the [`examples/`](./examples) directory:
- Simple TaskRun: [`examples/simple-taskrun.yaml`](./examples/simple-taskrun.yaml)
- Test TaskRun: [`test-taskrun.yaml`](./test-taskrun.yaml)

## Platform Support

### macOS
- **Status**: Fully supported and tested
- **Versions**: Ventura, Sonoma, Sequoia
- **Architectures**: Intel (x86_64) and Apple Silicon (arm64)
- **Networking**: k3s runs in Podman container with Podman networking

### Linux
- **Status**: In development (Phase 1 - CNI implementation in progress)
- **Distributions**: Ubuntu 22.04+, Fedora, RHEL
- **Networking**: k3s runs natively with Flannel CNI

## Current Limitations

This project is under active development. Current limitations include:

- Linux Flannel CNI integration is in progress
- ConfigMap/Secret environment variable references not yet supported
- Log streaming via `kubectl logs` not yet implemented
- No systemd service mode

## Architecture Documentation

Detailed architecture documentation is available in:
- [`designs/v2-virtual-kubelet.md`](./designs/v2-virtual-kubelet.md) - Complete architectural overview and design decisions

## Project Structure

```
tkn-runner/
├── cmd/tkn-runner/          # Main CLI application
├── pkg/
│   ├── config/              # Configuration management
│   ├── controlplane/k3s/    # k3s integration (macOS & Linux)
│   ├── virtualkubelet/      # Virtual Kubelet provider & Podman executor
│   └── manifests/           # Embedded Tekton CRDs & controller
├── examples/                # Sample TaskRun YAML files
├── designs/                 # Architecture documentation
└── docs/                    # Implementation notes
```

## How It's Different

**vs. Full Kubernetes:**
- Much lighter weight (single binary vs. multi-node cluster)
- Faster startup time
- Lower resource usage
- Perfect for local development and testing

**vs. Tekton on minikube/kind:**
- No VM or nested containers required on macOS
- Direct Podman integration
- Simpler networking model
- Easier to debug and understand

**Key Design Decision:**
- Uses **Virtual Kubelet pattern** instead of custom controllers
- This allows running **unmodified Tekton code** with automatic feature inheritance
- Platform-specific optimizations (macOS vs Linux) for best performance

## Contributing

Contributions are welcome! Current priorities include:
- Linux Flannel CNI implementation (Phase 1 - HIGH PRIORITY)
- Testing and validation
- Documentation improvements

## License

This project includes the Tekton Pipeline codebase. See individual components for their respective licenses.

## Acknowledgments

- [Tekton Pipelines](https://tekton.dev) - The Kubernetes-native CI/CD framework
- [Virtual Kubelet](https://virtual-kubelet.io) - The pattern that makes this possible
- [k3s](https://k3s.io) - Lightweight Kubernetes
- [Podman](https://podman.io) - Daemonless container runtime
