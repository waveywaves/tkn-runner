package manifests

import (
	"embed"
	"fmt"
	"io/fs"
	"path/filepath"
)

//go:embed crds
var crdsFS embed.FS

//go:embed controller/release.yaml
var controllerManifest []byte

// GetCRDs returns a list of all embedded CRD YAML files
func GetCRDs() ([]CRDFile, error) {
	var crds []CRDFile

	// Read all files from the embedded filesystem
	entries, err := fs.ReadDir(crdsFS, "crds")
	if err != nil {
		return nil, fmt.Errorf("failed to read embedded CRDs directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		// Only process YAML files
		if filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}

		// Read the file content
		content, err := fs.ReadFile(crdsFS, filepath.Join("crds", entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("failed to read CRD file %s: %w", entry.Name(), err)
		}

		crds = append(crds, CRDFile{
			Name:    entry.Name(),
			Content: content,
		})
	}

	return crds, nil
}

// CRDFile represents a CRD YAML file
type CRDFile struct {
	Name    string
	Content []byte
}

// GetControllerManifest returns the embedded Tekton controller manifest
func GetControllerManifest() []byte {
	return controllerManifest
}
