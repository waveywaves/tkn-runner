package virtualkubelet

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

// VirtualKubelet manages a virtual node and handles pod lifecycle
type VirtualKubelet struct {
	nodeName   string
	kubeconfig string
	provider   *PodmanProvider
	clientset  *kubernetes.Clientset
}

// New creates a new Virtual Kubelet instance
func New(nodeName, kubeconfig string) (*VirtualKubelet, error) {
	// Create Kubernetes client
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to build kubeconfig: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create k8s client: %w", err)
	}

	// Create provider
	provider, err := NewPodmanProvider(nodeName, kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create podman provider: %w", err)
	}

	return &VirtualKubelet{
		nodeName:   nodeName,
		kubeconfig: kubeconfig,
		provider:   provider,
		clientset:  clientset,
	}, nil
}

// Run starts the Virtual Kubelet
func (vk *VirtualKubelet) Run(ctx context.Context) error {
	fmt.Println("🚀 Starting Virtual Kubelet...")
	fmt.Printf("   Node name: %s\n", vk.nodeName)

	// Register node
	if err := vk.registerNode(ctx); err != nil {
		return fmt.Errorf("failed to register node: %w", err)
	}

	// Start node status updater
	go vk.updateNodeStatusLoop(ctx)

	// Start pod watcher
	if err := vk.watchPods(ctx); err != nil {
		return fmt.Errorf("failed to watch pods: %w", err)
	}

	return nil
}

// registerNode creates or updates the Node resource in Kubernetes
func (vk *VirtualKubelet) registerNode(ctx context.Context) error {
	fmt.Println("📝 Registering node with Kubernetes...")

	// Delete existing node if it exists (fresh start each time)
	err := vk.clientset.CoreV1().Nodes().Delete(ctx, vk.nodeName, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		fmt.Printf("⚠️  Warning: failed to delete existing node: %v\n", err)
	}

	// Wait for deletion to complete (max 5 seconds)
	if err == nil {
		fmt.Println("⏳ Waiting for node deletion to complete...")
		for i := 0; i < 10; i++ {
			_, err := vk.clientset.CoreV1().Nodes().Get(ctx, vk.nodeName, metav1.GetOptions{})
			if errors.IsNotFound(err) {
				break // Node is deleted
			}
			time.Sleep(500 * time.Millisecond)
		}
	}

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: vk.nodeName,
			Labels: map[string]string{
				"type":                   "virtual-kubelet",
				"kubernetes.io/role":     "agent",
				"kubernetes.io/hostname": vk.nodeName,
			},
		},
		Spec: corev1.NodeSpec{
			// No taints - this is the ONLY node, all pods must schedule here
		},
	}

	// Configure node status
	vk.provider.ConfigureNode(ctx, node)

	// Create node
	_, err = vk.clientset.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create node: %w", err)
	}

	fmt.Println("✅ Node registered successfully!")
	return nil
}

// updateNodeStatusLoop periodically updates the node status (heartbeat)
func (vk *VirtualKubelet) updateNodeStatusLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := vk.updateNodeStatus(ctx); err != nil {
				fmt.Printf("⚠️  Failed to update node status: %v\n", err)
			}
		}
	}
}

// updateNodeStatus updates the node status (heartbeat)
func (vk *VirtualKubelet) updateNodeStatus(ctx context.Context) error {
	node, err := vk.clientset.CoreV1().Nodes().Get(ctx, vk.nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get node: %w", err)
	}

	// Update conditions with current timestamp
	for i := range node.Status.Conditions {
		node.Status.Conditions[i].LastHeartbeatTime = metav1.Now()
	}

	_, err = vk.clientset.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update node status: %w", err)
	}

	return nil
}

// watchPods watches for pods assigned to this node
func (vk *VirtualKubelet) watchPods(ctx context.Context) error {
	fmt.Println("👀 Watching for pods assigned to this node...")

	// Create pod watcher filtered by node name
	watchlist := cache.NewListWatchFromClient(
		vk.clientset.CoreV1().RESTClient(),
		"pods",
		corev1.NamespaceAll,
		fields.OneTermEqualSelector("spec.nodeName", vk.nodeName),
	)

	_, controller := cache.NewInformer(
		watchlist,
		&corev1.Pod{},
		0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				pod := obj.(*corev1.Pod)
				fmt.Printf("🆕 Pod assigned to node: %s/%s\n", pod.Namespace, pod.Name)
				if err := vk.provider.CreatePod(ctx, pod); err != nil {
					fmt.Printf("❌ Failed to create pod: %v\n", err)
				}
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				pod := newObj.(*corev1.Pod)
				if err := vk.provider.UpdatePod(ctx, pod); err != nil {
					fmt.Printf("❌ Failed to update pod: %v\n", err)
				}
			},
			DeleteFunc: func(obj interface{}) {
				pod := obj.(*corev1.Pod)
				fmt.Printf("🗑️  Pod deleted: %s/%s\n", pod.Namespace, pod.Name)
				if err := vk.provider.DeletePod(ctx, pod); err != nil {
					fmt.Printf("❌ Failed to delete pod: %v\n", err)
				}
			},
		},
	)

	// Start controller in background
	stopCh := make(chan struct{})
	go controller.Run(stopCh)

	// Wait for cache to sync
	fmt.Println("⏳ Waiting for pod informer cache to sync...")
	if !cache.WaitForCacheSync(ctx.Done(), controller.HasSynced) {
		close(stopCh)
		return fmt.Errorf("failed to sync pod informer cache")
	}
	fmt.Println("✅ Pod informer cache synced!")

	// Wait for context cancellation
	<-ctx.Done()
	close(stopCh)
	return nil
}
