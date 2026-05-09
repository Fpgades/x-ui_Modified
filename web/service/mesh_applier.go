package service

import (
	"context"

	"github.com/mhsanaei/3x-ui/v2/mesh"
)

// MeshXrayApplier adapts XrayService to mesh.XrayApplier so the
// node-side gRPC server (mesh/server) can drive the local xray-core
// without importing web/service directly.
//
// Construct via NewMeshXrayApplier(&xrayService).
type MeshXrayApplier struct {
	xs *XrayService
}

// NewMeshXrayApplier wraps an existing XrayService.
func NewMeshXrayApplier(xs *XrayService) *MeshXrayApplier {
	return &MeshXrayApplier{xs: xs}
}

// ApplyJSON forwards to XrayService.RestartXrayWithRawConfig. The
// context is not currently used (XrayService doesn't take one) but is
// part of the interface so we can add timeouts later without breaking
// callers.
func (a *MeshXrayApplier) ApplyJSON(_ context.Context, configJSON []byte) error {
	return a.xs.RestartXrayWithRawConfig(configJSON)
}

// CurrentVersion reports the version string from the running xray
// process. Empty string when xray is not running.
func (a *MeshXrayApplier) CurrentVersion() string {
	return a.xs.GetXrayVersion()
}

// CurrentStatus is a short status word that the master surfaces to the
// operator on the Nodes page.
func (a *MeshXrayApplier) CurrentStatus() string {
	if a.xs.IsXrayRunning() {
		return "running"
	}
	if a.xs.DidXrayCrash() {
		return "errored"
	}
	return "stopped"
}

// Compile-time check that we satisfy the interface.
var _ mesh.XrayApplier = (*MeshXrayApplier)(nil)
