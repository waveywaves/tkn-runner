package virtualkubelet

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// PodmanExecutor executes Pods via Podman
type PodmanExecutor struct {
	podmanBinary string
	kubeconfig   string
}

// NewPodmanExecutor creates a new Podman executor
func NewPodmanExecutor(kubeconfig string) (*PodmanExecutor, error) {
	// Check if podman is available
	podmanBinary, err := exec.LookPath("podman")
	if err != nil {
		return nil, fmt.Errorf("podman not found in PATH: %w", err)
	}

	return &PodmanExecutor{
		podmanBinary: podmanBinary,
		kubeconfig:   kubeconfig,
	}, nil
}

// ExecutePod executes all containers in a Pod sequentially
func (e *PodmanExecutor) ExecutePod(ctx context.Context, pod *corev1.Pod) error {
	fmt.Printf("🚀 Executing Pod: %s/%s\n", pod.Namespace, pod.Name)

	// Execute init containers first
	for i, container := range pod.Spec.InitContainers {
		fmt.Printf("  ⏳ Running init container %d/%d: %s\n", i+1, len(pod.Spec.InitContainers), container.Name)
		if err := e.executeContainer(ctx, pod, &container, true); err != nil {
			return fmt.Errorf("init container %s failed: %w", container.Name, err)
		}
		fmt.Printf("  ✅ Init container completed: %s\n", container.Name)
	}

	// Separate sidecars from regular step containers
	var stepContainers []corev1.Container
	var sidecarContainers []corev1.Container

	for _, container := range pod.Spec.Containers {
		if isSidecar(&container) {
			sidecarContainers = append(sidecarContainers, container)
		} else {
			stepContainers = append(stepContainers, container)
		}
	}

	// Start sidecar containers in the background
	sidecarCtx, cancelSidecars := context.WithCancel(ctx)
	defer cancelSidecars()

	sidecarErrs := make(chan error, len(sidecarContainers))
	for i, container := range sidecarContainers {
		fmt.Printf("  🔄 Starting sidecar %d/%d: %s\n", i+1, len(sidecarContainers), container.Name)
		go func(c corev1.Container) {
			if err := e.executeSidecar(sidecarCtx, pod, &c); err != nil {
				sidecarErrs <- fmt.Errorf("sidecar %s failed: %w", c.Name, err)
			}
		}(container)
	}

	// Execute regular containers sequentially (Tekton steps)
	var stepErr error
	for i, container := range stepContainers {
		fmt.Printf("  ⏳ Running step %d/%d: %s\n", i+1, len(stepContainers), container.Name)
		if err := e.executeContainer(ctx, pod, &container, false); err != nil {
			stepErr = fmt.Errorf("container %s failed: %w", container.Name, err)
			break
		}
		fmt.Printf("  ✅ Step completed: %s\n", container.Name)
	}

	// Signal sidecars to stop
	cancelSidecars()

	// Check for sidecar errors (non-blocking)
	select {
	case err := <-sidecarErrs:
		if stepErr == nil {
			return err
		}
		// If steps also failed, return step error (more important)
		fmt.Printf("  ⚠️  Sidecar error (ignoring due to step failure): %v\n", err)
	default:
		// No sidecar errors
	}

	return stepErr
}

// executeContainer executes a single container
func (e *PodmanExecutor) executeContainer(ctx context.Context, pod *corev1.Pod, container *corev1.Container, isInit bool) error {
	containerName := e.getContainerName(pod, container, isInit)

	// Build podman run command
	// Use shared Podman network for inter-container communication
	networkName := "tkn-runner-net"
	args := []string{"run", "--rm", "--name", containerName, "--network", networkName}

	// Run as root to bypass Podman volume permission issues on macOS
	// This is a workaround for volume ownership/permission semantics in Podman on macOS
	args = append(args, "--user", "root")

	// Create a shared volume for /tekton that all containers in the pod can access
	// This replaces emptyDir behavior and allows init containers to pass data to step containers
	tektonSharedVolume := fmt.Sprintf("tekton-shared-%s-%s", pod.Namespace, pod.Name)
	if err := e.createVolume(ctx, tektonSharedVolume); err != nil {
		return fmt.Errorf("failed to create shared tekton volume: %w", err)
	}
	args = append(args, "-v", fmt.Sprintf("%s:/tekton", tektonSharedVolume))

	// Add environment variables
	for _, env := range container.Env {
		value := e.resolveEnvValue(pod, &env)
		args = append(args, "-e", fmt.Sprintf("%s=%s", env.Name, value))
	}

	// Add Kubernetes API server environment variables for in-cluster auth
	// Use k3s container name - Podman network provides DNS resolution
	args = append(args, "-e", "KUBERNETES_SERVICE_HOST=tkn-runner-k3s")
	args = append(args, "-e", "KUBERNETES_SERVICE_PORT=6443")

	// Set TEKTON_PLATFORM_COMMANDS with ARM64 platform support if not set
	// This prevents both "unexpected end of JSON input" and "could not find command for platform" errors
	// The entrypoint will use these commands when no explicit entrypoint is provided
	hasPlatformCommands := false
	for _, env := range container.Env {
		if env.Name == "TEKTON_PLATFORM_COMMANDS" {
			hasPlatformCommands = true
			break
		}
	}
	if !hasPlatformCommands {
		// Include ARM64 platform mappings for macOS Apple Silicon
		platformCommands := `{"linux/amd64":["/ko-app/entrypoint"],"linux/arm64":["/ko-app/entrypoint"],"linux/arm64/v8":["/ko-app/entrypoint"]}`
		args = append(args, "-e", fmt.Sprintf("TEKTON_PLATFORM_COMMANDS=%s", platformCommands))
	}

	// Add volume mounts using Podman volumes (works better on macOS)
	for _, mount := range container.VolumeMounts {
		// Special handling for service account token volumes
		if mount.MountPath == "/var/run/secrets/kubernetes.io/serviceaccount" {
			// Create service account volume with token files
			saVolumePath, err := e.createServiceAccountVolume(ctx, pod)
			if err != nil {
				fmt.Printf("  ⚠️  Failed to create service account volume: %v\n", err)
				// Continue anyway - some containers might work without it
				continue
			}
			// Mount as bind mount
			args = append(args, "-v", fmt.Sprintf("%s:%s:ro", saVolumePath, mount.MountPath))
			continue
		}

		// Find the volume definition in pod spec
		var volume *corev1.Volume
		for i := range pod.Spec.Volumes {
			if pod.Spec.Volumes[i].Name == mount.Name {
				volume = &pod.Spec.Volumes[i]
				break
			}
		}

		// Handle downwardAPI volumes
		if volume != nil && volume.DownwardAPI != nil {
			downwardPath, err := e.createDownwardAPIVolume(ctx, pod, volume)
			if err != nil {
				fmt.Printf("  ⚠️  Failed to create downward API volume: %v\n", err)
				continue
			}
			// Mount as bind mount
			args = append(args, "-v", fmt.Sprintf("%s:%s:ro", downwardPath, mount.MountPath))
			continue
		}

		// Regular volume mounting using Podman volumes
		volumeName := e.getVolumeName(pod, mount.Name)

		// Create the volume if it doesn't exist
		if err := e.createVolume(ctx, volumeName); err != nil {
			return fmt.Errorf("failed to create volume %s: %w", volumeName, err)
		}

		// Mount the volume
		args = append(args, "-v", fmt.Sprintf("%s:%s", volumeName, mount.MountPath))
	}

	// IMPORTANT: Tekton's entrypoint expects /tekton/downward to always be mounted
	// even if it's not in the container's volumeMounts list. We need to explicitly
	// mount it for all Tekton containers.
	for i := range pod.Spec.Volumes {
		volume := &pod.Spec.Volumes[i]
		if volume.Name == "tekton-internal-downward" && volume.DownwardAPI != nil {
			// Check if we already mounted this volume
			alreadyMounted := false
			for _, mount := range container.VolumeMounts {
				if mount.Name == volume.Name {
					alreadyMounted = true
					break
				}
			}

			if !alreadyMounted {
				// Create the downward API volume
				downwardPath, err := e.createDownwardAPIVolume(ctx, pod, volume)
				if err != nil {
					fmt.Printf("  ⚠️  Failed to create downward API volume: %v\n", err)
				} else {
					// Mount it at the standard Tekton location
					args = append(args, "-v", fmt.Sprintf("%s:/tekton/downward:ro", downwardPath))
				}
			}
			break
		}
	}

	// Note: With the shared /tekton volume, we don't need to mount individual tekton-internal volumes
	// The shared volume provides all the necessary writable space for entrypoint
	// Individual volumes like tekton-internal-bin and tekton-internal-steps are still mounted
	// via the container.VolumeMounts list above and will shadow the shared volume at their paths

	// Set working directory if specified
	if container.WorkingDir != "" {
		args = append(args, "-w", container.WorkingDir)
	}

	// Add image
	args = append(args, container.Image)

	// Add command and args
	if len(container.Command) > 0 {
		args = append(args, container.Command...)
	}
	if len(container.Args) > 0 {
		args = append(args, container.Args...)
	}

	// Execute podman run
	cmd := exec.CommandContext(ctx, e.podmanBinary, args...)
	// Capture output to both display and buffer for error reporting
	var stdout, stderr bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stdout, &stdout)
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)

	fmt.Printf("    🔧 Running: podman %s\n", strings.Join(args, " "))

	if err := cmd.Run(); err != nil {
		// Include captured stderr in error message
		if stderr.Len() > 0 {
			return fmt.Errorf("podman run failed: %w\nSTDERR: %s", err, stderr.String())
		}
		return fmt.Errorf("podman run failed: %w", err)
	}

	return nil
}

// resolveEnvValue resolves environment variable value from direct value or downward API
func (e *PodmanExecutor) resolveEnvValue(pod *corev1.Pod, env *corev1.EnvVar) string {
	// If there's a direct value, use it
	if env.Value != "" {
		return env.Value
	}

	// If there's a ValueFrom, resolve it
	if env.ValueFrom != nil {
		// Handle FieldRef (downward API for pod metadata)
		if env.ValueFrom.FieldRef != nil {
			fieldPath := env.ValueFrom.FieldRef.FieldPath
			switch fieldPath {
			case "metadata.name":
				return pod.Name
			case "metadata.namespace":
				return pod.Namespace
			case "metadata.uid":
				return string(pod.UID)
			case "spec.nodeName":
				return pod.Spec.NodeName
			case "spec.serviceAccountName":
				return pod.Spec.ServiceAccountName
			case "status.podIP":
				return pod.Status.PodIP
			case "status.hostIP":
				return pod.Status.HostIP
			default:
				// Handle metadata.labels['key'] and metadata.annotations['key']
				if strings.HasPrefix(fieldPath, "metadata.labels['") {
					key := strings.TrimSuffix(strings.TrimPrefix(fieldPath, "metadata.labels['"), "']")
					if val, ok := pod.Labels[key]; ok {
						return val
					}
				} else if strings.HasPrefix(fieldPath, "metadata.annotations['") {
					key := strings.TrimSuffix(strings.TrimPrefix(fieldPath, "metadata.annotations['"), "']")
					if val, ok := pod.Annotations[key]; ok {
						return val
					}
				}
				fmt.Printf("  ⚠️  Unknown fieldRef path: %s\n", fieldPath)
				return ""
			}
		}

		// Handle ConfigMapKeyRef (TODO: implement if needed)
		if env.ValueFrom.ConfigMapKeyRef != nil {
			fmt.Printf("  ⚠️  ConfigMapKeyRef not yet implemented for env var %s\n", env.Name)
			return ""
		}

		// Handle SecretKeyRef (TODO: implement if needed)
		if env.ValueFrom.SecretKeyRef != nil {
			fmt.Printf("  ⚠️  SecretKeyRef not yet implemented for env var %s\n", env.Name)
			return ""
		}

		// Handle ResourceFieldRef (TODO: implement if needed)
		if env.ValueFrom.ResourceFieldRef != nil {
			fmt.Printf("  ⚠️  ResourceFieldRef not yet implemented for env var %s\n", env.Name)
			return ""
		}
	}

	return ""
}

// CleanupPod cleans up containers and volumes for a Pod
func (e *PodmanExecutor) CleanupPod(ctx context.Context, pod *corev1.Pod) error {
	fmt.Printf("🧹 Cleaning up Pod: %s/%s\n", pod.Namespace, pod.Name)

	// Remove init containers
	for _, container := range pod.Spec.InitContainers {
		containerName := e.getContainerName(pod, &container, true)
		e.removeContainer(ctx, containerName)
	}

	// Remove regular containers
	for _, container := range pod.Spec.Containers {
		containerName := e.getContainerName(pod, &container, false)
		e.removeContainer(ctx, containerName)
	}

	// Clean up Podman volumes
	volumeNames := make(map[string]bool)
	for _, container := range pod.Spec.InitContainers {
		for _, mount := range container.VolumeMounts {
			volumeNames[e.getVolumeName(pod, mount.Name)] = true
		}
	}
	for _, container := range pod.Spec.Containers {
		for _, mount := range container.VolumeMounts {
			volumeNames[e.getVolumeName(pod, mount.Name)] = true
		}
	}

	for volumeName := range volumeNames {
		e.removeVolume(ctx, volumeName)
	}

	return nil
}

// removeContainer removes a single container
func (e *PodmanExecutor) removeContainer(ctx context.Context, containerName string) {
	cmd := exec.CommandContext(ctx, e.podmanBinary, "rm", "-f", containerName)
	if err := cmd.Run(); err != nil {
		// Ignore errors - container might not exist
		fmt.Printf("  ⚠️  Failed to remove container %s (might not exist): %v\n", containerName, err)
	}
}

// getContainerName generates a unique container name
func (e *PodmanExecutor) getContainerName(pod *corev1.Pod, container *corev1.Container, isInit bool) string {
	prefix := "step"
	if isInit {
		prefix = "init"
	}
	return fmt.Sprintf("tekton-%s-%s-%s-%s", prefix, pod.Namespace, pod.Name, container.Name)
}

// getVolumeName generates a unique volume name for a pod's volume
func (e *PodmanExecutor) getVolumeName(pod *corev1.Pod, volumeName string) string {
	return fmt.Sprintf("tekton-vol-%s-%s-%s", pod.Namespace, pod.Name, volumeName)
}

// createVolume creates a Podman volume if it doesn't exist
func (e *PodmanExecutor) createVolume(ctx context.Context, volumeName string) error {
	// Check if volume exists
	cmd := exec.CommandContext(ctx, e.podmanBinary, "volume", "inspect", volumeName)
	if err := cmd.Run(); err == nil {
		// Volume exists
		return nil
	}

	// Create volume
	cmd = exec.CommandContext(ctx, e.podmanBinary, "volume", "create", volumeName)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create volume: %w", err)
	}

	return nil
}

// removeVolume removes a Podman volume
func (e *PodmanExecutor) removeVolume(ctx context.Context, volumeName string) {
	cmd := exec.CommandContext(ctx, e.podmanBinary, "volume", "rm", "-f", volumeName)
	if err := cmd.Run(); err != nil {
		// Ignore errors - volume might not exist
		fmt.Printf("  ⚠️  Failed to remove volume %s (might not exist): %v\n", volumeName, err)
	}
}

// createServiceAccountVolume creates a directory with service account token files
func (e *PodmanExecutor) createServiceAccountVolume(ctx context.Context, pod *corev1.Pod) (string, error) {
	// Create directory for service account files
	saDir := filepath.Join(os.TempDir(), "tkn-runner-sa", pod.Namespace, pod.Name)
	if err := os.MkdirAll(saDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create service account directory: %w", err)
	}

	// Load kubeconfig and create Kubernetes client
	config, err := clientcmd.BuildConfigFromFlags("", e.kubeconfig)
	if err != nil {
		return "", fmt.Errorf("failed to build config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return "", fmt.Errorf("failed to create clientset: %w", err)
	}

	// Get service account name from pod spec (or use default)
	saName := pod.Spec.ServiceAccountName
	if saName == "" {
		saName = "default"
	}

	// Create service account token request
	tokenRequest := &authv1.TokenRequest{
		Spec: authv1.TokenRequestSpec{
			ExpirationSeconds: func() *int64 { exp := int64(3600 * 24); return &exp }(), // 24 hours
		},
	}

	// Request token from Kubernetes API
	tokenResponse, err := clientset.CoreV1().ServiceAccounts(pod.Namespace).CreateToken(
		ctx,
		saName,
		tokenRequest,
		metav1.CreateOptions{},
	)
	if err != nil {
		return "", fmt.Errorf("failed to create service account token: %w", err)
	}

	token := tokenResponse.Status.Token

	// Write token file
	if err := os.WriteFile(filepath.Join(saDir, "token"), []byte(token), 0644); err != nil {
		return "", fmt.Errorf("failed to write token file: %w", err)
	}

	// Get CA certificate from kubeconfig
	kubeconfigData, err := clientcmd.LoadFromFile(e.kubeconfig)
	if err != nil {
		return "", fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	contextName := kubeconfigData.CurrentContext
	context, ok := kubeconfigData.Contexts[contextName]
	if !ok {
		return "", fmt.Errorf("context %s not found", contextName)
	}

	cluster, ok := kubeconfigData.Clusters[context.Cluster]
	if !ok {
		return "", fmt.Errorf("cluster %s not found", context.Cluster)
	}

	// Write CA certificate
	caCert := cluster.CertificateAuthorityData
	if len(caCert) == 0 && cluster.CertificateAuthority != "" {
		caCert, err = os.ReadFile(cluster.CertificateAuthority)
		if err != nil {
			return "", fmt.Errorf("failed to read CA file: %w", err)
		}
	}
	if len(caCert) == 0 {
		return "", fmt.Errorf("no CA certificate found")
	}
	if err := os.WriteFile(filepath.Join(saDir, "ca.crt"), caCert, 0644); err != nil {
		return "", fmt.Errorf("failed to write ca.crt: %w", err)
	}

	// Write namespace file
	if err := os.WriteFile(filepath.Join(saDir, "namespace"), []byte(pod.Namespace), 0644); err != nil {
		return "", fmt.Errorf("failed to write namespace: %w", err)
	}

	return saDir, nil
}

// createDownwardAPIVolume creates a directory with downward API files
func (e *PodmanExecutor) createDownwardAPIVolume(ctx context.Context, pod *corev1.Pod, volume *corev1.Volume) (string, error) {
	// Create directory for downward API files
	downwardDir := filepath.Join(os.TempDir(), "tekton-downward", pod.Namespace, pod.Name, volume.Name)
	if err := os.MkdirAll(downwardDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create downward API directory: %w", err)
	}

	// Process each item in the downwardAPI spec
	for _, item := range volume.DownwardAPI.Items {
		if item.FieldRef != nil {
			// Resolve the field path
			value, err := e.resolveFieldPath(pod, item.FieldRef.FieldPath)
			if err != nil {
				fmt.Printf("  ⚠️  Failed to resolve field path %s: %v (using empty string)\n", item.FieldRef.FieldPath, err)
				// Use empty string as default value instead of skipping the file
				// This ensures the file exists even if the annotation/field isn't set yet
				value = ""
			}

			// Write to the file
			filePath := filepath.Join(downwardDir, item.Path)

			// Create parent directories if needed
			if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
				return "", fmt.Errorf("failed to create directory for %s: %w", item.Path, err)
			}

			if err := os.WriteFile(filePath, []byte(value), 0644); err != nil {
				return "", fmt.Errorf("failed to write downward API file %s: %w", item.Path, err)
			}
		}
	}

	return downwardDir, nil
}

// resolveFieldPath resolves a downward API field path to its value
func (e *PodmanExecutor) resolveFieldPath(pod *corev1.Pod, fieldPath string) (string, error) {
	// Handle metadata.annotations['key'] format
	if strings.HasPrefix(fieldPath, "metadata.annotations['") && strings.HasSuffix(fieldPath, "']") {
		// Extract annotation key
		key := strings.TrimPrefix(fieldPath, "metadata.annotations['")
		key = strings.TrimSuffix(key, "']")

		if value, ok := pod.Annotations[key]; ok {
			return value, nil
		}
		return "", fmt.Errorf("annotation %s not found", key)
	}

	// Handle metadata.labels['key'] format
	if strings.HasPrefix(fieldPath, "metadata.labels['") && strings.HasSuffix(fieldPath, "']") {
		key := strings.TrimPrefix(fieldPath, "metadata.labels['")
		key = strings.TrimSuffix(key, "']")

		if value, ok := pod.Labels[key]; ok {
			return value, nil
		}
		return "", fmt.Errorf("label %s not found", key)
	}

	// Handle simple field paths
	switch fieldPath {
	case "metadata.name":
		return pod.Name, nil
	case "metadata.namespace":
		return pod.Namespace, nil
	case "metadata.uid":
		return string(pod.UID), nil
	case "spec.nodeName":
		return pod.Spec.NodeName, nil
	case "spec.serviceAccountName":
		return pod.Spec.ServiceAccountName, nil
	case "status.podIP":
		return pod.Status.PodIP, nil
	case "status.hostIP":
		return pod.Status.HostIP, nil
	default:
		return "", fmt.Errorf("unsupported field path: %s", fieldPath)
	}
}

// isSidecar determines if a container is a sidecar container
// Tekton sidecars are identified by specific naming patterns or restart policies
func isSidecar(container *corev1.Container) bool {
	// Tekton's sidecarlogresults container has a specific name pattern
	if strings.HasPrefix(container.Name, "sidecar-") {
		return true
	}

	// Additional heuristic: containers with "sidecar" in the image name
	if strings.Contains(container.Image, "sidecarlogresults") {
		return true
	}

	return false
}

// executeSidecar executes a sidecar container in the background
// Sidecars run until the context is cancelled
func (e *PodmanExecutor) executeSidecar(ctx context.Context, pod *corev1.Pod, container *corev1.Container) error {
	containerName := e.getContainerName(pod, container, false) + "-sidecar"

	// Build podman run command (similar to executeContainer but runs in background)
	networkName := "tkn-runner-net"
	args := []string{"run", "--rm", "--name", containerName, "--network", networkName}

	// Run as root
	args = append(args, "--user", "root")

	// Create a shared volume for /tekton
	tektonSharedVolume := fmt.Sprintf("tekton-shared-%s-%s", pod.Namespace, pod.Name)
	if err := e.createVolume(ctx, tektonSharedVolume); err != nil {
		return fmt.Errorf("failed to create shared tekton volume: %w", err)
	}
	args = append(args, "-v", fmt.Sprintf("%s:/tekton", tektonSharedVolume))

	// Add environment variables
	for _, env := range container.Env {
		value := e.resolveEnvValue(pod, &env)
		args = append(args, "-e", fmt.Sprintf("%s=%s", env.Name, value))
	}

	// Add Kubernetes API server environment variables
	args = append(args, "-e", "KUBERNETES_SERVICE_HOST=tkn-runner-k3s")
	args = append(args, "-e", "KUBERNETES_SERVICE_PORT=6443")

	// Set TEKTON_PLATFORM_COMMANDS
	hasPlatformCommands := false
	for _, env := range container.Env {
		if env.Name == "TEKTON_PLATFORM_COMMANDS" {
			hasPlatformCommands = true
			break
		}
	}
	if !hasPlatformCommands {
		platformCommands := `{"linux/amd64":["/ko-app/entrypoint"],"linux/arm64":["/ko-app/entrypoint"],"linux/arm64/v8":["/ko-app/entrypoint"]}`
		args = append(args, "-e", fmt.Sprintf("TEKTON_PLATFORM_COMMANDS=%s", platformCommands))
	}

	// Add volume mounts
	for _, mount := range container.VolumeMounts {
		// Special handling for service account token volumes
		if mount.MountPath == "/var/run/secrets/kubernetes.io/serviceaccount" {
			saVolumePath, err := e.createServiceAccountVolume(ctx, pod)
			if err != nil {
				fmt.Printf("  ⚠️  Failed to create service account volume for sidecar: %v\n", err)
				continue
			}
			args = append(args, "-v", fmt.Sprintf("%s:%s:ro", saVolumePath, mount.MountPath))
			continue
		}

		// Regular volume mounting
		volumeName := e.getVolumeName(pod, mount.Name)
		if err := e.createVolume(ctx, volumeName); err != nil {
			return fmt.Errorf("failed to create volume %s: %w", volumeName, err)
		}
		args = append(args, "-v", fmt.Sprintf("%s:%s", volumeName, mount.MountPath))
	}

	// Set working directory if specified
	if container.WorkingDir != "" {
		args = append(args, "-w", container.WorkingDir)
	}

	// Add image
	args = append(args, container.Image)

	// Add command and args
	if len(container.Command) > 0 {
		args = append(args, container.Command...)
	}
	if len(container.Args) > 0 {
		args = append(args, container.Args...)
	}

	// Execute podman run in background
	cmd := exec.CommandContext(ctx, e.podmanBinary, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	fmt.Printf("    🔧 Running sidecar: podman %s\n", strings.Join(args, " "))

	if err := cmd.Run(); err != nil {
		// Context cancellation is expected, don't treat it as an error
		if ctx.Err() != nil {
			fmt.Printf("  ✅ Sidecar %s stopped (context cancelled)\n", container.Name)
			return nil
		}
		return fmt.Errorf("podman run failed for sidecar: %w", err)
	}

	return nil
}
