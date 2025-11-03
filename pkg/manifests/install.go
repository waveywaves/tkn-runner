package manifests

import (
	"context"
	"fmt"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/tools/clientcmd"
)

// InstallCRDs installs all embedded Tekton CRDs into the Kubernetes cluster
func InstallCRDs(ctx context.Context, kubeconfig string) error {
	// Create k8s client
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to build kubeconfig: %w", err)
	}

	apiextensionsClient, err := apiextensionsclientset.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create apiextensions client: %w", err)
	}

	// Get all embedded CRDs
	crdFiles, err := GetCRDs()
	if err != nil {
		return fmt.Errorf("failed to get embedded CRDs: %w", err)
	}

	fmt.Printf("Installing %d Tekton CRDs...\n", len(crdFiles))

	// Install each CRD
	for _, crdFile := range crdFiles {
		if err := installCRD(ctx, apiextensionsClient, crdFile); err != nil {
			return fmt.Errorf("failed to install CRD %s: %w", crdFile.Name, err)
		}
		fmt.Printf("  ✓ Installed %s\n", crdFile.Name)
	}

	fmt.Println("All CRDs installed successfully!")
	return nil
}

func installCRD(ctx context.Context, client apiextensionsclientset.Interface, crdFile CRDFile) error {
	// Parse YAML to CRD object
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(crdFile.Content, &crd); err != nil {
		return fmt.Errorf("failed to unmarshal CRD YAML: %w", err)
	}

	// Try to create the CRD
	_, err := client.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, &crd, metav1.CreateOptions{})
	if err != nil {
		// If it already exists, update it
		if apierrors.IsAlreadyExists(err) {
			// Get the existing CRD to preserve resourceVersion
			existing, getErr := client.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, crd.Name, metav1.GetOptions{})
			if getErr != nil {
				return fmt.Errorf("failed to get existing CRD: %w", getErr)
			}

			// Preserve resourceVersion for update
			crd.ResourceVersion = existing.ResourceVersion

			// Update the CRD
			_, err = client.ApiextensionsV1().CustomResourceDefinitions().Update(ctx, &crd, metav1.UpdateOptions{})
			if err != nil {
				return fmt.Errorf("failed to update existing CRD: %w", err)
			}
			return nil // Successfully updated
		}
		return fmt.Errorf("failed to create CRD: %w", err)
	}

	return nil // Successfully created
}
