# tkn-runner

> Experimental local Tekton execution using k3s and a Podman adapter

**tkn-runner** is a proof of concept combining a k3s control plane, the standard
[Tekton Pipelines](https://tekton.dev) controller, and a custom virtual-node
provider that translates Pods into Podman container commands.

It is not a Kubernetes-free engine or a fully compatible local Tekton runtime.
The next iteration targets the full Tekton ecosystem running locally, with
one `tkn-runner` binary using an existing Podman installation. Real Kubernetes
workers and upstream Tekton components would run inside that local environment;
no remote cluster or separately installed cluster CLI would be required.

- [Current PoC assessment and architectural drift](designs/local-validation-assessment.md)
- [Current proposal: full local Tekton ecosystem](designs/v4-local-tekton-ecosystem.md)
- [Superseded Task-only proposal](designs/v3-local-validation.md)

The proposal is not implemented. The binary currently supports only `start`
and `delete`; the proposed environment and run commands are not available.

## Why tkn-runner?

Tekton Pipelines is a powerful Kubernetes-native CI/CD framework, but it requires a full Kubernetes cluster to run. Setting up Kubernetes locally (minikube, kind, etc.) can be resource-intensive and complex for development and testing.

The PoC explores retaining Tekton's controller while executing its Pods through
Podman. It moves compatibility work into the worker adapter rather than
eliminating Kubernetes semantics. Footprint and speed advantages over a warm
local cluster have not been established by benchmarks.

## Implemented building blocks

- k3s startup and embedded Tekton CRD/controller installation.
- Virtual-node registration and a Pod watcher.
- Podman execution of init containers and steps, with basic sidecar handling.
- Named volumes and partial service-account/downward-API file handling.
- Standard Kubernetes API access for inspecting resources.

These are implementation components, not a compatibility certification. Logs,
results, lifecycle status, workspace semantics, and platform behavior have
important gaps described in the assessment.

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
│  │ • State      │      │ • K8s→Podman     │   │
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
4. Containers execute in Podman's runtime (a Linux VM on macOS), and the adapter reports status back to Tekton

## Prerequisites

- **Podman**: Container runtime (install via `brew install podman` on macOS or your package manager on Linux)
- **Go 1.25+**: For building from source
- **kubectl** (required): Controller installation invokes it
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
export KUBECONFIG=~/.tkn-runner/k3s/k3s.yaml
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

# Inspect the recorded status (the adapter's status reporting is incomplete)
kubectl get taskrun hello-world-taskrun -o yaml
```

Container output is printed by the foreground runner. The kubelet logs
endpoint is not implemented, so standard `kubectl logs` / `tkn ... logs`
should not be relied on.

### 4. Clean up

```bash
./tkn-runner delete
```

This command attempts to remove k3s data, Podman networks, and containers.
It also runs an unscoped `podman volume prune -f`, which can remove unused
volumes unrelated to tkn-runner. Review this behavior before using it on a
shared runtime.

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

# Delete a TaskRun
kubectl delete taskrun hello-world-taskrun
```

## Examples

More examples are available in the [`examples/`](./examples) directory:
- Simple TaskRun: [`examples/simple-taskrun.yaml`](./examples/simple-taskrun.yaml)
- Test TaskRun: [`test-taskrun.yaml`](./test-taskrun.yaml)

## Platform Support

### macOS
- **Status**: Prototype implementation; no cross-platform test suite.
- **Runtime**: k3s runs in a Podman container. Podman requires a Linux VM.
- **Networking**: Custom Podman network shared by the control plane and workloads.

### Linux
- **Status**: In development (Phase 1 - CNI implementation in progress)
- **Runtime**: Native k3s startup differs from the macOS path and currently does not disable its agent. The embedded binary is Linux ARM64.
- **Networking**: Linux integration remains incomplete; distribution compatibility is not established.

## Current Limitations

This project is under active development. Current limitations include:

- Linux Flannel CNI integration is in progress
- ConfigMap/Secret environment variable references not yet supported
- Log streaming via `kubectl logs` not yet implemented
- Results extraction and faithful per-step status are incomplete
- No ingestion of local uncommitted source
- Unsupported inputs can be approximated or ignored rather than rejected

## Architecture Documentation

Start with the [assessment](designs/local-validation-assessment.md) and
[full-ecosystem proposal](designs/v4-local-tekton-ecosystem.md). The
[v1](designs/v1-managed-by-controller.md) and
[v2](designs/v2-virtual-kubelet.md) documents are historical designs, and
[v3](designs/v3-local-validation.md) is a superseded restricted-execution
proposal. None is a verified feature inventory.

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
└── designs/                 # Assessment, proposal, and historical designs
```

## Comparison with a local cluster

The PoC retains Tekton reconciliation but replaces normal worker execution.
That makes the adapter responsible for storage, lifecycle, logs, networking,
and other semantics the controller expects. A warm local Kubernetes installation
is the appropriate correctness and performance baseline.

No measured speed, memory, or full-compatibility advantage is claimed.

## Contributing

Contributions are welcome. Before expanding runtime compatibility, review the
full-ecosystem contract and its acceptance criteria. The implementation
direction is real Tekton on real local Kubernetes workers, not further
approximation in the virtual-node adapter.

## License

This project includes the Tekton Pipeline codebase. See individual components for their respective licenses.

## Acknowledgments

- [Tekton Pipelines](https://tekton.dev) - The Kubernetes-native CI/CD framework
- [Virtual Kubelet](https://virtual-kubelet.io) - The pattern that makes this possible
- [k3s](https://k3s.io) - Lightweight Kubernetes
- [Podman](https://podman.io) - Daemonless container runtime
