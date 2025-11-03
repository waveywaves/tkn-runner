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
	"github.com/waveywaves/tkn-runner/pkg/embedded"
)

// K3sNative manages the k3s control plane using native binary (Linux)
type K3sNative struct {
	config     config.K3sConfig
	k3sPath    string
	kubeconfig string
	cmd        *exec.Cmd
}

// NewNative creates a new K3sNative instance
func NewNative(cfg config.K3sConfig) *K3sNative {
	return &K3sNative{
		config:     cfg,
		kubeconfig: filepath.Join(cfg.DataDir, "k3s.yaml"),
	}
}

// Start starts the k3s control plane
func (k *K3sNative) Start(ctx context.Context) error {
	// Create data directory
	if err := os.MkdirAll(k.config.DataDir, 0755); err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}

	// Extract k3s binary to data directory
	k.k3sPath = filepath.Join(k.config.DataDir, "k3s")
	if err := os.WriteFile(k.k3sPath, embedded.K3sBinary, 0755); err != nil {
		return fmt.Errorf("failed to write k3s binary: %w", err)
	}

	// Build k3s server command
	args := []string{
		"server",
		"--data-dir=" + k.config.DataDir,
		"--write-kubeconfig=" + k.kubeconfig,
		"--disable-cloud-controller",
		"--disable-network-policy",
	}

	// Add disabled components
	if len(k.config.DisableComponents) > 0 {
		args = append(args, "--disable="+strings.Join(k.config.DisableComponents, ","))
	}

	// Start k3s server
	k.cmd = exec.CommandContext(ctx, k.k3sPath, args...)
	k.cmd.Stdout = os.Stdout
	k.cmd.Stderr = os.Stderr

	fmt.Println("Starting k3s control plane...")
	if err := k.cmd.Start(); err != nil {
		return fmt.Errorf("failed to start k3s: %w", err)
	}

	// Wait for k3s to be ready
	if err := k.waitForReady(); err != nil {
		return fmt.Errorf("k3s failed to become ready: %w", err)
	}

	fmt.Println("k3s control plane is ready!")
	fmt.Printf("KUBECONFIG: %s\n", k.kubeconfig)

	return nil
}

// waitForReady waits for k3s to be ready
func (k *K3sNative) waitForReady() error {
	timeout := time.After(60 * time.Second)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return fmt.Errorf("timeout waiting for k3s to be ready")
		case <-ticker.C:
			// Check if kubeconfig exists
			if _, err := os.Stat(k.kubeconfig); err == nil {
				// Try to run kubectl get nodes
				cmd := exec.Command("kubectl", "--kubeconfig", k.kubeconfig, "get", "nodes")
				if err := cmd.Run(); err == nil {
					return nil
				}
			}
		}
	}
}

// Stop stops the k3s control plane
func (k *K3sNative) Stop() error {
	if k.cmd != nil && k.cmd.Process != nil {
		fmt.Println("Stopping k3s control plane...")
		return k.cmd.Process.Kill()
	}
	return nil
}

// GetKubeconfig returns the path to the kubeconfig file
func (k *K3sNative) GetKubeconfig() string {
	return k.kubeconfig
}
