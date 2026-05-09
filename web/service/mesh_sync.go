package service

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/mesh/client"
)

// MeshSyncService is the master-side glue between InboundService
// changes and the gRPC ApplyConfig calls that push the resulting
// per-node xray config to each remote node.
//
// In v1 the strategy is "rebuild and push everything on any change":
// every node's full config is regenerated and shipped. The
// ApplyConfigRequest.config_hash idempotency check on the node side
// turns the duplicate pushes into cheap no-ops.
//
// PushAll is safe to call from any goroutine; it serialises remote
// dials with an internal lock to keep ordering deterministic when
// multiple inbound mutations land in the same second.
type MeshSyncService struct {
	xrayService    *XrayService
	nodeService    *NodeService
	settingService SettingService

	mu sync.Mutex
}

// NewMeshSyncService wires the dependencies. The XrayService is the
// source of per-node config; NodeService gives us the registry and
// credentials to dial each remote node.
func NewMeshSyncService(xs *XrayService, ns *NodeService, ss SettingService) *MeshSyncService {
	return &MeshSyncService{
		xrayService:    xs,
		nodeService:    ns,
		settingService: ss,
	}
}

// PushAll regenerates and pushes the configuration to every paired
// remote node (status != "pending", != "local"). Errors per-node are
// logged and recorded on the node row but do not abort the loop —
// one broken node should not block the rest.
//
// Returns nil only if every push succeeded. The caller (cron tick)
// usually ignores the return; the per-node error is observable via
// node.last_error and the upcoming heartbeat status.
func (s *MeshSyncService) PushAll(ctx context.Context) error {
	if s.settingService.GetPanelMode() != PanelModeMaster {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	nodes, err := s.nodeService.GetAll()
	if err != nil {
		return err
	}

	var lastErr error
	for _, n := range nodes {
		if n.IsLocal {
			continue
		}
		if n.CaCertPem == "" || n.ClientCertPem == "" {
			// Not yet paired. Skip silently — Pair() handles this case.
			continue
		}
		if err := s.pushOne(ctx, n); err != nil {
			logger.Warningf("mesh sync: node %q (%d): %v", n.Name, n.Id, err)
			lastErr = err
		}
	}
	return lastErr
}

// PushNode regenerates and pushes only the named node's config. Used
// from the controller when the operator clicks "Force resync" on the
// Nodes UI (route added in a follow-up).
func (s *MeshSyncService) PushNode(ctx context.Context, nodeId int) error {
	n, err := s.nodeService.Get(nodeId)
	if err != nil {
		return err
	}
	if n.IsLocal {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pushOne(ctx, n)
}

func (s *MeshSyncService) pushOne(ctx context.Context, n *model.Node) error {
	cfg, err := s.xrayService.GetXrayConfigForNode(n.Id)
	if err != nil {
		s.nodeService.markError(n.Id, err)
		return err
	}
	payload, err := json.Marshal(cfg)
	if err != nil {
		s.nodeService.markError(n.Id, err)
		return err
	}
	hash := client.HashConfig(payload)

	// Avoid the ApplyConfig round-trip if we know the node already has
	// this exact payload. The node-side server also short-circuits on
	// matching hash, so this is just bandwidth optimisation.
	if hash == n.AppliedHash {
		return nil
	}

	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cli, err := client.Dial(dialCtx, n)
	if err != nil {
		s.nodeService.markError(n.Id, err)
		return err
	}
	defer cli.Close()

	resp, err := cli.ApplyConfig(dialCtx, hash, payload)
	if err != nil {
		s.nodeService.markError(n.Id, err)
		return err
	}
	if resp.GetError() != "" {
		// Node accepted the call but xray rejected the config (e.g.
		// invalid Reality dest, port already bound). Surface the error
		// to the operator without flipping status to disconnected —
		// the node is reachable, just unhappy.
		s.nodeService.markError(n.Id, errString(resp.GetError()))
		return errString(resp.GetError())
	}

	if err := s.nodeService.MarkSeen(n.Id, n.Version, resp.GetXrayVersion(), hash); err != nil {
		// Non-fatal: the push succeeded, only the bookkeeping update failed.
		logger.Warningf("mesh sync: failed to update last_seen for node %d: %v", n.Id, err)
	}
	return nil
}

// errString is a tiny error wrapper so we can return the node-reported
// error message verbatim without importing errors here.
type errString string

func (e errString) Error() string { return string(e) }
