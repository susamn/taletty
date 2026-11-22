package audible

import "github.com/susamn/taletty/engine/connectors/audible/internal/bridge"

const (
	// ContractVersion is the independently released engine module/API contract version.
	ContractVersion = "v0.1.0"
	// BridgeProtocolVersion is the required JSON-lines bridge protocol version.
	BridgeProtocolVersion = bridge.Version
	// PinnedPythonVersion is the evidenced CPython minor version for the adapter.
	PinnedPythonVersion = "3.12"
	// PinnedAudibleVersion is the pinned audible Python package version.
	PinnedAudibleVersion = "0.13.1"
)
