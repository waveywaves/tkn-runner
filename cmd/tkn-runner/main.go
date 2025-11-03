package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/waveywaves/tkn-runner/pkg/config"
	"github.com/waveywaves/tkn-runner/pkg/controlplane/k3s"
	"github.com/waveywaves/tkn-runner/pkg/manifests"
	"github.com/waveywaves/tkn-runner/pkg/virtualkubelet"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: tkn-runner <command>")
		fmt.Println("Commands:")
		fmt.Println("  start     Start the tkn-runner control plane")
		fmt.Println("  delete    Delete all tkn-runner resources (containers, volumes, data)")
		os.Exit(1)
	}

	command := os.Args[1]

	switch command {
	case "start":
		if err := startCommand(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "delete":
		if err := deleteCommand(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Printf("Unknown command: %s\n", command)
		os.Exit(1)
	}
}

func startCommand() error {
	// Load default configuration
	cfg := config.DefaultConfig()

	// Create k3s control plane
	cp := k3s.New(cfg.ControlPlane.K3s)

	// Setup context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		fmt.Println("\nReceived shutdown signal...")
		cancel()
	}()

	// Start k3s
	if err := cp.Start(ctx); err != nil {
		return fmt.Errorf("failed to start control plane: %w", err)
	}

	// Cleanup on exit
	defer func() {
		if err := cp.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "Error stopping control plane: %v\n", err)
		}
	}()

	// Install Tekton CRDs
	fmt.Println()
	if err := manifests.InstallCRDs(ctx, cp.GetKubeconfig()); err != nil {
		return fmt.Errorf("failed to install Tekton CRDs: %w", err)
	}

	// Start Virtual Kubelet FIRST (so Tekton controller has a node to schedule on)
	fmt.Println()
	vk, err := virtualkubelet.New("tkn-runner-node", cp.GetKubeconfig())
	if err != nil {
		return fmt.Errorf("failed to create virtual kubelet: %w", err)
	}

	// Run VK in goroutine with error logging
	go func() {
		if err := vk.Run(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "Virtual Kubelet error: %v\n", err)
		}
	}()

	// Give VK a moment to register the node
	time.Sleep(2 * time.Second)

	// Install Tekton Controller (now it has a node to schedule on!)
	fmt.Println()
	if err := manifests.InstallController(ctx, cp.GetKubeconfig()); err != nil {
		return fmt.Errorf("failed to install Tekton controller: %w", err)
	}

	fmt.Println()
	fmt.Println("Tekton Runner is ready!")
	fmt.Printf("Use: export KUBECONFIG=%s\n", cp.GetKubeconfig())
	fmt.Println()

	// Wait for context cancellation
	<-ctx.Done()
	fmt.Println("Shutting down...")

	return nil
}

func deleteCommand() error {
	fmt.Println("🗑️  Deleting all tkn-runner resources...")
	fmt.Println()

	// Get configuration for paths
	cfg := config.DefaultConfig()

	// 1. Stop and remove k3s containers (try both old and new names for safety)
	fmt.Println("📦 Stopping and removing k3s containers...")
	containers := []string{"tkn-runner-k3s", "tekton-runner-k3s"}
	for _, container := range containers {
		// Try to stop
		cmd := exec.Command("podman", "stop", container)
		if err := cmd.Run(); err == nil {
			fmt.Printf("  ✓ Stopped %s\n", container)
		}

		// Try to remove
		cmd = exec.Command("podman", "rm", "-f", container)
		if err := cmd.Run(); err == nil {
			fmt.Printf("  ✓ Removed %s\n", container)
		}
	}

	// 2. Remove Podman networks
	fmt.Println()
	fmt.Println("🌐 Removing Podman networks...")
	networks := []string{"tkn-runner-net", "tekton-runner-net"}
	for _, network := range networks {
		cmd := exec.Command("podman", "network", "rm", network)
		if err := cmd.Run(); err == nil {
			fmt.Printf("  ✓ Removed %s\n", network)
		}
	}

	// 3. Remove volumes
	fmt.Println()
	fmt.Println("💾 Removing Podman volumes...")

	// Remove specific named volumes
	namedVolumes := []string{
		"tkn-runner-k3s-data",
		"tekton-runner-k3s-data",
	}
	for _, volume := range namedVolumes {
		cmd := exec.Command("podman", "volume", "rm", "-f", volume)
		if err := cmd.Run(); err == nil {
			fmt.Printf("  ✓ Removed %s\n", volume)
		}
	}

	// Prune all unused volumes (tekton-shared-*, tekton-vol-*)
	fmt.Println("  🧹 Pruning unused volumes...")
	cmd := exec.Command("podman", "volume", "prune", "-f")
	if output, err := cmd.CombinedOutput(); err == nil {
		fmt.Printf("  ✓ Pruned volumes\n")
	} else {
		fmt.Printf("  ⚠️  Volume prune: %s\n", output)
	}

	// 4. Remove data directories
	fmt.Println()
	fmt.Println("📁 Removing data directories...")

	homeDir, _ := os.UserHomeDir()
	dataDirs := []string{
		cfg.DataDir,                                    // ~/.tkn-runner
		filepath.Join(homeDir, ".tekton-runner"),       // Old name (if exists)
		filepath.Join(os.TempDir(), "tkn-runner-sa"),   // Service account temp files
		filepath.Join(os.TempDir(), "tekton-runner-sa"), // Old name
		filepath.Join(os.TempDir(), "tekton-downward"),  // Downward API temp files
	}

	for _, dir := range dataDirs {
		if err := os.RemoveAll(dir); err != nil {
			fmt.Printf("  ⚠️  Failed to remove %s: %v\n", dir, err)
		} else {
			fmt.Printf("  ✓ Removed %s\n", dir)
		}
	}

	// 5. Clean up any running step containers
	fmt.Println()
	fmt.Println("🐳 Cleaning up running containers...")
	cmd = exec.Command("podman", "ps", "-a", "--filter", "name=tekton-", "--format", "{{.Names}}")
	if output, err := cmd.Output(); err == nil && len(output) > 0 {
		// List of containers found, remove them all at once
		cmd = exec.Command("sh", "-c", "podman ps -a --filter name=tekton- --format '{{.Names}}' | xargs -r podman rm -f")
		if err := cmd.Run(); err == nil {
			fmt.Println("  ✓ Removed all Tekton containers")
		} else {
			fmt.Println("  ⚠️  Some containers may not have been removed")
		}
	} else {
		fmt.Println("  ✓ No Tekton containers found")
	}

	fmt.Println()
	fmt.Println("✅ All tkn-runner resources deleted!")
	fmt.Println()
	fmt.Println("💡 Tip: You can start fresh with 'tkn-runner start'")

	return nil
}
