# Tekton Runner v2: Virtual Kubelet Architecture

## Executive Summary

This document outlines the Virtual Kubelet-based architecture for running Tekton Pipelines without Kubernetes worker nodes. Unlike the v1 design which proposed a custom controller watching the `managedBy` field, v2 leverages Virtual Kubelet to intercept Pod lifecycle events and translate them to Podman execution.

### Key Design Principles

- **Zero Tekton Modifications**: Use the standard, unmodified Tekton Pipeline controller
- **Automatic Feature Inheritance**: All Tekton features (PipelineRuns, DAGs, Triggers, etc.) work automatically
- **Pod-to-Runtime Translation**: Virtual Kubelet acts as a bridge between Kubernetes Pod API and Podman
- **Minimal Control Plane**: k3s provides only API server, state storage, and Flannel CNI networking - no actual workload scheduling

### Architectural Shift from v1

| Aspect | v1: Custom Controller | v2: Virtual Kubelet |
|--------|----------------------|---------------------|
| Tekton Controller | Disabled or filtered | Standard, unmodified |
| Custom Code | Full reconciler logic | Pod lifecycle translation only |
| Feature Parity | Manual re-implementation | Automatic inheritance |
| Maintenance Burden | High - track Tekton changes | Low - just maintain translation layer |
| PipelineRun Support | Custom DAG orchestration | Free - Tekton handles it |
| Complexity | High | Medium |

---

## 1. Architecture Overview

### System Components

```
┌─────────────────────────────────────────────────────────────┐
│  Tekton Runner Process                                       │
│                                                               │
│  ┌────────────────────────────────────────────────────────┐  │
│  │  k3s Control Plane (API Server + etcd/sqlite)          │  │
│  │  - Tekton CRDs installed                               │  │
│  │  - Standard Kubernetes API                             │  │
│  │  - Flannel CNI daemon (creates network)                │  │
│  │  - NO kubelet, NO container runtime                    │  │
│  └────────────────────────────────────────────────────────┘  │
│                          ▲                                    │
│                          │ K8s API                            │
│                          │                                    │
│  ┌───────────────────────┴────────────────────────────────┐  │
│  │  Tekton Pipeline Controller (Unmodified)               │  │
│  │  - Reconciles TaskRuns/PipelineRuns                    │  │
│  │  - Creates Pods normally                               │  │
│  │  - Monitors Pod status                                 │  │
│  │  - Handles DAG orchestration                           │  │
│  └───────────────────────┬────────────────────────────────┘  │
│                          │ Creates Pods                       │
│                          ▼                                    │
│  ┌────────────────────────────────────────────────────────┐  │
│  │  Virtual Kubelet                                       │  │
│  │  - Registers "tkn-runner-node"                      │  │
│  │  - Implements Pod lifecycle interface:                 │  │
│  │    • CreatePod(pod) → ExecutePod via Podman           │  │
│  │    • UpdatePod(pod) → Handle updates                  │  │
│  │    • DeletePod(pod) → Cleanup containers/volumes      │  │
│  │    • GetPod(ns, name) → Return pod status             │  │
│  │  - Updates Pod status back to K8s API                 │  │
│  └───────────────────────┬────────────────────────────────┘  │
│                          │                                    │
└──────────────────────────┼────────────────────────────────────┘
                           │
                           ▼
                  ┌─────────────────────────────────────┐
                  │  Podman (on Flannel CNI Network)    │
                  │  - Joins k3s Flannel network        │
                  │  - Executes containers              │
                  │  - Manages volumes                  │
                  │  - Uses CNI for networking          │
                  └─────────────────────────────────────┘
```

### Execution Flow

1. **User creates TaskRun/PipelineRun**
   - Standard Tekton YAML, no special annotations required
   - Applied via `kubectl apply -f taskrun.yaml`

2. **Tekton Controller reconciles**
   - Validates TaskRun spec
   - Creates Pod with Tekton-specific structure:
     - Init containers (place-scripts, place-tools)
     - Step containers (user-defined steps)
     - Volumes for /tekton filesystem
     - DownwardAPI for metadata

3. **Kubernetes schedules Pod**
   - Sees only one node: "tkn-runner-node" (Virtual Kubelet)
   - Schedules Pod to virtual node
   - Pod enters Pending state

4. **Virtual Kubelet intercepts Pod**
   - `CreatePod(pod)` called by Virtual Kubelet framework
   - VK immediately marks Pod as Running in K8s API
   - Starts background execution

5. **Podman Executor translates Pod → Containers**
   - Joins k3s Flannel CNI network (same network as k3s)
   - Executes init containers sequentially
   - Executes step containers sequentially
   - Mounts volumes (Podman volumes, bind mounts)
   - Sets environment variables
   - Captures logs and exit codes

6. **Status updates flow back**
   - Executor reports success/failure to VK
   - VK updates Pod status (Running → Succeeded/Failed)
   - Tekton Controller sees Pod completion
   - Updates TaskRun/PipelineRun status

7. **User sees results**
   - `kubectl get taskrun` shows Succeeded/Failed
   - `tkn taskrun logs` shows container output
   - TaskRun results extracted from /tekton/results

---

## 2. Virtual Kubelet Design

### Provider Interface

Virtual Kubelet requires implementing a Provider interface:

```go
type PodNotifier interface {
    NotifyPods(context.Context, func(*corev1.Pod))
}

type PodLifecycleHandler interface {
    CreatePod(ctx context.Context, pod *corev1.Pod) error
    UpdatePod(ctx context.Context, pod *corev1.Pod) error
    DeletePod(ctx context.Context, pod *corev1.Pod) error
    GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error)
    GetPodStatus(ctx context.Context, namespace, name string) (*corev1.PodStatus, error)
    GetPods(ctx context.Context) ([]*corev1.Pod, error)
}

type NodeProvider interface {
    ConfigureNode(ctx context.Context, node *corev1.Node)
}
```

### PodmanProvider Implementation

```go
type PodmanProvider struct {
    nodeName        string
    operatingSystem string

    // In-memory pod tracking
    pods      map[string]*corev1.Pod
    podsMutex sync.RWMutex

    // Podman executor for container execution
    executor *PodmanExecutor

    // Kubernetes client for status updates
    clientset *kubernetes.Clientset
}
```

**Key Responsibilities:**

1. **Pod State Management**
   - Maintain in-memory map of all managed pods
   - Thread-safe access via RWMutex
   - Serve GetPod/GetPods queries from memory

2. **Lifecycle Orchestration**
   - `CreatePod`: Spawn goroutine for background execution, immediately mark Running
   - `UpdatePod`: Handle pod spec updates (currently no-op)
   - `DeletePod`: Stop containers, cleanup volumes
   - `GetPod`/`GetPodStatus`: Return cached pod state

3. **Status Reporting**
   - Update pod status in local cache
   - Push status to Kubernetes API via `clientset.CoreV1().Pods().UpdateStatus()`
   - Generate container statuses (Running/Terminated)
   - Assign Pod IPs (fake or real from Podman network)

4. **Node Configuration**
   - Report fake node capacity (20 CPU, 100Gi memory, 110 pods)
   - Set node conditions (Ready, MemoryPressure, DiskPressure)
   - Identify as "Tekton Runner (Podman)" OS

---

## 3. Pod-to-Podman Translation

### Container Execution Model

Tekton Pods follow a specific structure:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: example-taskrun-pod
spec:
  initContainers:
  - name: place-scripts
    image: busybox
    volumeMounts:
    - name: tekton-internal-scripts
      mountPath: /tekton/scripts
  - name: place-tools
    image: gcr.io/tekton-releases/entrypoint
    volumeMounts:
    - name: tekton-internal-tools
      mountPath: /tekton/tools

  containers:
  - name: step-hello
    image: ubuntu
    command: ["/tekton/tools/entrypoint"]
    args: ["-wait_file", "/tekton/downward/ready", ...]
    env:
    - name: TEKTON_STEP_NUMBER
      value: "0"
    volumeMounts:
    - name: tekton-internal-workspace
      mountPath: /workspace
    - name: tekton-internal-home
      mountPath: /tekton/home
    - name: tekton-internal-results
      mountPath: /tekton/results
```

### Translation Strategy

The PodmanExecutor translates this structure into sequential Podman container executions:

```
Pod Spec                           Podman Execution
────────────────────────────────   ─────────────────────────────────
initContainers[0]: place-scripts   → podman run --rm --name tekton-init-...
                                        -v tekton-shared-ns-name:/tekton
                                        busybox [commands]

initContainers[1]: place-tools     → podman run --rm --name tekton-init-...
                                        -v tekton-shared-ns-name:/tekton
                                        gcr.io/tekton-releases/entrypoint

containers[0]: step-hello          → podman run --rm --name tekton-step-...
                                        -v tekton-shared-ns-name:/tekton
                                        -e TEKTON_STEP_NUMBER=0
                                        --network cbr0
                                        ubuntu /tekton/tools/entrypoint ...

containers[1]: step-build          → podman run --rm --name tekton-step-...
                                        [same volume, new container]
```

**Key Patterns:**

1. **Sequential Execution**: Init containers first, then regular containers in order
2. **Shared /tekton Volume**: All containers mount same Podman volume for data sharing
3. **Container Naming**: `tekton-{init|step}-{namespace}-{podname}-{containername}`
4. **Flannel CNI Network**: All containers join k3s Flannel network (cbr0) via CNI

---

## 4. Volume Management

### Volume Types and Translation

| Kubernetes Volume Type | Translation Mechanism | Implementation |
|------------------------|----------------------|----------------|
| **emptyDir** | Podman named volume | `podman volume create tekton-vol-{ns}-{pod}-{name}` |
| **downwardAPI** | Bind mount to temp directory | Create files with pod metadata, mount read-only |
| **serviceAccount** | Bind mount to temp directory | Generate token via K8s API, create token/ca.crt/namespace files |
| **configMap** | (Future) Fetch from K8s API, bind mount | Not yet implemented |
| **secret** | (Future) Fetch from K8s API, bind mount | Not yet implemented |
| **persistentVolumeClaim** | (Future) Map to host directory | Not yet implemented |

### Shared /tekton Volume

Tekton requires a writable `/tekton` directory for its internal operations:

```
/tekton/
├── scripts/        # Placed by place-scripts init container
├── tools/          # Placed by place-tools init container
│   └── entrypoint  # Tekton's step wrapper
├── downward/       # Pod metadata (ready marker, etc.)
├── home/           # HOME directory for steps
├── results/        # Task results written by steps
├── steps/          # Step status tracking
└── run/            # Runtime files
```

**Design Decision**: Use a single shared Podman volume for all `/tekton` content instead of separate volumes per subdirectory.

**Rationale**:
- Init containers populate `/tekton/scripts` and `/tekton/tools`
- Step containers need to read from these locations
- Separate volumes would create isolation, breaking init → step communication
- Shared volume mimics emptyDir behavior in real Kubernetes

**Implementation**:
```go
// Create shared volume once per pod
tektonSharedVolume := fmt.Sprintf("tekton-shared-%s-%s", pod.Namespace, pod.Name)
createVolume(tektonSharedVolume)

// Mount in every container (init and regular)
args = append(args, "-v", fmt.Sprintf("%s:/tekton", tektonSharedVolume))
```

### DownwardAPI Volume Handling

Tekton uses downwardAPI to expose pod metadata to containers:

```yaml
volumes:
- name: tekton-internal-downward
  downwardAPI:
    items:
    - path: ready
      fieldRef:
        fieldPath: metadata.annotations['tekton.dev/ready']
```

**Translation Process**:

1. Create temporary directory: `/tmp/tekton-downward/{namespace}/{pod}/{volume-name}`
2. For each downwardAPI item:
   - Resolve field path (metadata.name, metadata.annotations['key'], etc.)
   - Write value to file at `{downwardDir}/{item.path}`
3. Bind mount directory into container at volumeMount.mountPath
4. Mount as read-only (`:ro`)

**Field Path Resolution**:
```go
func resolveFieldPath(pod *corev1.Pod, fieldPath string) (string, error) {
    // Simple paths
    if fieldPath == "metadata.name" { return pod.Name, nil }
    if fieldPath == "metadata.namespace" { return pod.Namespace, nil }

    // Annotation access: metadata.annotations['tekton.dev/ready']
    if strings.HasPrefix(fieldPath, "metadata.annotations['") {
        key := extractKey(fieldPath)
        return pod.Annotations[key], nil
    }

    // Label access: metadata.labels['app']
    if strings.HasPrefix(fieldPath, "metadata.labels['") {
        key := extractKey(fieldPath)
        return pod.Labels[key], nil
    }
}
```

### Service Account Token Handling

Tekton steps need Kubernetes API access for:
- Fetching secrets
- Creating/updating resources
- Authentication to registry via image pull secrets

**Token Generation**:
```go
func createServiceAccountVolume(pod *corev1.Pod) (string, error) {
    // 1. Request token from K8s API
    tokenRequest := &authv1.TokenRequest{
        Spec: authv1.TokenRequestSpec{
            ExpirationSeconds: ptr.Int64(3600 * 24), // 24 hours
        },
    }

    saName := pod.Spec.ServiceAccountName
    if saName == "" { saName = "default" }

    tokenResponse, err := clientset.CoreV1().ServiceAccounts(pod.Namespace).
        CreateToken(ctx, saName, tokenRequest, metav1.CreateOptions{})

    // 2. Create temp directory
    saDir := filepath.Join("/tmp/tkn-runner-sa", pod.Namespace, pod.Name)
    os.MkdirAll(saDir, 0755)

    // 3. Write files
    os.WriteFile(filepath.Join(saDir, "token"), []byte(tokenResponse.Status.Token), 0644)
    os.WriteFile(filepath.Join(saDir, "ca.crt"), caCertFromKubeconfig(), 0644)
    os.WriteFile(filepath.Join(saDir, "namespace"), []byte(pod.Namespace), 0644)

    return saDir, nil
}
```

**Mount into Container**:
```bash
podman run \
  -v /tmp/tkn-runner-sa/default/taskrun-abc:/var/run/secrets/kubernetes.io/serviceaccount:ro \
  ...
```

**In-Cluster Config**:
Containers see standard service account mount and can use `rest.InClusterConfig()`:
- Token: `/var/run/secrets/kubernetes.io/serviceaccount/token`
- CA: `/var/run/secrets/kubernetes.io/serviceaccount/ca.crt`
- Namespace: `/var/run/secrets/kubernetes.io/serviceaccount/namespace`

---

## 5. Environment Variable Resolution

Tekton uses environment variables extensively for:
- Step ordering (`TEKTON_STEP_NUMBER`)
- Results paths (`/tekton/results/{name}`)
- Downward API values (pod name, namespace)

### Variable Sources

**1. Direct Values**
```yaml
env:
- name: TEKTON_STEP_NUMBER
  value: "0"
```
→ `podman run -e TEKTON_STEP_NUMBER=0`

**2. Field References (Downward API)**
```yaml
env:
- name: POD_NAME
  valueFrom:
    fieldRef:
      fieldPath: metadata.name
```
→ Resolve at execution time, inject as `-e POD_NAME=taskrun-abc`

**3. ConfigMap/Secret References**
```yaml
env:
- name: API_KEY
  valueFrom:
    secretKeyRef:
      name: api-secret
      key: key
```
→ (Future) Fetch from K8s API, inject value

### Kubernetes API Access from Containers

Containers need to communicate with the k3s API server for:
- Fetching ConfigMaps/Secrets
- Updating TaskRun status
- Image pull secrets

**Solution**: Shared Flannel CNI network (see Section 7 for details).

Step containers join the same Flannel network as k3s:

```go
// Step containers use k3s Flannel network
podman run --network cbr0 \
  -e KUBERNETES_SERVICE_HOST=10.42.0.1 \
  -e KUBERNETES_SERVICE_PORT=6443 \
  ...
```

- k3s API server binds to Flannel bridge gateway: `10.42.0.1:6443`
- Step containers get IPs from same subnet: `10.42.0.10+`
- Direct IP connectivity via Flannel CNI

---

## 6. Step Ordering and Execution Guarantees

Tekton steps must execute **sequentially** with strict ordering guarantees.

### How Tekton's Entrypoint Handles Ordering

**Critical Design Point**: Step ordering is **NOT** enforced by the executor - it's handled by Tekton's entrypoint binary.

In standard Tekton (and in Virtual Kubelet):
1. All step containers run with `/tekton/tools/entrypoint` as their entrypoint
2. The entrypoint wrapper coordinates step execution:
   - Each step waits on a "ready" file before executing: `/tekton/tools/{step-n}/out`
   - Previous step writes ready file when it completes
   - Current step polls for ready file, blocks until it exists, then executes
   - On completion, writes its own ready file for the next step
3. All steps share the `/tekton` volume for communication

**Example Step Container Spec**:
```yaml
containers:
- name: step-hello
  image: ubuntu
  command: ["/tekton/tools/entrypoint"]
  args:
    - "-wait_file"
    - "/tekton/downward/ready"
    - "-post_file"
    - "/tekton/tools/0/out"
    - "-termination_path"
    - "/tekton/termination"
    - "-step_metadata_dir"
    - "/tekton/steps/step-hello"
    - "--"
    - "/bin/bash"
    - "-c"
    - "echo hello"
  env:
  - name: TEKTON_STEP_NUMBER
    value: "0"
```

The entrypoint:
- Waits for `/tekton/downward/ready` (pod-level ready signal)
- Executes the actual command: `/bin/bash -c "echo hello"`
- Writes completion marker to `/tekton/tools/0/out`
- Next step (step-1) waits for `/tekton/tools/0/out` before starting

### Virtual Kubelet's Responsibility

The Virtual Kubelet executor **does not enforce step ordering** - it only needs to ensure:

1. **Init containers run before step containers**:
   ```go
   func (e *PodmanExecutor) ExecutePod(pod *corev1.Pod) error {
       // Execute init containers sequentially (required)
       for _, container := range pod.Spec.InitContainers {
           if err := e.executeContainer(ctx, pod, &container, true); err != nil {
               return err
           }
       }

       // Execute step containers
       // Sequential execution is simplest, but entrypoint would coordinate
       // even if containers were started in parallel
       for _, container := range pod.Spec.Containers {
           if err := e.executeContainer(ctx, pod, &container, false); err != nil {
               return err
           }
       }

       return nil
   }
   ```

2. **All containers share the `/tekton` volume**:
   - Shared Podman volume mounted at `/tekton` in all containers
   - Enables entrypoint coordination via ready files
   - Allows init containers (place-scripts, place-tools) to populate `/tekton/{scripts,tools}`

**Why Init Containers Must Be Sequential**:
- Init containers (place-scripts, place-tools) don't use the entrypoint wrapper
- They must complete before step containers start
- They populate `/tekton/tools/entrypoint` which step containers need
- This is a Kubernetes Pod guarantee that Virtual Kubelet preserves

**Why Step Container Execution Can Be Sequential**:
- Simpler implementation: run each container, wait for exit, run next
- Entrypoint coordination still works (ready files prevent actual execution)
- Avoids complexity of managing parallel container lifecycles
- No performance benefit to parallelism (entrypoint blocks anyway)

**Alternative: Parallel Step Container Starts**:
In theory, all step containers could be started simultaneously:
- Entrypoint in each would block on ready file
- Step-0 runs first (no wait file), writes `/tekton/tools/0/out`
- Step-1 sees ready file, runs, writes `/tekton/tools/1/out`
- etc.

This would match real Kubernetes more closely (all containers "running" from Pod perspective), but adds complexity with no practical benefit for Podman execution.

---

## 7. Network Architecture: Platform-Specific Networking

### Design Philosophy

Tekton Runner's networking architecture **differs by platform** due to fundamental constraints:

- **Linux**: Uses **Flannel CNI** shared between k3s and Podman
- **macOS**: Uses **Podman network** with DNS resolution (Flannel not feasible)

Both approaches provide the same outcome: step containers can reach the k3s API server.

### Why Platform-Specific Approaches?

**The Core Constraint:**
- **Linux**: k3s runs as native binary → creates real network interfaces on host → Podman can join via CNI
- **macOS**: k3s runs in Podman container → Flannel interfaces isolated inside container → Podman network is only viable option

**Why Not Flannel on macOS?**
1. k3s must run in a container (macOS cannot run k3s natively)
2. Flannel creates `cbr0` and `flannel.1` interfaces **inside** the k3s container
3. Step containers run separately in the same Podman VM
4. No shared network namespace - Flannel network is isolated and inaccessible

---

## 7.1. Linux: Flannel CNI (Shared Network)

### Architecture

```
┌─────────────────────────────────────────────────────────────┐
│  Linux Host                                                  │
│                                                               │
│  ┌────────────────────────────────────────────────────────┐  │
│  │  k3s Server (Native Binary)                            │  │
│  │  - Runs flannel daemon                                 │  │
│  │  - Creates CNI config at:                              │  │
│  │    /var/lib/rancher/k3s/agent/etc/cni/net.d/           │  │
│  │  - API Server binds to Flannel network                 │  │
│  └────────────────────────────────────────────────────────┘  │
│                          │                                    │
│                          │ Creates Host Network Interfaces    │
│                          ▼                                    │
│  ┌────────────────────────────────────────────────────────┐  │
│  │  Host Network Interfaces                               │  │
│  │  - flannel.1 (VXLAN overlay interface)                 │  │
│  │  - cbr0 (CNI bridge)                                   │  │
│  │  - Subnet: 10.42.0.0/16                                │  │
│  │  - Gateway: 10.42.0.1                                  │  │
│  └────────────────────────────────────────────────────────┘  │
│                          │                                    │
│                          │ Podman containers join via CNI     │
│                          ▼                                    │
│  ┌────────────────────────────────────────────────────────┐  │
│  │  Flannel CNI Network (cbr0)                            │  │
│  │                                                         │  │
│  │  Network Members:                                      │  │
│  │  ├─ k3s API Server: 10.42.0.1:6443                     │  │
│  │  ├─ Tekton Controller Pod: 10.42.0.2 (inside k3s)      │  │
│  │  ├─ Step Container 1: 10.42.0.10 (Podman via CNI)      │  │
│  │  ├─ Step Container 2: 10.42.0.11 (Podman via CNI)      │  │
│  │  └─ Step Container N: 10.42.0.x  (Podman via CNI)      │  │
│  └────────────────────────────────────────────────────────┘  │
│                                                               │
│  ┌────────────────────────────────────────────────────────┐  │
│  │  Podman Containers (Step Execution)                    │  │
│  │  - Use k3s CNI configuration (discovered)              │  │
│  │  - Invoke same Flannel CNI plugins                     │  │
│  │  - Get IPs from same IPAM pool                         │  │
│  │  - Reach k3s API at 10.42.0.1:6443                     │  │
│  └────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────┘
```

### Why Flannel on Linux?

**Flannel** is chosen as the CNI plugin for Linux because:

- ✅ **k3s Default** - Built into k3s, no extra installation
- ✅ **Native Host Interfaces** - Creates real network interfaces accessible to Podman
- ✅ **Battle-Tested** - Used in production k8s clusters globally
- ✅ **Multi-Host Ready** - VXLAN overlay enables future cross-node communication
- ✅ **Lightweight** - Low overhead, suitable for single-node and edge
- ✅ **Podman Compatible** - Works with Podman's CNI support

### How k3s Flannel Works (Linux)

When k3s starts with Flannel on Linux:

1. **Flannel Daemon Starts**
   ```bash
   k3s server  # Default uses flannel
   ```
   k3s launches flannel daemon internally

2. **CNI Configuration Created**
   ```
   /var/lib/rancher/k3s/agent/etc/cni/net.d/10-flannel.conflist
   ```

   Content (generated by k3s):
   ```json
   {
     "name": "cbr0",
     "cniVersion": "0.3.1",
     "plugins": [
       {
         "type": "flannel",
         "delegate": {
           "hairpinMode": true,
           "forceAddress": true,
           "isDefaultGateway": true
         }
       },
       {
         "type": "portmap",
         "capabilities": {"portMappings": true}
       }
     ]
   }
   ```

3. **Network Interfaces Created on Host**
   - Bridge: `cni0` or `cbr0` (host interface)
   - Overlay: `flannel.1` (VXLAN, host interface)
   - Gateway IP: `10.42.0.1` (default)

4. **Subnet Configuration**
   - Flannel reads subnet from `/run/flannel/subnet.env`:
     ```
     FLANNEL_NETWORK=10.42.0.0/16
     FLANNEL_SUBNET=10.42.0.1/24
     FLANNEL_MTU=1450
     FLANNEL_IPMASQ=true
     ```

### Podman Integration with Flannel CNI (Linux)

Podman must use the **same CNI configuration** as k3s to join the Flannel network.

#### CNI Configuration Discovery

```go
// pkg/networking/cni/discovery.go

type CNIDiscovery struct {
    k3sDataDir string
}

func (d *CNIDiscovery) FindK3sCNIConfig() (*CNIConfig, error) {
    // k3s CNI config location
    k3sCNIDir := filepath.Join(d.k3sDataDir, "agent", "etc", "cni", "net.d")

    // Find CNI config file (typically 10-flannel.conflist)
    files, err := os.ReadDir(k3sCNIDir)
    if err != nil {
        return nil, fmt.Errorf("failed to read k3s CNI directory: %w", err)
    }

    for _, file := range files {
        if strings.HasSuffix(file.Name(), ".conflist") ||
           strings.HasSuffix(file.Name(), ".conf") {

            configPath := filepath.Join(k3sCNIDir, file.Name())
            config, err := d.parseCNIConfig(configPath)
            if err != nil {
                continue
            }

            return config, nil
        }
    }

    return nil, fmt.Errorf("no CNI config found in %s", k3sCNIDir)
}

func (d *CNIDiscovery) parseCNIConfig(path string) (*CNIConfig, error) {
    data, err := os.ReadFile(path)
    if err != nil {
        return nil, err
    }

    var config struct {
        Name       string `json:"name"`
        CNIVersion string `json:"cniVersion"`
    }

    if err := json.Unmarshal(data, &config); err != nil {
        return nil, err
    }

    return &CNIConfig{
        Name:       config.Name,
        ConfigPath: filepath.Dir(path),
        PluginType: "flannel",
    }, nil
}
```

#### Making CNI Config Available to Podman

Podman searches for CNI configs in standard locations:
- `/etc/cni/net.d/`
- `/opt/cni/net.d/`
- Custom path via `NETCONFPATH` environment variable

**Option 1: Symlink (Recommended)**
```go
func (m *Manager) exposeCNIToPodman() error {
    k3sCNIConfig := filepath.Join(m.k3sDataDir,
        "agent/etc/cni/net.d/10-flannel.conflist")

    podmanCNIDir := "/etc/cni/net.d"
    os.MkdirAll(podmanCNIDir, 0755)

    podmanCNIConfig := filepath.Join(podmanCNIDir, "10-flannel.conflist")

    // Remove existing symlink/file
    os.Remove(podmanCNIConfig)

    // Create symlink
    return os.Symlink(k3sCNIConfig, podmanCNIConfig)
}
```

**Option 2: Environment Variable**
```go
func (e *PodmanExecutor) setupCNIEnvironment() error {
    k3sCNIDir := filepath.Join(e.k3sDataDir, "agent/etc/cni/net.d")

    // Tell Podman where to find CNI configs
    os.Setenv("NETCONFPATH", k3sCNIDir)
    os.Setenv("CNI_PATH", "/opt/cni/bin")

    return nil
}
```

**Option 3: Copy Config**
```go
func (m *Manager) copyCNIConfig() error {
    src := filepath.Join(m.k3sDataDir, "agent/etc/cni/net.d/10-flannel.conflist")
    dst := "/etc/cni/net.d/10-flannel.conflist"

    data, err := os.ReadFile(src)
    if err != nil {
        return err
    }

    return os.WriteFile(dst, data, 0644)
}
```

**Recommendation:** Use **Symlink** - keeps configs in sync if k3s updates them.

#### Podman Container Execution with CNI (Linux)

```go
// pkg/virtualkubelet/executor.go

type PodmanExecutor struct {
    networkName string  // e.g., "cbr0" from k3s CNI config
    k3sDataDir  string
}

func NewPodmanExecutor(kubeconfig string, k3sDataDir string) (*PodmanExecutor, error) {
    var networkName string

    if runtime.GOOS == "linux" {
        // Linux: Discover k3s CNI configuration
        discovery := &CNIDiscovery{k3sDataDir: k3sDataDir}
        cniConfig, err := discovery.FindK3sCNIConfig()
        if err != nil {
            return nil, fmt.Errorf("failed to find k3s CNI config: %w", err)
        }

        // Expose CNI config to Podman
        manager := &Manager{k3sDataDir: k3sDataDir}
        if err := manager.exposeCNIToPodman(); err != nil {
            return nil, fmt.Errorf("failed to expose CNI to Podman: %w", err)
        }

        networkName = cniConfig.Name  // "cbr0" from Flannel
    } else {
        // macOS: Use Podman network (see Section 7.2)
        networkName = "tkn-runner-net"
    }

    return &PodmanExecutor{
        networkName: networkName,
        k3sDataDir:  k3sDataDir,
    }, nil
}

func (e *PodmanExecutor) executeContainer(...) error {
    args := []string{"run", "--rm"}

    // Use discovered network (platform-specific)
    args = append(args, "--network", e.networkName)

    // Linux: Podman invokes Flannel CNI plugin
    // Container gets IP from same IPAM as k3s (10.42.0.x)

    // k3s API accessible at gateway IP (Linux)
    if runtime.GOOS == "linux" {
        args = append(args,
            "-e", "KUBERNETES_SERVICE_HOST=10.42.0.1",  // Flannel gateway
            "-e", "KUBERNETES_SERVICE_PORT=6443",
        )
    } else {
        // macOS: Use DNS name (see Section 7.2)
        args = append(args,
            "-e", "KUBERNETES_SERVICE_HOST=tkn-runner-k3s",
            "-e", "KUBERNETES_SERVICE_PORT=6443",
        )
    }

    // Continue with volumes, mounts, etc...
}
```

### Network Flow: Step Container → k3s API (Linux)

```
Step Container (10.42.0.10)
    ↓
Reaches out to: 10.42.0.1:6443
    ↓
Flannel Bridge (cbr0) on host
    ↓
Routes to: k3s API Server
    ↓
API Server processes request
    ↓
Response back via bridge
    ↓
Step Container receives response
```

**Example: Service Account Token Auth**
```bash
# Inside step container on Linux
$ cat /var/run/secrets/kubernetes.io/serviceaccount/token
eyJhbGc...

$ curl -k \
  -H "Authorization: Bearer $(cat /var/run/secrets/kubernetes.io/serviceaccount/token)" \
  https://10.42.0.1:6443/api/v1/namespaces/default/pods

# Success! API accessible via Flannel network
```

### Linux Implementation Summary

**Network Setup:**
1. k3s runs natively, creates Flannel network
2. Flannel creates `flannel.1` VXLAN interface
3. Bridge `cni0` or `cbr0` created
4. k3s API binds to all interfaces (including bridge)

**Podman Integration:**
- Podman runs natively
- Uses same CNI plugins as k3s
- Containers get IPs from same subnet
- Direct connectivity to k3s API

**CNI Plugin Binaries:**
k3s bundles CNI plugins at:
```
/var/lib/rancher/k3s/data/current/bin/
├── flannel
├── host-local
├── portmap
├── bridge
└── ...
```

Podman must use these binaries:
```bash
export CNI_PATH=/var/lib/rancher/k3s/data/current/bin
```

---

## 7.2. macOS: Podman Network (DNS-Based)

### Architecture

```
┌────────────────────────────────────────────────────────────┐
│  macOS Host                                                 │
│                                                              │
│  ┌────────────────────────────────────────────────────────┐│
│  │  Podman Machine (Lima VM)                              ││
│  │                                                          ││
│  │  ┌────────────────────────────────────────────────────┐││
│  │  │  Podman Network: tkn-runner-net                 │││
│  │  │  - Subnet: 10.89.0.0/16 (Podman-managed)           │││
│  │  │  - DNS: Enabled (container name resolution)        │││
│  │  │  - No Flannel CNI (not accessible)                 │││
│  │  └────────────────────────────────────────────────────┘││
│  │                 │                                       ││
│  │        ┌────────┴─────────┐                            ││
│  │        │                  │                            ││
│  │  ┌─────▼────────────┐  ┌─▼───────────────────┐        ││
│  │  │ k3s Container    │  │ Step Container       │        ││
│  │  │                  │  │                      │        ││
│  │  │ Name:            │  │ KUBERNETES_          │        ││
│  │  │ tkn-runner-   │  │ SERVICE_HOST=        │        ││
│  │  │ k3s              │◄─┤ tkn-runner-k3s    │        ││
│  │  │                  │  │ (DNS resolution)     │        ││
│  │  │ --flannel-       │  │                      │        ││
│  │  │ backend=none     │  │ Reaches k3s via DNS  │        ││
│  │  │ (disabled)       │  │                      │        ││
│  │  │                  │  │                      │        ││
│  │  │ IP: 10.89.0.x    │  │ IP: 10.89.0.y        │        ││
│  │  └──────────────────┘  └──────────────────────┘        ││
│  │                                                          ││
│  │  Both containers in same Podman network                 ││
│  │  Direct IP connectivity via Podman's bridge             ││
│  │  DNS: tkn-runner-k3s → k3s container IP              ││
│  └────────────────────────────────────────────────────────┘│
└────────────────────────────────────────────────────────────┘
```

### Why Podman Network (Not Flannel)?

**Critical Constraint:** k3s cannot run natively on macOS.

1. **k3s runs in a Podman container** (in the Podman VM)
2. **Flannel interfaces are created inside the k3s container**:
   - `flannel.1` (VXLAN) - inside k3s container
   - `cbr0` (bridge) - inside k3s container
3. **Step containers run separately** in the same Podman VM
4. **No network namespace sharing** - Flannel network is isolated

**Why Flannel Doesn't Work:**
- ❌ Flannel interfaces not accessible outside k3s container
- ❌ No shared network namespace with step containers
- ❌ CNI configuration isolated inside k3s container
- ❌ Podman cannot invoke CNI plugins from inside another container

**Podman Network Solution:**
- ✅ Create a shared Podman network: `tkn-runner-net`
- ✅ Both k3s and step containers join this network
- ✅ Podman provides DNS: `tkn-runner-k3s` resolves to k3s container IP
- ✅ Simple, reliable, works on macOS

### k3s Container Configuration (macOS)

```bash
# Create shared Podman network
podman network create tkn-runner-net

# Start k3s in container
podman run -d \
  --name tkn-runner-k3s \
  --privileged \
  --cgroupns=host \
  --network tkn-runner-net \
  -p 6443:6443 \
  -v tkn-runner-k3s-data:/var/lib/rancher/k3s \
  rancher/k3s:v1.31.4-k3s1 \
  server \
  --disable-agent \
  --flannel-backend=none \
  --disable-cloud-controller \
  --disable-network-policy \
  --disable-kube-proxy \
  --tls-san=tkn-runner-k3s \
  --tls-san=10.89.0.0/16
```

**Key Flags:**
- `--flannel-backend=none` - Disable Flannel (not needed, Podman handles networking)
- `--network tkn-runner-net` - Join shared Podman network
- `--tls-san=tkn-runner-k3s` - Allow TLS via DNS name
- `-p 6443:6443` - Expose API to host for kubectl access

### Step Container Execution (macOS)

```bash
# Step containers join same network
podman run --rm \
  --name tekton-step-default-taskrun-hello \
  --network tkn-runner-net \
  -e KUBERNETES_SERVICE_HOST=tkn-runner-k3s \
  -e KUBERNETES_SERVICE_PORT=6443 \
  -v tekton-shared-default-taskrun:/tekton \
  ubuntu \
  /tekton/tools/entrypoint ...
```

**How DNS Works:**
1. Step container looks up `tkn-runner-k3s`
2. Podman's internal DNS resolves to k3s container IP (e.g., `10.89.0.5`)
3. Step container connects to `10.89.0.5:6443`
4. k3s API server responds

### Network Flow: Step Container → k3s API (macOS)

```
Step Container (10.89.0.15)
    ↓
DNS lookup: tkn-runner-k3s
    ↓
Podman DNS: Returns 10.89.0.5 (k3s container IP)
    ↓
Connects to: 10.89.0.5:6443
    ↓
Podman bridge routes traffic
    ↓
k3s API Server receives request
    ↓
Response back via bridge
    ↓
Step Container receives response
```

**Example: Service Account Token Auth**
```bash
# Inside step container on macOS
$ cat /var/run/secrets/kubernetes.io/serviceaccount/token
eyJhbGc...

# DNS resolution
$ nslookup tkn-runner-k3s
Server:    10.89.0.1
Address:   10.89.0.1#53

Name:      tkn-runner-k3s
Address:   10.89.0.5

# API call via DNS name
$ curl -k \
  -H "Authorization: Bearer $(cat /var/run/secrets/kubernetes.io/serviceaccount/token)" \
  https://tkn-runner-k3s:6443/api/v1/namespaces/default/pods

# Success! API accessible via Podman network + DNS
```

### macOS Implementation Summary

**Network Setup:**
1. Create Podman network: `tkn-runner-net`
2. k3s runs in container, joins network
3. Flannel explicitly disabled (`--flannel-backend=none`)
4. Podman provides DNS resolution

**Step Container Execution:**
- Join same Podman network
- Use DNS name `tkn-runner-k3s` for API access
- Direct IP connectivity via Podman's bridge
- No CNI plugins needed

**Advantages:**
- ✅ Simple architecture
- ✅ Reliable DNS resolution
- ✅ No CNI complexity
- ✅ Works within Podman VM constraints

**Trade-offs:**
- ⚠️ Different from Linux approach
- ⚠️ No true Kubernetes networking
- ⚠️ Cannot easily add multiple nodes (future)

---

## 7.3. Unified Startup Sequence (Platform-Aware)

```go
// cmd/tkn-runner/main.go

func startCommand() error {
    cfg := config.DefaultConfig()

    // 1. Start k3s (platform-specific)
    //    - Linux: Native binary with Flannel
    //    - macOS: Podman container with custom network
    cp := k3s.New(cfg.ControlPlane.K3s)
    if err := cp.Start(ctx); err != nil {
        return err
    }
    defer cp.Stop()

    // 2. Platform-specific network setup
    if runtime.GOOS == "linux" {
        // Linux: Wait for Flannel CNI config to be created
        time.Sleep(5 * time.Second)

        // Discover and expose CNI config to Podman
        networkMgr := networking.NewManager(cfg.DataDir)
        if err := networkMgr.SetupFromK3s(cp.GetDataDir()); err != nil {
            return fmt.Errorf("failed to setup networking: %w", err)
        }
        fmt.Println("✅ Flannel CNI configured for Podman")
    } else {
        // macOS: Podman network already created by k3s.Start()
        fmt.Println("✅ Using Podman network: tkn-runner-net")
    }

    // 3. Install Tekton CRDs
    if err := manifests.InstallCRDs(ctx, cp.GetKubeconfig()); err != nil {
        return err
    }

    // 4. Start Virtual Kubelet (uses platform-specific network)
    vk, err := virtualkubelet.New(
        "tkn-runner-node",
        cp.GetKubeconfig(),
        cp.GetDataDir(),
    )
    if err != nil {
        return err
    }

    go vk.Run(ctx)
    time.Sleep(2 * time.Second)

    // 5. Install Tekton Controller
    if err := manifests.InstallController(ctx, cp.GetKubeconfig()); err != nil {
        return err
    }

    fmt.Println()
    fmt.Println("Tekton Runner is ready!")
    if runtime.GOOS == "linux" {
        fmt.Printf("Network: Flannel CNI (shared with k3s)\n")
        fmt.Printf("k3s API: https://10.42.0.1:6443\n")
    } else {
        fmt.Printf("Network: Podman network (tkn-runner-net)\n")
        fmt.Printf("k3s API: https://tkn-runner-k3s:6443\n")
    }
    fmt.Printf("Kubeconfig: %s\n", cp.GetKubeconfig())

    <-ctx.Done()
    return nil
}
```

### Advantages of Flannel Network (Linux)

1. **True Kubernetes Networking**
   - Same CNI stack as production k8s
   - Pods and control plane on same network
   - Familiar to Kubernetes operators

2. **No Special DNS Configuration**
   - Step containers reach API at gateway IP (10.42.0.1)
   - Standard Kubernetes networking model
   - No DNS hacks required

3. **Future Multi-Node Support**
   - Flannel VXLAN overlay supports cross-host
   - Can distribute step execution across machines
   - Network policies can be added later

4. **Controller-to-Step Communication**
   - Tekton Controller runs in k3s (on Flannel)
   - Step containers on same network
   - Controller can reach step containers directly (webhooks, metrics)

5. **Standard Tooling Works**
   - `kubectl port-forward` works
   - Service mesh can be added
   - Network debugging tools work as expected

### Advantages of Podman Network (macOS)

1. **Simplicity**
   - No CNI configuration needed
   - Podman creates and manages network
   - DNS works out of the box

2. **Reliability**
   - Well-tested Podman network implementation
   - No complex CNI plugin interactions
   - Works within Podman VM constraints

3. **Developer-Friendly**
   - Easy to debug (standard Podman tools)
   - Clear DNS resolution
   - Fewer moving parts

### Platform Comparison

| Aspect | Linux (Flannel) | macOS (Podman Network) |
|--------|----------------|----------------------|
| **k3s Mode** | Native binary | Podman container |
| **Network Type** | Flannel CNI (VXLAN) | Podman bridge network |
| **Subnet** | 10.42.0.0/16 | 10.89.0.0/16 |
| **API Access** | Gateway IP (10.42.0.1) | DNS name (tkn-runner-k3s) |
| **CNI Plugins** | Required | Not used |
| **Flannel** | Enabled | Disabled (--flannel-backend=none) |
| **DNS Resolution** | IP-based | Container name DNS |
| **Multi-Node Ready** | ✅ Yes (VXLAN overlay) | ❌ No (single Podman VM) |
| **Complexity** | Medium | Low |
| **Production Use** | ✅ Recommended | ⚠️ Development only |

---

## 8. Log Collection

### Log Sources

- **Standard Streams**: Podman containers write to stdout/stderr
- **Tekton Logs**: May write to `/tekton/steps/{step-name}/logs`

### Current Design

```go
cmd := exec.CommandContext(ctx, "podman", "run", ...)
cmd.Stdout = io.MultiWriter(os.Stdout, &stdoutBuffer)
cmd.Stderr = io.MultiWriter(os.Stderr, &stderrBuffer)

if err := cmd.Run(); err != nil {
    // Logs available in stdoutBuffer, stderrBuffer
}
```

**Behavior**:
- Logs stream to Tekton Runner's stdout in real-time
- Logs also captured in memory buffers
- On error, stderr included in error message

**Limitations**:
- Logs not persisted to Kubernetes API (Pod status message size limit)
- `kubectl logs` doesn't work (no kubelet log endpoint)
- `tkn taskrun logs` may not work without additional integration

### Future Design Considerations

**Option 1: Log Streaming API**
- Virtual Kubelet can implement `GetContainerLogs()` interface
- Stream from Podman logs: `podman logs -f {container}`
- Enable `kubectl logs pod/name -c container`

**Option 2: Log Storage**
- Write logs to files in `/var/log/tkn-runner/{namespace}/{pod}/{container}.log`
- Serve via HTTP endpoint
- Integrate with Tekton Dashboard

**Option 3: External Logging**
- Forward logs to external system (Loki, Elasticsearch)
- Tekton Dashboard queries external system

---

## 8. Results Extraction

Tekton tasks can produce results:

```yaml
apiVersion: tekton.dev/v1
kind: Task
metadata:
  name: example
spec:
  results:
  - name: commit-sha
    description: Git commit SHA
  steps:
  - name: get-sha
    image: alpine/git
    script: |
      git rev-parse HEAD | tee $(results.commit-sha.path)
```

### How Results Work

1. Tekton injects env var: `TEKTON_RESULTS_commit-sha=/tekton/results/commit-sha`
2. Step writes to `/tekton/results/commit-sha`
3. Tekton controller reads from Pod termination message or annotations
4. Updates TaskRun status with result values

### Virtual Kubelet Integration

**Current Gap**: Virtual Kubelet doesn't extract results from `/tekton/results` and update Pod annotations.

**Design Solution**:

```go
func (e *PodmanExecutor) ExecutePod(pod *corev1.Pod) error {
    // Execute all containers...

    // After successful execution, extract results
    results, err := e.extractResults(pod)
    if err != nil {
        return err
    }

    // Update pod annotations with results
    return e.updatePodResults(pod, results)
}

func (e *PodmanExecutor) extractResults(pod *corev1.Pod) (map[string]string, error) {
    results := make(map[string]string)

    // Mount the shared tekton volume and read results
    volumeName := fmt.Sprintf("tekton-shared-%s-%s", pod.Namespace, pod.Name)

    // Inspect volume to get mount point
    mountPoint := getVolumeMountPoint(volumeName)
    resultsDir := filepath.Join(mountPoint, "results")

    // Read all result files
    files, _ := os.ReadDir(resultsDir)
    for _, file := range files {
        content, err := os.ReadFile(filepath.Join(resultsDir, file.Name()))
        if err == nil {
            results[file.Name()] = string(content)
        }
    }

    return results, nil
}
```

**Alternative**: Mount Podman volume to host directory at creation time, read directly from filesystem.

---

## 9. Advantages of Virtual Kubelet Approach

### 1. Automatic Feature Inheritance

**PipelineRuns and DAG Orchestration**:
- Tekton Controller handles task dependencies (`runAfter`, `when` conditions)
- Parallel task execution at Pipeline level (not Pod level)
- Finally tasks, retries, timeouts - all handled by Tekton

**Triggers**:
- EventListeners create Pods normally
- Interceptors, TriggerBindings work unchanged
- No custom integration needed

**Tekton Dashboard**:
- Queries Kubernetes API for TaskRuns/PipelineRuns
- Sees Pod status updates from Virtual Kubelet
- Displays logs (if log API implemented)

**Catalog Compatibility**:
- Tekton Catalog tasks work without modification
- No special annotations or configuration needed

### 2. Reduced Maintenance Burden

**What v1 Would Require**:
- Reimplement TaskRun reconciliation logic
- Reimplement PipelineRun DAG orchestration
- Track Tekton API changes and update custom controller
- Handle all edge cases (cancellation, timeout, retries)
- Write extensive tests for reconciliation logic

**What v2 Requires**:
- Implement Pod lifecycle interface (5 methods)
- Translate Pod spec → Podman commands
- Update Pod status correctly
- Handle Tekton-specific volume layout

**Lines of Code Comparison** (estimated):
- v1 Custom Controller: ~5,000-10,000 lines
- v2 Virtual Kubelet: ~1,500-2,000 lines

### 3. Upgrade Path

**Tekton Version Updates**:
- v1: May require controller code changes to match new Tekton APIs
- v2: Just update Tekton controller manifest, no code changes

**Feature Adoption**:
- v1: New Tekton features require custom implementation
- v2: New features work automatically if they use standard Pod API

### 4. Developer Experience

**Testing Pipelines Locally**:
```bash
# Start Tekton Runner
tkn-runner start

# Use standard Tekton workflows
export KUBECONFIG=~/.tkn-runner/k3s/kubeconfig.yaml
kubectl apply -f task.yaml
tkn task start build-app --showlog
```

**No Mental Model Change**:
- Same YAML as production Kubernetes
- Same CLI tools (kubectl, tkn)
- Same debugging approach (get pods, describe, logs)

---

## 10. Comparison to v1 Design

### Architectural Philosophy

**v1: Replace Tekton Controller**
- Tekton Controller filtered or disabled
- Custom controller watches `managedBy: "tkn-runner"`
- Direct execution of tasks via Podman/Dagger/etc.
- Manual status updates to TaskRun/PipelineRun

**v2: Extend Kubernetes Execution Layer**
- Tekton Controller runs unmodified
- Virtual Kubelet appears as a "node" to Kubernetes
- Pods created normally, intercepted at execution layer
- Automatic status propagation through Pod → TaskRun

### Code Complexity

| Component | v1 Implementation | v2 Implementation |
|-----------|------------------|-------------------|
| **TaskRun Reconciliation** | Full custom reconciler | None - Tekton handles it |
| **PipelineRun Orchestration** | Custom DAG logic | None - Tekton handles it |
| **Pod Creation** | Manual Pod template generation | None - Tekton creates Pods |
| **Status Updates** | Update TaskRun.Status directly | Update Pod.Status, Tekton syncs |
| **Execution Layer** | Implement Executor interface | Implement VK Provider interface |
| **Volume Management** | Custom logic for workspaces | Translate Pod volumes to Podman |
| **Cancellation** | Watch TaskRun updates, kill process | Watch Pod updates, kill Podman |

### Feature Support

| Feature | v1 Approach | v2 Approach |
|---------|-------------|-------------|
| **Basic TaskRun** | Custom implementation | Pod translation |
| **PipelineRun** | Custom DAG scheduler | Automatic |
| **Retries** | Custom retry logic | Automatic |
| **Timeouts** | Custom timeout handling | Automatic |
| **Finally Tasks** | Custom implementation | Automatic |
| **When Expressions** | Custom evaluation | Automatic |
| **Matrix/Fan-out** | Custom parallelization | Automatic |
| **Triggers** | Custom integration | Automatic |
| **Dashboard** | Custom API endpoints? | Automatic |
| **Results** | Custom extraction & storage | Extract from volume + Pod annotation |

### Maintenance Over Time

**Scenario: Tekton Releases New Feature (e.g., Hermetic Execution)**

**v1 Response**:
1. Study new feature implementation in Tekton
2. Design equivalent behavior in custom controller
3. Implement custom logic
4. Test compatibility
5. Update documentation

**v2 Response**:
1. Update Tekton controller manifest to new version
2. Test that Pod translation still works
3. Done (if feature uses standard Pod API)

### Trade-offs

**v1 Advantages**:
- More control over execution model
- Can optimize for non-Pod workloads (Lambda, Dagger native)
- Simpler mental model (no "fake" Pods)

**v2 Advantages**:
- Much less code to write and maintain
- Automatic feature inheritance
- Easier to explain: "It's Tekton, but on Podman"
- Safer: Tekton's battle-tested logic

---

## 11. Technical Considerations

### Node Scheduling

**Single Virtual Node**:
- All Pods scheduled to "tkn-runner-node"
- No need for node selectors or affinity rules
- Node reports large capacity (20 CPU, 100Gi RAM) - all fake

**Pod Scheduling Behavior**:
- k3s scheduler sees only one node
- All Pods schedulable (assuming node conditions are Ready)
- No preemption, no resource limits enforced

**Future: Multiple Virtual Nodes**:
```
tkn-runner-node-podman    (backend: Podman)
tkn-runner-node-dagger    (backend: Dagger)
tkn-runner-node-docker    (backend: Docker)
```

Use `nodeSelector` or `nodeAffinity` to pick backend:
```yaml
apiVersion: tekton.dev/v1
kind: TaskRun
spec:
  podTemplate:
    nodeSelector:
      backend: dagger
```

### Concurrency

**Pod-Level Parallelism**:
- Virtual Kubelet can handle multiple CreatePod calls concurrently
- Each Pod execution runs in separate goroutine
- Podman handles concurrent container execution

**Step-Level Sequencing**:
- Within a Pod, steps execute sequentially (required by Tekton)
- Separate Pods (separate TaskRuns) can run in parallel

**Pipeline-Level Parallelism**:
- Tekton Controller orchestrates parallel tasks in a Pipeline
- Creates multiple Pods for parallel tasks
- Virtual Kubelet executes each Pod independently

### Resource Limits

**Kubernetes Resource Requests/Limits**:
```yaml
containers:
- name: step-build
  resources:
    requests:
      memory: "1Gi"
      cpu: "1"
    limits:
      memory: "2Gi"
      cpu: "2"
```

**Translation to Podman**:
```bash
podman run \
  --memory=2Gi \
  --cpus=2 \
  ...
```

**Enforcement**:
- Podman uses cgroups to enforce limits
- Actual enforcement depends on host system cgroup configuration
- Rootless Podman may have limitations

### Security Considerations

**Rootless Podman**:
- Preferred for security: containers run as non-root user
- User namespaces provide isolation
- No privileged operations required

**Service Account Tokens**:
- Generated via Kubernetes TokenRequest API
- Short-lived (24 hours default)
- Scoped to specific ServiceAccount RBAC

**Network Isolation**:
- Podman network provides basic isolation
- All Tekton steps in same network (required for API access)
- Not as strong as Kubernetes NetworkPolicies

**Secrets Handling**:
- Secrets fetched from K8s API over HTTPS
- Written to temp files with restrictive permissions
- Cleaned up after Pod deletion

### Platform Support

**Linux**:
- k3s binary runs natively
- Podman rootless recommended
- Full feature support

**macOS**:
- k3s runs in Podman container (Podman machine)
- Tekton steps run in Podman containers
- Networking via Podman's VM networking
- Volume mounting has caveats (ownership, permissions)

**Windows**:
- k3s via WSL2 or Podman machine
- Podman on Windows uses WSL2 backend
- Similar challenges to macOS

---

## 12. Future Enhancements

### Pluggable Execution Backends

**Design**: Multiple Virtual Kubelet nodes, each with different executor backend.

**Interface**:
```go
type Executor interface {
    ExecuteContainer(ctx context.Context, pod *corev1.Pod, container *corev1.Container) error
}

type PodmanExecutor struct {}
type DaggerExecutor struct {}
type DockerExecutor struct {}
```

**Virtual Kubelet Provider**:
```go
type Provider struct {
    executor Executor  // Pluggable
}

func NewProvider(executorType string) *Provider {
    var executor Executor
    switch executorType {
    case "podman":
        executor = &PodmanExecutor{}
    case "dagger":
        executor = &DaggerExecutor{}
    default:
        executor = &PodmanExecutor{}
    }
    return &Provider{executor: executor}
}
```

### Remote Execution

**SSH Executor**:
- Virtual Kubelet runs locally
- Executor connects to remote machine via SSH
- Executes Podman/Docker commands remotely
- Streams logs back over SSH

**Cloud Executors**:
- AWS Lambda: Package step as Lambda function, invoke
- Cloud Run: Deploy step as Cloud Run service, invoke
- Azure Container Instances: Create ACI for each step

### Enhanced Logging

**Implement GetContainerLogs**:
```go
func (p *PodmanProvider) GetContainerLogs(
    ctx context.Context,
    namespace, podName, containerName string,
    opts api.ContainerLogOpts,
) (io.ReadCloser, error) {
    // Stream from Podman logs
    cmd := exec.Command("podman", "logs", "-f", containerName)
    return cmd.StdoutPipe()
}
```

Enables:
- `kubectl logs pod/name -c container`
- Tekton Dashboard log viewer
- Real-time log streaming

### Workspace Persistence

**PVC Support**:
- Map PersistentVolumeClaim to host directory
- Share across TaskRuns in same Pipeline
- Enable workspace caching

```go
func (e *PodmanExecutor) handlePVC(pvcName string) string {
    pvcDir := filepath.Join("/var/lib/tkn-runner/pvc", pvcName)
    os.MkdirAll(pvcDir, 0755)
    return pvcDir
}
```

### Observability

**Metrics**:
- Tekton Runner metrics: tasks executed, success rate, duration
- Expose Prometheus endpoint
- Integrate with Grafana dashboards

**Tracing**:
- OpenTelemetry instrumentation
- Trace Pod creation → execution → completion
- Identify bottlenecks

---

## 12. Deployment & Distribution: Learning from vCluster Standalone

### Overview

vCluster Standalone (v0.29+) provides an excellent reference architecture for packaging and distributing Tekton Runner. It demonstrates how to transform a complex Kubernetes-based system into a simple, single-binary service that can run on bare metal, VMs, or edge devices.

### vCluster Standalone Architecture

```
┌───────────────────────────────────────────────┐
│  Host Machine (Bare Metal / VM)               │
│                                                │
│  ┌──────────────────────────────────────────┐ │
│  │  SystemD Service: vcluster.service       │ │
│  │                                           │ │
│  │  ExecStart=/usr/local/bin/vcluster       │ │
│  │            server --config /etc/...      │ │
│  └──────────────────────────────────────────┘ │
│                    │                           │
│                    ├─ API Server               │
│                    ├─ etcd (embedded)          │
│                    ├─ Controller Manager       │
│                    ├─ Scheduler                │
│                    │                           │
│                    ├─ CoreDNS                  │
│                    ├─ kube-proxy               │
│                    └─ Flannel CNI              │
│                                                │
│  Control plane node can also be worker node   │
└───────────────────────────────────────────────┘
```

### Key Components of vCluster Standalone

**1. Single Binary Distribution**
- Single executable: `/usr/local/bin/vcluster`
- Embeds all Kubernetes control plane components
- Embedded etcd for state storage
- No external dependencies beyond the binary itself

**2. SystemD Integration**
```ini
[Unit]
Description=vCluster Standalone
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
ExecStart=/usr/local/bin/vcluster server --config /etc/vcluster/config.yaml
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
```

**3. One-Command Installation**
```bash
export VCLUSTER_VERSION="v0.29.0"
curl -sfL https://github.com/loft-sh/vcluster/releases/download/${VCLUSTER_VERSION}/install-standalone.sh | sh -s -- --vcluster-name standalone
```

The installation script:
- Downloads vCluster binary from GitHub releases
- Creates `/etc/vcluster/` directory for configuration
- Generates default config file
- Installs systemd unit file
- Enables and starts the service

**4. YAML Configuration**
```yaml
controlPlane:
  standalone:
    enabled: true
  joinNode:
    enabled: true  # Control plane can also run workloads
privateNodes:
  enabled: true
```

**5. Automatic Component Installation**
- kubectl binary
- CoreDNS (service discovery)
- kube-proxy (network routing)
- Flannel CNI (default networking)

### Lessons for Tekton Runner

#### 1. SystemD Service Pattern

**Current Tekton Runner:**
- CLI tool: `tkn-runner start`
- Runs in foreground
- User must keep terminal open or background manually
- Ctrl+C to stop

**vCluster-Inspired Approach:**
```bash
# Installation
sudo tkn-runner install

# Creates systemd service
sudo systemctl enable tkn-runner
sudo systemctl start tkn-runner

# Management
sudo systemctl status tkn-runner
sudo journalctl -u tkn-runner -f

# Stop
sudo systemctl stop tkn-runner
```

**Benefits:**
- ✅ Runs in background automatically
- ✅ Survives reboots
- ✅ Standard Linux service management
- ✅ Integrated logging (journald)
- ✅ Better for production/appliance deployments

**Trade-offs:**
- ⚠️ Requires sudo/root for installation
- ⚠️ More complex for development/testing
- ⚠️ Harder to debug (logs in journald, not stdout)

#### 2. Installation Script

**Proposed `install-tkn-runner.sh`:**
```bash
#!/bin/bash
# install-tkn-runner.sh

set -e

TEKTON_RUNNER_VERSION="${TEKTON_RUNNER_VERSION:-latest}"
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/tkn-runner"
DATA_DIR="/var/lib/tkn-runner"

echo "Installing Tekton Runner ${TEKTON_RUNNER_VERSION}..."

# Download binary
ARCH=$(uname -m)
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
BINARY_URL="https://github.com/waveywaves/tkn-runner/releases/download/${TEKTON_RUNNER_VERSION}/tkn-runner-${OS}-${ARCH}"

echo "Downloading from ${BINARY_URL}..."
curl -sfL "${BINARY_URL}" -o "${INSTALL_DIR}/tkn-runner"
chmod +x "${INSTALL_DIR}/tkn-runner"

# Create directories
mkdir -p "${CONFIG_DIR}" "${DATA_DIR}" "${DATA_DIR}/k3s"

# Generate default config
cat > "${CONFIG_DIR}/config.yaml" <<EOF
dataDir: ${DATA_DIR}

controlPlane:
  k3s:
    dataDir: ${DATA_DIR}/k3s

networking:
  cni: flannel

virtualKubelet:
  nodeName: tkn-runner-node

executor:
  backend: podman

logging:
  level: info
  format: json
EOF

# Create systemd unit file
cat > /etc/systemd/system/tkn-runner.service <<EOF
[Unit]
Description=Tekton Runner Service
Documentation=https://github.com/waveywaves/tkn-runner
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${INSTALL_DIR}/tkn-runner serve --config ${CONFIG_DIR}/config.yaml
Restart=always
RestartSec=10
Environment="HOME=${DATA_DIR}"

# Logging
StandardOutput=journal
StandardError=journal
SyslogIdentifier=tkn-runner

[Install]
WantedBy=multi-user.target
EOF

# Reload systemd
systemctl daemon-reload

# Enable and start service
systemctl enable tkn-runner
systemctl start tkn-runner

echo ""
echo "✅ Tekton Runner installed successfully!"
echo ""
echo "Commands:"
echo "  Status:     sudo systemctl status tkn-runner"
echo "  Logs:       sudo journalctl -u tkn-runner -f"
echo "  Stop:       sudo systemctl stop tkn-runner"
echo "  Restart:    sudo systemctl restart tkn-runner"
echo "  Uninstall:  sudo tkn-runner uninstall"
echo ""
echo "Configuration: ${CONFIG_DIR}/config.yaml"
echo "Data:          ${DATA_DIR}"
echo "Kubeconfig:    ${DATA_DIR}/k3s/server/cred/admin.kubeconfig"
```

#### 3. Configuration File Structure

**Proposed `/etc/tkn-runner/config.yaml`:**
```yaml
# Data directory for all Tekton Runner state
dataDir: /var/lib/tkn-runner

# Control plane configuration
controlPlane:
  type: k3s  # Future: support k0s, kcp, etc.
  k3s:
    dataDir: /var/lib/tkn-runner/k3s
    disabledComponents:
      - traefik
      - servicelb
      - metrics-server
      - local-storage

# Network configuration
networking:
  cni: flannel  # Use k3s Flannel CNI
  # Future: bridge, calico, cilium

# Virtual Kubelet configuration
virtualKubelet:
  nodeName: tkn-runner-node
  capacity:
    cpu: "20"
    memory: "100Gi"
    pods: "110"

# Executor configuration
executor:
  backend: podman  # Future: docker, dagger, shell
  podman:
    rootless: true
    socket: unix:///run/user/1000/podman/podman.sock

# Logging
logging:
  level: info  # debug, info, warn, error
  format: json  # json, text
  output: stdout  # stdout, file, journald

# Tekton configuration
tekton:
  controllerNamespace: tekton-pipelines
  installCRDs: true
  installController: true
```

#### 4. Command Structure

**Proposed Commands:**

```go
// cmd/tkn-runner/main.go

func main() {
    app := &cli.App{
        Name:    "tkn-runner",
        Usage:   "Run Tekton Pipelines without Kubernetes workers",
        Version: "v0.1.0",
        Commands: []*cli.Command{
            // Development Mode
            {
                Name:    "start",
                Usage:   "Start Tekton Runner in foreground (development mode)",
                Aliases: []string{"run"},
                Flags: []cli.Flag{
                    &cli.StringFlag{
                        Name:  "data-dir",
                        Value: "~/.tkn-runner",
                        Usage: "Data directory",
                    },
                },
                Action: startCommand,
            },

            // Service Mode
            {
                Name:  "install",
                Usage: "Install Tekton Runner as systemd service",
                Flags: []cli.Flag{
                    &cli.StringFlag{
                        Name:  "config",
                        Value: "/etc/tkn-runner/config.yaml",
                        Usage: "Configuration file path",
                    },
                },
                Action: installCommand,
            },
            {
                Name:   "uninstall",
                Usage:  "Uninstall Tekton Runner systemd service",
                Action: uninstallCommand,
            },
            {
                Name:  "serve",
                Usage: "Run server (used by systemd)",
                Flags: []cli.Flag{
                    &cli.StringFlag{
                        Name:     "config",
                        Value:    "/etc/tkn-runner/config.yaml",
                        Usage:    "Configuration file path",
                        Required: true,
                    },
                },
                Action: serveCommand,
            },

            // Utilities
            {
                Name:   "status",
                Usage:  "Show Tekton Runner status",
                Action: statusCommand,
            },
            {
                Name:   "kubeconfig",
                Usage:  "Print kubeconfig path or content",
                Action: kubeconfigCommand,
            },
            {
                Name:   "version",
                Usage:  "Show version information",
                Action: versionCommand,
            },
        },
    }

    app.Run(os.Args)
}
```

**Command Implementations:**

```go
func installCommand(c *cli.Context) error {
    configPath := c.String("config")

    // Check if already installed
    if serviceExists("tkn-runner") {
        return fmt.Errorf("tkn-runner service already installed")
    }

    // Create config directory
    configDir := filepath.Dir(configPath)
    if err := os.MkdirAll(configDir, 0755); err != nil {
        return fmt.Errorf("failed to create config directory: %w", err)
    }

    // Generate default config if not exists
    if _, err := os.Stat(configPath); os.IsNotExist(err) {
        if err := generateDefaultConfig(configPath); err != nil {
            return err
        }
    }

    // Create systemd unit file
    if err := createSystemdUnit(configPath); err != nil {
        return err
    }

    // Enable and start service
    exec.Command("systemctl", "daemon-reload").Run()
    exec.Command("systemctl", "enable", "tkn-runner").Run()
    exec.Command("systemctl", "start", "tkn-runner").Run()

    fmt.Println("✅ Tekton Runner installed and started")
    fmt.Printf("Config: %s\n", configPath)
    fmt.Println("Use: sudo systemctl status tkn-runner")

    return nil
}

func uninstallCommand(c *cli.Context) error {
    // Stop and disable service
    exec.Command("systemctl", "stop", "tkn-runner").Run()
    exec.Command("systemctl", "disable", "tkn-runner").Run()

    // Remove systemd unit file
    os.Remove("/etc/systemd/system/tkn-runner.service")
    exec.Command("systemctl", "daemon-reload").Run()

    fmt.Println("✅ Tekton Runner uninstalled")
    fmt.Println("Note: Data directory not removed. Manual cleanup:")
    fmt.Println("  sudo rm -rf /var/lib/tkn-runner")
    fmt.Println("  sudo rm -rf /etc/tkn-runner")

    return nil
}

func serveCommand(c *cli.Context) error {
    configPath := c.String("config")

    // Load configuration
    cfg, err := loadConfig(configPath)
    if err != nil {
        return fmt.Errorf("failed to load config: %w", err)
    }

    // This is the main server logic (current startCommand)
    return runServer(cfg)
}

func statusCommand(c *cli.Context) error {
    // Check systemd service status
    cmd := exec.Command("systemctl", "is-active", "tkn-runner")
    output, _ := cmd.Output()

    serviceStatus := strings.TrimSpace(string(output))

    fmt.Printf("Service Status: %s\n", serviceStatus)

    if serviceStatus == "active" {
        // Check k3s health
        // Check Virtual Kubelet health
        // Show active TaskRuns
    }

    return nil
}
```

#### 5. Dual Mode Support: Development vs. Production

**Development Mode** (Current behavior, keep it!):

```bash
# Quick start for local development
tkn-runner start

# Runs in foreground
# Logs to stdout
# Easy to kill (Ctrl+C)
# No sudo required
# Data in ~/.tkn-runner
```

**Use cases:**
- Local development and testing
- Quick iteration
- Debugging
- No system-wide installation

**Production Mode** (New, vCluster-inspired):

```bash
# Install as system service
sudo tkn-runner install

# Managed by systemd
sudo systemctl start/stop/restart tkn-runner
sudo journalctl -u tkn-runner -f

# Survives reboots
# Standard service management
# Data in /var/lib/tkn-runner
```

**Use cases:**
- Production deployments
- Server/VM installations
- CI/CD infrastructure
- Edge devices
- Appliance-style deployments

### Comparison: Current vs. vCluster-Inspired Approach

| Aspect | Current (Development) | Proposed (Service Mode) |
|--------|----------------------|-------------------------|
| **Installation** | `go install` or binary | `curl ... \| sh` or `tkn-runner install` |
| **Running** | `tkn-runner start` | `systemctl start tkn-runner` |
| **Lifecycle** | Manual (foreground) | SystemD (background) |
| **Logs** | stdout/stderr | journald |
| **Data Location** | `~/.tkn-runner` | `/var/lib/tkn-runner` |
| **Config** | Hard-coded defaults | `/etc/tkn-runner/config.yaml` |
| **Survives Reboot** | No | Yes |
| **Requires Sudo** | No | Yes (for install) |
| **Best For** | Development, testing | Production, servers, edge |

### Implementation Strategy

**Phase 1: Foundation (Current)**
- ✅ Keep `tkn-runner start` for development
- ✅ Basic configuration structure
- ✅ Clean command-line interface

**Phase 2: Service Mode**
- Add `install`/`uninstall`/`serve` commands
- Create systemd unit file
- Support config file loading
- Installation script

**Phase 3: Distribution**
- GitHub Actions for releases
- Build binaries for multiple platforms
- Publish installation script
- Documentation for both modes

**Phase 4: Advanced Features**
- Config validation and migration
- HA support (multi-node)
- Update mechanism
- Backup/restore

### Benefits of This Approach

1. **Developer-Friendly**
   - Keep simple `tkn-runner start` for development
   - No system-wide changes required for testing
   - Fast iteration cycle

2. **Production-Ready**
   - SystemD integration for reliable service management
   - Standard Linux deployment patterns
   - Automatic startup on boot

3. **Familiar Patterns**
   - Same patterns as vCluster, k3s, docker
   - Well-understood service management
   - Easy to document and support

4. **Flexible Deployment**
   - Support both development and production use cases
   - Single binary works in both modes
   - Configuration-driven behavior

### Configuration Management Best Practices

**From vCluster, we learn:**

1. **Sensible Defaults**
   - Work out-of-the-box with minimal config
   - Override only what's necessary
   - Auto-detect platform differences

2. **YAML Configuration**
   - Human-readable format
   - Easy to version control
   - Clear structure

3. **Validation**
   - Validate config on startup
   - Clear error messages
   - Fail fast on misconfiguration

4. **Documentation**
   - Example configurations
   - Comment every field
   - Migration guides

### Recommendations

1. **Keep Development Mode Simple**
   - Don't break existing `tkn-runner start` workflow
   - Low barrier to entry for new users
   - Fast local testing

2. **Add Service Mode Incrementally**
   - Start with `install`/`uninstall` commands
   - Add systemd support
   - Polish installation script
   - Document both modes clearly

3. **Learn from vCluster's Success**
   - Single binary distribution
   - One-command installation
   - Standard service patterns
   - Clear separation of dev vs. prod modes

4. **Future: Consider Embedding k3s**
   - vCluster embeds all components
   - True single binary (no external k3s dependency)
   - Larger binary (~50-70MB) but simpler deployment
   - Good for appliance-style distributions

---

## 13. Resource Overhead Analysis

### Overview

Understanding the resource requirements of Tekton Runner v2 is critical for deployment planning. This section analyzes CPU, memory, disk, and network overhead for all components, comparing idle vs. active states and providing recommendations for different deployment scenarios.

### Component-by-Component Breakdown (Idle State)

#### 1. k3s Control Plane (Native Process)

**Components:**
- API Server
- etcd (embedded sqlite3)
- Controller Manager
- Scheduler
- Flannel daemon

**Resource Usage:**
- **CPU**: 0.2-0.5 cores (idle), spikes to 1-2 cores during reconciliation
- **Memory**:
  - Server-only mode: ~100-150 MB
  - With Flannel: ~200-250 MB
  - Under load: ~300-400 MB
- **Disk**:
  - Binary: ~70 MB
  - Data (sqlite + certs): ~50-100 MB initially
  - Grows with resources: +10 MB per 100 resources

**Notes:** k3s documentation states server-only mode uses ~100MB RAM, but Flannel CNI adds ~50-100 MB overhead.

---

#### 2. Tekton Pipeline Controller (Pod in k3s)

**Components:**
- tekton-pipelines-controller container
- tekton-pipelines-webhook container

**Resource Usage:**
- **CPU**: 0.1-0.2 cores (idle), spikes to 0.5-1 core during TaskRun creation
- **Memory**:
  - Controller: ~50-80 MB (idle), ~150-200 MB (active)
  - Webhook: ~30-50 MB
  - Total: ~100-150 MB (idle), ~200-250 MB (active)
- **Disk**: Container images ~150-200 MB

**Notes:** Tekton's resource requests are typically 100m CPU and 100Mi memory per container.

---

#### 3. Virtual Kubelet (Go Process)

**Components:**
- Virtual Kubelet framework
- Podman provider implementation
- In-memory pod tracking

**Resource Usage:**
- **CPU**: 0.05-0.1 cores (very light, mostly idle)
- **Memory**: ~30-50 MB (Go runtime + pod state)
- **Disk**: Binary ~20-30 MB

**Notes:** Virtual Kubelet is lightweight; similar to other VK providers (AWS Fargate VK uses ~40MB).

---

#### 4. Podman (Container Runtime)

**Base Daemon (rootless mode):**
- **CPU**: Negligible (event-driven, no persistent daemon)
- **Memory**: ~20-30 MB (rootless), ~50-80 MB (rootful with service)
- **Disk**:
  - Binary: ~30 MB
  - Image cache: Variable (depends on pulled images)

**Notes:** Podman is daemonless by default - no persistent process except when containers are actively running.

---

#### 5. Flannel CNI

**Components:**
- Flannel daemon (embedded in k3s)
- CNI plugin binaries

**Resource Usage:**
- **CPU**: 0.05-0.1 cores
- **Memory**: ~50-80 MB
- **Disk**: CNI plugins ~20 MB

---

### Total Baseline (Idle State)

| Component | CPU | Memory | Disk |
|-----------|-----|--------|------|
| k3s Control Plane | 0.2-0.5 | 200-250 MB | 120-170 MB |
| Tekton Controller | 0.1-0.2 | 100-150 MB | 150-200 MB |
| Virtual Kubelet | 0.05-0.1 | 30-50 MB | 20-30 MB |
| Podman (rootless) | ~0 | 20-30 MB | 30 MB |
| Flannel CNI | 0.05-0.1 | 50-80 MB | 20 MB |
| **TOTAL** | **0.4-0.9 cores** | **400-560 MB** | **340-450 MB** |

**Rounded Conservative Estimates (Idle):**
- **CPU**: ~0.5-1 core
- **Memory**: ~500-600 MB
- **Disk**: ~500 MB (base installation, excluding image cache)

---

### Active Load: Per TaskRun Overhead

#### Infrastructure Overhead (per TaskRun)

When a TaskRun is created and executed:

**CPU Spikes:**
- Tekton Controller (reconciliation): +0.2-0.5 cores (brief)
- Virtual Kubelet (Pod lifecycle): +0.05-0.1 cores
- k3s API Server (status updates): +0.1-0.2 cores
- **Total**: +0.3-0.8 cores (transient spikes)

**Memory:**
- In-memory Pod representation (k3s + VK): ~5-10 MB per Pod
- Podman volume metadata: ~1-2 MB per volume
- **Total**: +10-20 MB per concurrent TaskRun

**Disk (temporary per TaskRun):**
- Shared `/tekton` volume: ~10-50 MB (scripts, results)
- Container logs: ~1-10 MB per step
- Service account tokens, downward API: ~1-5 MB
- **Total**: +50-150 MB (cleaned up after TaskRun completes)

---

#### Workload Overhead (step containers)

**This is entirely workload-dependent.**

Example TaskRun with 3 steps:
```yaml
apiVersion: tekton.dev/v1
kind: TaskRun
spec:
  taskSpec:
    steps:
    - name: clone
      image: alpine/git
      # Image: ~10 MB
      # Memory: ~20 MB

    - name: build
      image: golang:1.21
      # Image: ~800 MB
      # Memory: ~1-2 GB during compilation

    - name: test
      image: golang:1.21
      # Image: cached
      # Memory: ~500 MB during tests
```

**Per Step Container:**
- **CPU**: Variable (0.1-8 cores, depends on step workload)
- **Memory**: Variable (50 MB - 8 GB, depends on step workload)
- **Disk**:
  - Container image: Variable (10 MB - 2 GB per unique image)
  - Workspace data: Variable

**Critical Point:** Steps run **sequentially** (not in parallel), so:
- **Peak memory** = memory of largest step (not sum of all steps)
- **CPU** = CPU of currently executing step

---

### Concurrent TaskRuns Scaling

Virtual Kubelet can handle multiple concurrent TaskRuns, with Podman executing containers in parallel.

**Resource Scaling for N Concurrent TaskRuns:**

| Concurrent TaskRuns | CPU (infrastructure) | Memory (infrastructure) | Notes |
|---------------------|----------------------|-------------------------|-------|
| 0 (idle) | 0.5-1 core | 500-600 MB | Baseline |
| 1 | 0.8-1.8 cores | 510-620 MB | Baseline + 1 TaskRun overhead |
| 5 | 1.5-4 cores | 550-700 MB | Baseline + 5 TaskRuns |
| 10 | 2.5-7 cores | 600-800 MB | Baseline + 10 TaskRuns |
| 20 | 4-12 cores | 700 MB - 1 GB | API server load increases |
| 50 | 8-20 cores | 1-1.5 GB | Near k3s bottleneck |

**Notes:**
- Above numbers are **infrastructure overhead only** (not including actual step workload)
- Add step container resources on top
- k3s API server becomes bottleneck at ~50-100 concurrent TaskRuns
- Podman can handle 100+ containers, limited by system resources
- System file descriptor limits may need tuning (`ulimit -n`)

---

### Storage Breakdown

#### Persistent Disk Usage

```
/var/lib/tkn-runner/                    (or ~/.tkn-runner/)
├── k3s/
│   ├── server/
│   │   ├── db/
│   │   │   └── state.db               # 10-100 MB (grows with resources)
│   │   ├── tls/                       # 5-10 MB (certificates)
│   │   ├── manifests/                 # 5 MB (Tekton CRDs)
│   │   └── cred/
│   │       └── admin.kubeconfig       # <1 MB
│   └── data/
│       └── current/bin/               # 50 MB (CNI plugins)
│
└── Podman image cache (if using system Podman)
    ├── overlay-images/                # Variable (pulled container images)
    └── volumes/                       # Variable (TaskRun workspaces)

Total Base Installation: ~500 MB
```

#### Temporary Disk Usage (per TaskRun)

```
/tmp/tkn-runner-sa/{namespace}/{pod}/    # Service account tokens: 1-5 MB
/tmp/tekton-downward/{namespace}/{pod}/     # Downward API files: <1 MB
Podman volumes: tekton-shared-{ns}-{pod}    # 10-100 MB per TaskRun
```

Cleaned up automatically after TaskRun completion.

#### Container Image Cache

Container images are the primary disk consumer:

| Image Type | Size Range | Examples |
|------------|------------|----------|
| Alpine-based | 10-50 MB | alpine/git, alpine |
| Language runtimes | 200-800 MB | golang:1.21, node:20, python:3.11 |
| Builder images | 500 MB - 2 GB | gradle, maven, rust |
| Full OS images | 100-300 MB | ubuntu, debian |

**Total cache can grow to:** 5-20 GB over time with frequent builds.

**Recommendation:** Enable periodic image pruning:
```bash
# Prune images older than 24 hours
podman image prune -a --filter "until=24h"

# Or via cron job
0 2 * * * podman image prune -a --filter "until=72h" -f
```

---

### Network Overhead

#### Flannel VXLAN Overlay

For single-node deployment:
- **Bandwidth**: Negligible (no cross-host traffic)
- **Latency**: +0.5-1 ms (VXLAN encapsulation overhead)
- **MTU**: Reduced by 50 bytes (VXLAN header: default 1500 → 1450)

For multi-node (future):
- **Bandwidth**: Depends on cross-node pod communication
- **Latency**: +1-3 ms (VXLAN + network latency)

#### k3s API Traffic

Per TaskRun lifecycle:
- TaskRun creation: ~100-500 KB (Pod spec + CRDs)
- Status updates (per step): ~10-50 KB each
- Logs/results: ~1-10 KB per update

**Bandwidth Requirements:**
- Idle: <100 KB/s
- 10 concurrent TaskRuns: ~1-5 MB/s (mostly API calls)
- 100 concurrent TaskRuns: ~10-50 MB/s (API becomes bottleneck)

#### Container Image Pulls

- **Bandwidth**: Variable (1-100 MB/s, depends on registry and network)
- **Latency**: Variable (depends on image size and network)

**Optimization:** Use a local registry or image mirror for frequently used images.

---

### Comparison to Alternatives

#### vs. Tekton on Full Kubernetes (minikube, kind)

| Component | Minikube/Kind | Tekton Runner v2 | Savings |
|-----------|---------------|------------------|---------|
| Control Plane (k8s) | 500-800 MB | 200-250 MB | ~60% |
| Kubelet | 100-150 MB | 0 MB | 100% |
| Container Runtime | 100-200 MB | 20-30 MB | ~85% |
| Tekton Controllers | 100-150 MB | 100-150 MB | 0% |
| CNI (Flannel) | 50-80 MB | 50-80 MB | 0% |
| **Total Idle** | **850 MB - 1.3 GB** | **400-600 MB** | **~50%** |
| **Disk** | ~1-1.5 GB | ~500 MB | ~60% |
| **CPU (idle)** | ~1-1.5 cores | ~0.5-1 core | ~40% |

**Tekton Runner v2 saves:**
- ~400-700 MB RAM (no kubelet, lighter control plane)
- ~500 MB - 1 GB disk (no full k8s binaries, smaller footprint)
- ~0.5-1 CPU cores (no kubelet scheduling overhead)

---

### System Requirements

#### Minimum (Development/Testing)

**For single TaskRun at a time:**
- **CPU**: 2 cores
- **Memory**: 2 GB RAM
- **Disk**: 5 GB free space
- **OS**: Linux (native) or macOS (Podman machine adds +1 GB RAM overhead)

#### Recommended (Light Production)

**For <10 concurrent TaskRuns:**
- **CPU**: 4 cores
- **Memory**: 4 GB RAM
- **Disk**: 20 GB free space (allows image cache growth)
- **Network**: 10 Mbps+ (for image pulls)

#### Heavy Production

**For 20-50 concurrent TaskRuns:**
- **CPU**: 8+ cores
- **Memory**: 8-16 GB RAM
- **Disk**: 50+ GB free space
- **Network**: 100 Mbps+ (for faster image pulls)

#### Platform-Specific Notes

**Linux (Native):**
- Most efficient - all components run natively
- Use resource estimates as-is

**macOS (Podman Machine):**
- Add +1 GB RAM overhead for Podman VM
- k3s runs in Podman container, add +200-300 MB RAM
- Total idle overhead: ~700-900 MB instead of ~500-600 MB

**Windows (WSL2 + Podman):**
- Similar to macOS overhead
- WSL2 VM adds base overhead
- Recommend 8 GB RAM minimum

---

### Optimization Strategies

#### 1. Minimize k3s Overhead

```bash
k3s server \
  --disable traefik,servicelb,metrics-server,local-storage \
  --kube-apiserver-arg=max-requests-inflight=200 \
  --kube-apiserver-arg=max-mutating-requests-inflight=100 \
  --kube-controller-manager-arg=concurrent-deployment-syncs=5
```

**Saves:** ~50-100 MB RAM, reduces API server contention.

#### 2. Single Replica Tekton Controller

Deploy Tekton controller with 1 replica instead of HA setup:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: tekton-pipelines-controller
spec:
  replicas: 1  # Instead of 2+
```

**Saves:** ~100-150 MB RAM per additional replica.

#### 3. Use Rootless Podman

```bash
# Enable rootless mode
podman system migrate
```

**Saves:** ~30-50 MB RAM (no root daemon required).

#### 4. Enable Automatic Image Pruning

```bash
# Add to systemd service or cron
0 2 * * * /usr/bin/podman image prune -a --filter "until=72h" -f
```

**Saves:** Reclaim 5-20 GB disk over time.

#### 5. Periodic Database Compaction

k3s sqlite database grows over time:

```bash
# Compact sqlite database (requires k3s stop)
systemctl stop tkn-runner
sqlite3 /var/lib/tkn-runner/k3s/server/db/state.db "VACUUM;"
systemctl start tkn-runner
```

**Saves:** Reclaim 10-50 MB disk.

#### 6. Limit TaskRun History

Configure Tekton to automatically cleanup completed TaskRuns:

```yaml
# In Tekton ConfigMap
apiVersion: v1
kind: ConfigMap
metadata:
  name: config-defaults
  namespace: tekton-pipelines
data:
  default-task-run-workspace-binding: |
    emptyDir: {}
  default-timeout-minutes: "60"
  default-service-account: "default"
  default-managed-by-label-value: "tekton-pipelines"
  # Cleanup completed TaskRuns after 1 hour
  default-task-run-completed-max-age: "1h"
```

**Saves:** Reduces k3s database growth, keeps memory usage stable.

---

### Monitoring Resource Usage

#### Check Current Usage

```bash
# Overall system resources
top
htop

# k3s memory usage
ps aux | grep k3s

# Podman containers
podman stats

# Disk usage
du -sh /var/lib/tkn-runner/*
podman system df

# Image cache size
podman images --format "{{.Size}}" | awk '{sum+=$1} END {print sum/1024/1024 " MB"}'
```

#### Set Up Monitoring

For production deployments, consider:

**Prometheus + Grafana:**
- Expose k3s metrics: `--kube-apiserver-arg=enable-prometheus-metrics=true`
- Use node_exporter for system metrics
- Use cadvisor for container metrics

**Simple Alerting:**
```bash
# Alert if memory > 80%
free | awk '/Mem:/ {if ($3/$2 > 0.8) print "High memory usage!"}'

# Alert if disk > 90%
df -h / | awk 'NR==2 {if ($5+0 > 90) print "High disk usage!"}'
```

---

### Scaling Limits

#### Theoretical Limits

Based on component constraints:

| Resource | Limit | Bottleneck |
|----------|-------|------------|
| **Concurrent TaskRuns** | ~50-100 | k3s API server throughput |
| **Total TaskRuns (lifetime)** | Unlimited | sqlite DB grows (~10 MB/100 resources) |
| **Container Images** | Unlimited | Disk space only |
| **Pod/Task Size** | ~10 MB Pod spec | k3s etcd/sqlite limit (default 1.5 MB) |

#### Practical Limits

Real-world constraints:

| Scenario | Limit | Reason |
|----------|-------|--------|
| **Developer Laptop** | 5-10 concurrent | RAM constraint (2-4 GB available) |
| **Small VM (2 vCPU, 4 GB RAM)** | 5-15 concurrent | CPU + RAM constraint |
| **Medium Server (8 vCPU, 16 GB RAM)** | 30-50 concurrent | k3s API bottleneck |
| **Large Server (16+ vCPU, 32+ GB RAM)** | 50-100 concurrent | k3s single-node limit |

**Beyond 100 concurrent:** Consider multi-node deployment (future enhancement).

---

### Resource Usage Summary

#### Idle State (No Active TaskRuns)

✅ **CPU**: ~0.5-1 core
✅ **Memory**: ~500-600 MB
✅ **Disk**: ~500 MB (base) + image cache

**Comparable to:** Small Java application or lightweight database.

#### Per TaskRun (Infrastructure Only)

✅ **CPU**: +0.3-0.8 cores (transient spikes)
✅ **Memory**: +10-20 MB
✅ **Disk**: +50-150 MB (temporary)

**Notes:** Actual step workload dominates these numbers.

#### Real-World Example

**Scenario:** Building a Go application (3 steps: clone, build, test)

| Phase | CPU | Memory | Disk |
|-------|-----|--------|------|
| Idle | 0.5 cores | 500 MB | 500 MB |
| Clone (alpine/git) | 1 core | 520 MB | 510 MB |
| Build (golang:1.21) | 4 cores | 2 GB | 1.3 GB |
| Test (golang:1.21) | 2 cores | 800 MB | 1.3 GB |
| After cleanup | 0.5 cores | 500 MB | 1.3 GB |

**Peak:** 4 cores, 2 GB RAM during build step.
**Baseline restored** after TaskRun completes (except cached image).

#### Comparison Chart

```
Resource Usage: Tekton Runner v2 vs. Full Kubernetes

Memory (MB)
   │
1500│                                    ┌─ Full k8s (minikube)
   │                               ┌────┤
1000│                          ┌────┘    │
   │                     ┌────┤          │
 500│────────────────────┤     │          │
   │                    └─ Tekton Runner v2
   └─────────────────────────────────────────
     Idle    1 TaskRun   5 TaskRuns   10 TaskRuns

Tekton Runner v2: ~50% less overhead
```

---

### Conclusion

**Tekton Runner v2 is lightweight and efficient:**

1. **Minimal Idle Overhead**: ~500-600 MB RAM, can run on modest hardware
2. **Efficient Scaling**: Linear overhead per TaskRun (~10-20 MB each)
3. **Workload-Dominated**: Actual step containers use far more resources than infrastructure
4. **50% Savings vs. Full Kubernetes**: Eliminating kubelet and using lighter k3s saves significant resources
5. **Production-Ready**: Can handle 20-50 concurrent TaskRuns on a medium server

**Best suited for:**
- ✅ Local development (minimal laptop overhead)
- ✅ Edge devices (Raspberry Pi 4 with 4 GB RAM)
- ✅ Small to medium CI/CD workloads
- ✅ Cost-sensitive deployments (smaller VMs)

**Not ideal for:**
- ❌ Very high concurrency (>100 TaskRuns) - use full k8s cluster
- ❌ Multi-node orchestration - requires k8s cluster features

---

## 14. Conclusion

The Virtual Kubelet architecture (v2) provides a simpler, more maintainable approach to running Tekton Pipelines outside of traditional Kubernetes clusters. By leveraging the standard Tekton Pipeline controller and intercepting execution at the Pod layer, we inherit all of Tekton's features automatically while minimizing custom code.

### Design Validation

**Does it work?**
- ✅ TaskRuns execute successfully
- ✅ Init containers and steps run sequentially
- ✅ Volumes shared correctly (/tekton filesystem)
- ✅ Service account tokens generated and mounted
- ✅ Pod status updates propagate to TaskRun status

**Is it maintainable?**
- ✅ ~1,500 lines of code vs. ~10,000 for custom controller
- ✅ Tekton updates don't require code changes
- ✅ Clear separation of concerns (VK vs. Executor)

**Is it extensible?**
- ✅ Executor interface can support multiple backends
- ✅ Multiple virtual nodes enable backend selection
- ✅ Standard K8s patterns (nodeSelector) for routing

### Key Insight

The Virtual Kubelet approach transforms the problem from "reimplementing Tekton" to "translating Pods to containers." This is a much smaller, better-defined problem that leverages existing, mature components (Tekton Controller, Virtual Kubelet framework, Podman) rather than building everything from scratch.

By treating Kubernetes as an API layer and Tekton as an orchestration layer, we achieve the original goals of the v1 design (running Tekton without K8s workers) with significantly less complexity and better forward compatibility.
