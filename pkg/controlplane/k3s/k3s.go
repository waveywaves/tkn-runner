package k3s

import (
	"context"
	"runtime"

	"github.com/waveywaves/tkn-runner/pkg/config"
)

// ControlPlane defines the interface for k3s control plane implementations
type ControlPlane interface {
	Start(ctx context.Context) error
	Stop() error
	GetKubeconfig() string
}

// New creates a new k3s control plane instance based on the runtime OS
func New(cfg config.K3sConfig) ControlPlane {
	if runtime.GOOS == "darwin" {
		// macOS: use Podman container approach
		return NewPodman(cfg)
	}
	// Linux: use native binary approach
	return NewNative(cfg)
}
