package mesh

import "context"

// XrayApplier is the runtime contract the gRPC server uses to drive
// the local xray-core. Decoupled from web/service so server tests can
// substitute fakes; the production wiring in web/web.go connects it
// to XrayService.
//
// Mirrors the interface declared in mesh/server but lives here so
// other parts of the panel (heartbeat, status reports) can also use
// the same abstraction without importing mesh/server.
type XrayApplier interface {
	ApplyJSON(ctx context.Context, configJSON []byte) error
	CurrentVersion() string
	CurrentStatus() string
}
