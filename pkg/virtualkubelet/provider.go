package virtualkubelet

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// PodmanProvider implements a Virtual Kubelet provider that executes Pods via Podman
type PodmanProvider struct {
	nodeName      string
	operatingSystem string

	// Track running pods
	pods      map[string]*corev1.Pod
	podsMutex sync.RWMutex

	// Podman executor
	executor *PodmanExecutor

	// Kubernetes client for updating pod status
	clientset *kubernetes.Clientset

	// IP address counter for generating fake pod IPs
	nextIP int
}

// NewPodmanProvider creates a new Podman-based Virtual Kubelet provider
func NewPodmanProvider(nodeName, kubeconfig string) (*PodmanProvider, error) {
	executor, err := NewPodmanExecutor(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create podman executor: %w", err)
	}

	// Create Kubernetes client for updating pod status
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to build kubeconfig: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	return &PodmanProvider{
		nodeName:        nodeName,
		operatingSystem: "Linux", // Podman runs Linux containers
		pods:            make(map[string]*corev1.Pod),
		executor:        executor,
		clientset:       clientset,
		nextIP:          1, // Start from 10.244.0.1
	}, nil
}

// CreatePod accepts a Pod definition and executes it via Podman
func (p *PodmanProvider) CreatePod(ctx context.Context, pod *corev1.Pod) error {
	key := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)

	fmt.Printf("📦 CreatePod called: %s\n", key)

	p.podsMutex.Lock()
	p.pods[key] = pod.DeepCopy()
	p.podsMutex.Unlock()

	// Execute pod via Podman in background
	go func() {
		if err := p.executor.ExecutePod(context.Background(), pod); err != nil {
			fmt.Printf("❌ Pod execution failed %s: %v\n", key, err)
			p.updatePodStatus(pod, corev1.PodFailed, err.Error())
		} else {
			fmt.Printf("✅ Pod execution succeeded %s\n", key)
			p.updatePodStatus(pod, corev1.PodSucceeded, "Completed")
		}
	}()

	// Mark pod as running immediately
	p.updatePodStatus(pod, corev1.PodRunning, "Pod is running")

	return nil
}

// UpdatePod updates a running Pod
func (p *PodmanProvider) UpdatePod(ctx context.Context, pod *corev1.Pod) error {
	key := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	fmt.Printf("🔄 UpdatePod called: %s\n", key)

	p.podsMutex.Lock()
	p.pods[key] = pod.DeepCopy()
	p.podsMutex.Unlock()

	return nil
}

// DeletePod removes a Pod and cleans up Podman containers
func (p *PodmanProvider) DeletePod(ctx context.Context, pod *corev1.Pod) error {
	key := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	fmt.Printf("🗑️  DeletePod called: %s\n", key)

	p.podsMutex.Lock()
	delete(p.pods, key)
	p.podsMutex.Unlock()

	// Clean up Podman containers
	if err := p.executor.CleanupPod(ctx, pod); err != nil {
		fmt.Printf("⚠️  Failed to cleanup pod %s: %v\n", key, err)
	}

	return nil
}

// GetPod retrieves a Pod by namespace and name
func (p *PodmanProvider) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	key := fmt.Sprintf("%s/%s", namespace, name)

	p.podsMutex.RLock()
	pod, exists := p.pods[key]
	p.podsMutex.RUnlock()

	if !exists {
		return nil, fmt.Errorf("pod not found: %s", key)
	}

	return pod.DeepCopy(), nil
}

// GetPodStatus returns the status of a Pod
func (p *PodmanProvider) GetPodStatus(ctx context.Context, namespace, name string) (*corev1.PodStatus, error) {
	pod, err := p.GetPod(ctx, namespace, name)
	if err != nil {
		return nil, err
	}

	return &pod.Status, nil
}

// GetPods returns all managed Pods
func (p *PodmanProvider) GetPods(ctx context.Context) ([]*corev1.Pod, error) {
	p.podsMutex.RLock()
	defer p.podsMutex.RUnlock()

	pods := make([]*corev1.Pod, 0, len(p.pods))
	for _, pod := range p.pods {
		pods = append(pods, pod.DeepCopy())
	}

	return pods, nil
}

// updatePodStatus updates the status of a pod
func (p *PodmanProvider) updatePodStatus(pod *corev1.Pod, phase corev1.PodPhase, message string) {
	key := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)

	p.podsMutex.Lock()
	defer p.podsMutex.Unlock()

	if storedPod, exists := p.pods[key]; exists {
		// Get real pod IP from first container if running
		if storedPod.Status.PodIP == "" && phase == corev1.PodRunning {
			// Try to get IP from first container
			if len(storedPod.Spec.Containers) > 0 {
				containerName := fmt.Sprintf("tekton-step-%s-%s-%s", pod.Namespace, pod.Name, storedPod.Spec.Containers[0].Name)
				if ip := p.getContainerIP(containerName); ip != "" {
					storedPod.Status.PodIP = ip
					storedPod.Status.HostIP = "127.0.0.1"
				}
			}
			// Fallback to fake IP if we can't get real one
			if storedPod.Status.PodIP == "" {
				storedPod.Status.PodIP = fmt.Sprintf("10.244.0.%d", p.nextIP)
				storedPod.Status.HostIP = "127.0.0.1"
				p.nextIP++
			}
		}

		storedPod.Status.Phase = phase
		storedPod.Status.Message = message
		storedPod.Status.Conditions = []corev1.PodCondition{
			{
				Type:               corev1.PodReady,
				Status:             corev1.ConditionTrue,
				LastTransitionTime: metav1.Now(),
			},
		}

		// Update container statuses
		for i := range storedPod.Spec.Containers {
			containerName := storedPod.Spec.Containers[i].Name

			var containerState corev1.ContainerState
			if phase == corev1.PodRunning {
				containerState = corev1.ContainerState{
					Running: &corev1.ContainerStateRunning{
						StartedAt: metav1.NewTime(time.Now()),
					},
				}
			} else if phase == corev1.PodSucceeded {
				now := time.Now()
				containerState = corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						ExitCode:   0,
						Reason:     "Completed",
						StartedAt:  metav1.NewTime(now.Add(-1 * time.Minute)), // Assume 1 min execution
						FinishedAt: metav1.NewTime(now),
					},
				}
			} else if phase == corev1.PodFailed {
				now := time.Now()
				containerState = corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						ExitCode:   1,
						Reason:     "Error",
						Message:    message,
						StartedAt:  metav1.NewTime(now.Add(-1 * time.Minute)), // Assume 1 min execution
						FinishedAt: metav1.NewTime(now),
					},
				}
			}

			storedPod.Status.ContainerStatuses = append(storedPod.Status.ContainerStatuses, corev1.ContainerStatus{
				Name:  containerName,
				State: containerState,
				Ready: phase == corev1.PodRunning,
			})
		}

		// Push status update to Kubernetes API
		// This is critical - without this, pods show as Pending in Kubernetes
		// and no service endpoints are created for the webhooks
		_, err := p.clientset.CoreV1().Pods(storedPod.Namespace).UpdateStatus(context.Background(), storedPod, metav1.UpdateOptions{})
		if err != nil {
			fmt.Printf("⚠️  Failed to update pod status in Kubernetes for %s: %v\n", key, err)
		} else {
			fmt.Printf("✅ Updated pod status in Kubernetes: %s -> %s\n", key, phase)
		}
	}
}

// ConfigureNode sets up node information for the Virtual Kubelet
func (p *PodmanProvider) ConfigureNode(ctx context.Context, node *corev1.Node) {
	node.Status.Capacity = corev1.ResourceList{
		corev1.ResourceCPU:    *p.resourceQuantity("20"),
		corev1.ResourceMemory: *p.resourceQuantity("100Gi"),
		corev1.ResourcePods:   *p.resourceQuantity("110"),
	}
	node.Status.Allocatable = node.Status.Capacity

	node.Status.Conditions = []corev1.NodeCondition{
		{
			Type:               corev1.NodeReady,
			Status:             corev1.ConditionTrue,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletReady",
			Message:            "kubelet is ready",
		},
		{
			Type:               corev1.NodeMemoryPressure,
			Status:             corev1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletHasSufficientMemory",
			Message:            "kubelet has sufficient memory",
		},
		{
			Type:               corev1.NodeDiskPressure,
			Status:             corev1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletHasNoDiskPressure",
			Message:            "kubelet has no disk pressure",
		},
	}

	node.Status.NodeInfo = corev1.NodeSystemInfo{
		Architecture:    "amd64",
		OperatingSystem: p.operatingSystem,
		OSImage:         "Tekton Runner (Podman)",
		KubeletVersion:  "v1.31.0-vk",
	}
}

func (p *PodmanProvider) resourceQuantity(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

// getContainerIP gets the IP address of a container from Podman
func (p *PodmanProvider) getContainerIP(containerName string) string {
	// Use podman inspect to get container IP on tkn-runner-net network
	// Use full path to podman binary to avoid alias issues
	// Retry a few times since container might not be fully created yet
	for i := 0; i < 5; i++ {
		if i > 0 {
			time.Sleep(500 * time.Millisecond)
		}
		// Use index syntax for map keys with dashes in Go templates
		cmd := exec.Command("/opt/homebrew/bin/podman", "inspect", containerName, "--format", `{{(index .NetworkSettings.Networks "tkn-runner-net").IPAddress}}`)
		output, err := cmd.Output()
		if err == nil {
			ip := strings.TrimSpace(string(output))
			if ip != "" {
				fmt.Printf("✅ Got real IP for container %s: %s\n", containerName, ip)
				return ip
			}
		}
	}
	fmt.Printf("⚠️  Could not get IP for container %s after retries\n", containerName)
	return ""
}
