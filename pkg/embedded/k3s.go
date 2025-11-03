package embedded

import (
	_ "embed"
)

//go:embed k3s-binary
var K3sBinary []byte
