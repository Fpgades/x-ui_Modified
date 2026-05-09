package mesh

import (
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/mesh/pki"
)

// IdentityID is the singleton row id for node identity. There is at
// most one row in node_identities; we hard-pin it to 1 (matches the
// `check:id = 1` constraint in the model).
const IdentityID = 1

// LoadIdentity fetches the singleton node identity. Returns
// (nil, NotFound err) if not yet initialised — callers that want
// lazy creation should use EnsureIdentity instead.
func LoadIdentity() (*model.NodeIdentity, error) {
	db := database.GetDB()
	var id model.NodeIdentity
	if err := db.First(&id, IdentityID).Error; err != nil {
		return nil, err
	}
	return &id, nil
}

// EnsureIdentity returns the existing node identity, generating a
// fresh CA + server cert + name on first call.
//
// Note: this *creates* identity material whenever it's missing. Calling
// this in standalone mode is harmless but pointless: the identity is
// only used when the panel is in node mode. main.go/web.Server should
// only invoke this on the node-mode boot path.
func EnsureIdentity() (*model.NodeIdentity, error) {
	if id, err := LoadIdentity(); err == nil {
		return id, nil
	}

	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "node"
	}

	ca, err := pki.GenerateCA("xui-mesh-node-" + hostname)
	if err != nil {
		return nil, err
	}
	srv, err := pki.GenerateServerCert(
		ca,
		hostname,
		[]string{hostname, "localhost"},
		[]net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	)
	if err != nil {
		return nil, err
	}

	id := &model.NodeIdentity{
		Id:            IdentityID,
		NodeName:      hostname,
		CaCertPem:     string(ca.CertPEM),
		CaKeyPem:      string(ca.KeyPEM),
		ServerCertPem: string(srv.CertPEM),
		ServerKeyPem:  string(srv.KeyPEM),
	}
	if err := database.GetDB().Create(id).Error; err != nil {
		return nil, fmt.Errorf("create node identity: %w", err)
	}
	return id, nil
}

// MintBootstrapToken generates a fresh pairing token, persists it on
// the singleton identity row, and returns the raw token to display in
// the UI. Replaces any previous unused token.
//
// The token is bound to the node's current server cert fingerprint;
// it cannot be reused after a cert rotation, by design.
func MintBootstrapToken(ttl time.Duration) (string, error) {
	id, err := EnsureIdentity()
	if err != nil {
		return "", err
	}

	tok, err := MintToken([]byte(id.ServerCertPem))
	if err != nil {
		return "", err
	}

	if ttl <= 0 {
		ttl = BootstrapTokenTTL
	}
	expiry := time.Now().Add(ttl).UnixMilli()

	if err := database.GetDB().Model(&model.NodeIdentity{}).
		Where("id = ?", IdentityID).
		Updates(map[string]any{
			"bootstrap_token":  tok,
			"bootstrap_expiry": expiry,
		}).Error; err != nil {
		return "", err
	}

	return tok, nil
}

// FinalizePair persists the pinned master fingerprint+name and burns
// the bootstrap token. Called from the gRPC server's Pair handler
// after the master's CSR has been signed.
func FinalizePair(masterFingerprint, masterName string) error {
	if masterFingerprint == "" {
		return errors.New("empty master fingerprint")
	}
	now := time.Now().UnixMilli()
	return database.GetDB().Model(&model.NodeIdentity{}).
		Where("id = ?", IdentityID).
		Updates(map[string]any{
			"master_client_fingerprint": masterFingerprint,
			"master_name":               masterName,
			"master_paired_at":          now,
			"bootstrap_token":           "",
			"bootstrap_expiry":          0,
		}).Error
}

// Unpair clears the pinned master and the keypair-related fields.
// Called when the operator hits "Unpair" in node-mode UI before
// switching back to standalone. Identity itself is preserved (so a
// subsequent re-pair to a different master can reuse the same
// generated CA).
func Unpair() error {
	return database.GetDB().Model(&model.NodeIdentity{}).
		Where("id = ?", IdentityID).
		Updates(map[string]any{
			"master_client_fingerprint": "",
			"master_name":               "",
			"master_paired_at":          0,
			"bootstrap_token":           "",
			"bootstrap_expiry":          0,
		}).Error
}
