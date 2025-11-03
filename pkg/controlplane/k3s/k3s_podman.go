package k3s

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/waveywaves/tkn-runner/pkg/config"
)

const (
	k3sImage         = "rancher/k3s:v1.31.4-k3s1"
	containerName    = "tkn-runner-k3s"
	k3sPort          = "6443"
)

// K3sPodman manages k3s running in a Podman container (for macOS)
type K3sPodman struct {
	config     config.K3sConfig
	kubeconfig string
}

// NewPodman creates a new K3sPodman instance
func NewPodman(cfg config.K3sConfig) *K3sPodman {
	return &K3sPodman{
		config:     cfg,
		kubeconfig: filepath.Join(cfg.DataDir, "k3s.yaml"),
	}
}

// Start starts k3s in a Podman container
func (k *K3sPodman) Start(ctx context.Context) error {
	// Create data directory
	if err := os.MkdirAll(k.config.DataDir, 0755); err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}

	// Check if container already exists
	exists, err := k.containerExists()
	if err != nil {
		return fmt.Errorf("failed to check container existence: %w", err)
	}

	if exists {
		fmt.Println("Existing k3s container found, starting it...")
		if err := k.startExistingContainer(); err != nil {
			return fmt.Errorf("failed to start existing container: %w", err)
		}
	} else {
		fmt.Println("Creating new k3s container...")
		if err := k.createContainer(); err != nil {
			return fmt.Errorf("failed to create container: %w", err)
		}
	}

	// Wait for k3s to be ready
	if err := k.waitForReady(); err != nil {
		return fmt.Errorf("k3s failed to become ready: %w", err)
	}

	// Extract and fix kubeconfig
	if err := k.setupKubeconfig(); err != nil {
		return fmt.Errorf("failed to setup kubeconfig: %w", err)
	}

	fmt.Println("k3s control plane is ready!")
	fmt.Printf("KUBECONFIG: %s\n", k.kubeconfig)

	return nil
}

// containerExists checks if the k3s container already exists
func (k *K3sPodman) containerExists() (bool, error) {
	cmd := exec.Command("podman", "ps", "-a", "--filter", "name=^"+containerName+"$", "--format", "{{.Names}}")
	output, err := cmd.Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(output)) == containerName, nil
}

// startExistingContainer starts an existing stopped container
func (k *K3sPodman) startExistingContainer() error {
	cmd := exec.Command("podman", "start", containerName)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// createContainer creates a new k3s container
func (k *K3sPodman) createContainer() error {
	// Create a named volume for k3s data (instead of bind mount)
	volumeName := "tkn-runner-k3s-data"

	// Create volume if it doesn't exist
	volCmd := exec.Command("podman", "volume", "create", volumeName)
	volCmd.Run() // Ignore error if volume already exists

	// Create shared Podman network for inter-container communication
	networkName := "tkn-runner-net"
	netCmd := exec.Command("podman", "network", "create", networkName)
	netCmd.Run() // Ignore error if network already exists

	args := []string{
		"run",
		"-d",
		"--name", containerName,
		"--privileged",
		"--cgroupns=host",
		"--network", networkName, // Use shared network instead of host network
		"-p", k3sPort + ":" + k3sPort,
		"-v", volumeName + ":/var/lib/rancher/k3s",
		"--restart", "unless-stopped", // Auto-restart on crash
		"--memory", "1g",               // Limit memory to 1GB
		"--cpus", "2",                  // Limit to 2 CPUs
		k3sImage,
		"server",
		"--disable-agent",
		"--flannel-backend=none",
		"--disable-cloud-controller",
		"--disable-network-policy",
		"--disable-kube-proxy",
		"--tls-san=host.containers.internal",      // For backward compatibility
		"--tls-san=tkn-runner-k3s",                // TLS SAN for container DNS name
		"--tls-san=10.89.0.0/16",                  // TLS SAN for the Podman network subnet
	}

	// Add disabled components
	if len(k.config.DisableComponents) > 0 {
		args = append(args, "--disable="+strings.Join(k.config.DisableComponents, ","))
	}

	cmd := exec.Command("podman", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	fmt.Println("Starting k3s control plane in Podman...")
	return cmd.Run()
}

// setupKubeconfig extracts kubeconfig from container and fixes the server URL
func (k *K3sPodman) setupKubeconfig() error {
	// Copy kubeconfig from container
	cmd := exec.Command("podman", "cp", containerName+":/etc/rancher/k3s/k3s.yaml", k.kubeconfig)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to copy kubeconfig: %w", err)
	}

	// Read kubeconfig
	content, err := os.ReadFile(k.kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to read kubeconfig: %w", err)
	}

	// Replace server URL to use localhost
	fixed := strings.ReplaceAll(string(content), "https://127.0.0.1:6443", "https://127.0.0.1:"+k3sPort)
	fixed = strings.ReplaceAll(fixed, "server: https://0.0.0.0:6443", "server: https://127.0.0.1:"+k3sPort)

	// Handle any other potential internal IPs
	lines := strings.Split(fixed, "\n")
	for i, line := range lines {
		if strings.Contains(line, "server: https://") && !strings.Contains(line, "127.0.0.1") {
			lines[i] = "    server: https://127.0.0.1:" + k3sPort
		}
	}
	fixed = strings.Join(lines, "\n")

	// Write back
	if err := os.WriteFile(k.kubeconfig, []byte(fixed), 0600); err != nil {
		return fmt.Errorf("failed to write kubeconfig: %w", err)
	}

	return nil
}

// waitForReady waits for k3s to be ready
func (k *K3sPodman) waitForReady() error {
	timeout := time.After(90 * time.Second)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	fmt.Println("Waiting for k3s to be ready...")

	for {
		select {
		case <-timeout:
			return fmt.Errorf("timeout waiting for k3s to be ready")
		case <-ticker.C:
			// Check if k3s.yaml exists in container
			cmd := exec.Command("podman", "exec", containerName, "test", "-f", "/etc/rancher/k3s/k3s.yaml")
			if err := cmd.Run(); err == nil {
				// Give it a bit more time to fully initialize
				time.Sleep(2 * time.Second)
				return nil
			}
		}
	}
}

// Stop stops the k3s container (but doesn't remove it)
func (k *K3sPodman) Stop() error {
	fmt.Println("Stopping k3s container...")
	cmd := exec.Command("podman", "stop", containerName)
	return cmd.Run()
}

// GetKubeconfig returns the path to the kubeconfig file
func (k *K3sPodman) GetKubeconfig() string {
	return k.kubeconfig
}
