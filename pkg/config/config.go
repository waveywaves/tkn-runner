package config

import (
	"os"
	"path/filepath"
)

// Config represents the configuration for tkn-runner
type Config struct {
	ControlPlane ControlPlaneConfig `yaml:"control_plane"`
	DataDir      string             `yaml:"data_dir"`
}

// ControlPlaneConfig represents the control plane configuration
type ControlPlaneConfig struct {
	Type string     `yaml:"type"`
	K3s  K3sConfig  `yaml:"k3s"`
}

// K3sConfig represents k3s-specific configuration
type K3sConfig struct {
	DataDir           string   `yaml:"data_dir"`
	DisableComponents []string `yaml:"disable_components"`
}

// DefaultConfig returns a default configuration
func DefaultConfig() *Config {
	homeDir, _ := os.UserHomeDir()
	dataDir := filepath.Join(homeDir, ".tkn-runner")

	return &Config{
		ControlPlane: ControlPlaneConfig{
			Type: "k3s",
			K3s: K3sConfig{
				DataDir: filepath.Join(dataDir, "k3s"),
				DisableComponents: []string{
					"traefik",
					"servicelb",
					"local-storage",
					"metrics-server",
				},
			},
		},
		DataDir: dataDir,
	}
}
