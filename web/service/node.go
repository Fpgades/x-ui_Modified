// Package service: NodeService manages the `nodes` table on a
// master-mode panel and orchestrates the cryptographic handshake with
// a remote node-mode panel.
//
// On standalone-mode panels NodeService is still usable (it reads the
// synthetic id=1 row), but Pair/Delete on remote nodes is rejected.
package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/mesh/client"
	"github.com/mhsanaei/3x-ui/v2/mesh/pki"
)

// NodeService is the master-mode operator-facing API for managing
// remote nodes. All gRPC orchestration is hidden behind these methods.
type NodeService struct {
	settingService SettingService
}

// localNodeID is the synthetic "this panel" row planted at boot. See
// docs/MESH_DESIGN.md §3.
const localNodeID = 1

// Settings keys used to persist the master's own CA. Generated lazily
// the first time a node is paired. We store them as settings rather
// than introducing a `master_identity` table because there's exactly
// one master CA per install — same shape as a singleton.
const (
	keyMasterCaCert = "meshMasterCaCertPem"
	keyMasterCaKey  = "meshMasterCaKeyPem"
	keyMasterName   = "meshMasterName"
)

// GetAll returns all nodes, including the synthetic id=1 local row.
// Callers that want only "remote" nodes should filter IsLocal=false.
func (s *NodeService) GetAll() ([]*model.Node, error) {
	db := database.GetDB()
	var nodes []*model.Node
	if err := db.Order("id ASC").Find(&nodes).Error; err != nil {
		return nil, err
	}
	return nodes, nil
}

// Get returns a single node by id.
func (s *NodeService) Get(id int) (*model.Node, error) {
	db := database.GetDB()
	var n model.Node
	if err := db.First(&n, id).Error; err != nil {
		return nil, err
	}
	return &n, nil
}

// CreateRemoteNode inserts a fresh row in 'pending' status and returns
// it. The row holds the connection target only — no credentials yet;
// those are populated by Pair.
func (s *NodeService) CreateRemoteNode(name, address string, port int, apiAddress string, apiPort int) (*model.Node, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("node name required")
	}
	if name == "local" {
		return nil, errors.New("name 'local' is reserved for the synthetic node row")
	}
	if strings.TrimSpace(apiAddress) == "" {
		apiAddress = address
	}
	if apiPort <= 0 {
		v, err := s.settingService.GetMeshNodeApiPort()
		if err != nil || v <= 0 {
			v = 62050
		}
		apiPort = v
	}

	db := database.GetDB()
	n := &model.Node{
		Name:       name,
		Address:    address,
		Port:       port,
		ApiAddress: apiAddress,
		ApiPort:    apiPort,
		Status:     model.NodeStatusPending,
		IsLocal:    false,
	}
	if err := db.Create(n).Error; err != nil {
		return nil, err
	}
	return n, nil
}

// UpdateNode patches the editable fields on a Node row. For the local
// node only name is meaningful; for remote nodes the operator may also
// retarget address/port/apiAddress/apiPort (e.g. when the node moves
// to a new IP).
//
// Empty strings/zero ints in the patch mean "leave unchanged", except
// for name which must always be present and non-empty.
func (s *NodeService) UpdateNode(id int, name, address string, port int, apiAddress string, apiPort int) (*model.Node, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("node name cannot be empty")
	}
	n, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	updates := map[string]any{
		"name": name,
	}
	if !n.IsLocal {
		if address != "" {
			updates["address"] = address
		}
		if port > 0 {
			updates["port"] = port
		}
		if apiAddress != "" {
			updates["api_address"] = apiAddress
		}
		if apiPort > 0 {
			updates["api_port"] = apiPort
		}
	}
	if err := database.GetDB().Model(&model.Node{}).Where("id = ?", id).Updates(updates).Error; err != nil {
		return nil, err
	}
	return s.Get(id)
}

// Delete removes a remote node row. Refuses to delete the synthetic
// local node. Caller is responsible for first reassigning any inbounds
// that reference this node — we enforce a hard error here as a safety
// net.
func (s *NodeService) Delete(id int) error {
	if id == localNodeID {
		return errors.New("the local node cannot be deleted")
	}

	db := database.GetDB()
	var count int64
	if err := db.Model(&model.Inbound{}).Where("node_id = ?", id).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("node has %d inbounds assigned; reassign them first", count)
	}
	return db.Delete(&model.Node{}, id).Error
}

// Pair runs the bootstrap handshake against a node row that's still in
// 'pending' status. On success, the row is updated with pinned cert
// material and flipped to 'connected'.
//
// nodeID:    the ID of the node row created by CreateRemoteNode
// token:     the bootstrap token shown by the remote node's UI
// timeout:   max time to spend on the handshake (TLS dial + RPC)
func (s *NodeService) Pair(ctx context.Context, nodeID int, token string, timeout time.Duration) error {
	if nodeID == localNodeID {
		return errors.New("cannot pair with the local node")
	}

	n, err := s.Get(nodeID)
	if err != nil {
		return fmt.Errorf("node %d not found: %w", nodeID, err)
	}
	if n.IsLocal {
		return errors.New("cannot pair with the local node")
	}

	masterCA, err := s.ensureMasterCA()
	if err != nil {
		return fmt.Errorf("master CA: %w", err)
	}
	masterName, err := s.getOrSetMasterName()
	if err != nil {
		return fmt.Errorf("master name: %w", err)
	}

	csrPEM, clientKeyPEM, err := pki.GenerateClientCSR("master:" + masterName)
	if err != nil {
		return fmt.Errorf("generate master CSR: %w", err)
	}

	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	addr := fmt.Sprintf("%s:%d", n.ApiAddress, n.ApiPort)
	res, err := client.Pair(ctx, addr, token, masterCA.CertPEM, csrPEM, masterName)
	if err != nil {
		s.markError(nodeID, err)
		return err
	}

	db := database.GetDB()
	if err := db.Model(&model.Node{}).Where("id = ?", nodeID).Updates(map[string]any{
		"ca_cert_pem":     string(res.NodeCAPEM),
		"client_cert_pem": string(res.SignedClientCert),
		"client_key_pem":  string(clientKeyPEM),
		"status":          model.NodeStatusConnected,
		"version":         res.NodeVersion,
		"xray_version":    res.XrayVersion,
		"last_seen":       time.Now().UnixMilli(),
		"last_error":      "",
	}).Error; err != nil {
		return fmt.Errorf("persist pairing: %w", err)
	}

	logger.Infof("mesh: paired with node %q at %s", n.Name, addr)
	return nil
}

// markError flips a node's status to 'error' and records the message,
// best-effort. Used by Pair / heartbeat / ApplyConfig failure paths.
func (s *NodeService) markError(id int, e error) {
	if e == nil {
		return
	}
	db := database.GetDB()
	_ = db.Model(&model.Node{}).Where("id = ?", id).Updates(map[string]any{
		"status":     model.NodeStatusError,
		"last_error": e.Error(),
	}).Error
}

// MarkSeen updates last_seen on a heartbeat. Called by the heartbeat
// goroutine after a successful Heartbeat round trip.
func (s *NodeService) MarkSeen(id int, version, xrayVersion, appliedHash string) error {
	db := database.GetDB()
	return db.Model(&model.Node{}).Where("id = ?", id).Updates(map[string]any{
		"status":       model.NodeStatusConnected,
		"last_seen":    time.Now().UnixMilli(),
		"version":      version,
		"xray_version": xrayVersion,
		"applied_hash": appliedHash,
		"last_error":   "",
	}).Error
}

// ensureMasterCA returns the master CA, generating + persisting it on
// first call. Idempotent.
func (s *NodeService) ensureMasterCA() (*pki.CA, error) {
	cert, err := s.settingService.getString(keyMasterCaCert)
	keyPEM, err2 := s.settingService.getString(keyMasterCaKey)
	if err == nil && err2 == nil && cert != "" && keyPEM != "" {
		return &pki.CA{CertPEM: []byte(cert), KeyPEM: []byte(keyPEM)}, nil
	}

	hostname, _ := net.LookupAddr("127.0.0.1")
	cn := "xui-mesh-master"
	if len(hostname) > 0 {
		cn = "xui-mesh-master-" + hostname[0]
	}
	ca, err := pki.GenerateCA(cn)
	if err != nil {
		return nil, err
	}
	if err := s.settingService.setString(keyMasterCaCert, string(ca.CertPEM)); err != nil {
		return nil, err
	}
	if err := s.settingService.setString(keyMasterCaKey, string(ca.KeyPEM)); err != nil {
		return nil, err
	}
	logger.Info("mesh: generated master CA")
	return ca, nil
}

func (s *NodeService) getOrSetMasterName() (string, error) {
	v, err := s.settingService.getString(keyMasterName)
	if err == nil && v != "" {
		return v, nil
	}
	hostname, _ := net.LookupAddr("127.0.0.1")
	name := "master"
	if len(hostname) > 0 {
		name = hostname[0]
	}
	if err := s.settingService.setString(keyMasterName, name); err != nil {
		return name, err
	}
	return name, nil
}
