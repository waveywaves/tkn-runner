package manifests

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

// InstallController installs the Tekton controller into the cluster
func InstallController(ctx context.Context, kubeconfig string) error {
	fmt.Println("Installing Tekton controller...")

	// Get the embedded manifest
	manifest := GetControllerManifest()

	// Write to a temp file for kubectl apply
	tmpFile, err := os.CreateTemp("", "tekton-controller-*.yaml")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write(manifest); err != nil {
		return fmt.Errorf("failed to write manifest: %w", err)
	}
	tmpFile.Close()

	// Apply using kubectl
	cmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", tmpFile.Name(), "--kubeconfig", kubeconfig)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to apply controller manifest: %w\nOutput: %s", err, string(output))
	}

	fmt.Println("Tekton controller manifests applied successfully!")

	// Wait for controller deployment to be ready
	if err := waitForController(ctx, kubeconfig); err != nil {
		return fmt.Errorf("controller failed to become ready: %w", err)
	}

	fmt.Println("✅ Tekton controller is ready!")
	return nil
}

// waitForController waits for the Tekton controller deployment to be ready
func waitForController(ctx context.Context, kubeconfig string) error {
	fmt.Println("Waiting for Tekton controller to be ready...")

	// Create k8s config
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to build kubeconfig: %w", err)
	}

	// Create dynamic client
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create dynamic client: %w", err)
	}

	// Wait for deployment to be ready
	deploymentGVR := schema.GroupVersionResource{
		Group:    "apps",
		Version:  "v1",
		Resource: "deployments",
	}

	return wait.PollUntilContextTimeout(ctx, 2*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		deployment, err := dynamicClient.Resource(deploymentGVR).
			Namespace("tekton-pipelines").
			Get(ctx, "tekton-pipelines-controller", metav1.GetOptions{})
		if err != nil {
			// Deployment might not exist yet
			return false, nil
		}

		// Check if deployment is ready
		status, found, err := unstructured.NestedMap(deployment.Object, "status")
		if err != nil || !found {
			return false, nil
		}

		replicas, found, err := unstructured.NestedInt64(status, "replicas")
		if err != nil || !found {
			return false, nil
		}

		readyReplicas, found, err := unstructured.NestedInt64(status, "readyReplicas")
		if err != nil || !found {
			return false, nil
		}

		return replicas == readyReplicas && replicas > 0, nil
	})
}

// ApplyManifestDirect applies a YAML manifest directly using the k8s client
// This is an alternative to kubectl apply that doesn't require external binary
func ApplyManifestDirect(ctx context.Context, kubeconfig string, manifest []byte) error {
	// Create k8s config
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to build kubeconfig: %w", err)
	}

	// Create dynamic client
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create dynamic client: %w", err)
	}

	// Split YAML into individual resources
	decoder := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)
	reader := bytes.NewReader(manifest)
	yamlReader := io.NopCloser(reader)

	// TODO: Parse and apply each resource
	// This is more complex but doesn't require kubectl
	_ = dynamicClient
	_ = decoder
	_ = yamlReader

	return fmt.Errorf("not implemented yet - use InstallController instead")
}
