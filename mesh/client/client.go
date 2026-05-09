// Package client is the master-side wrapper around the mesh gRPC
// service. The master keeps one Client per remote node and reuses it
// for ApplyConfig / GetStats / Heartbeat / etc.
//
// There are two distinct dial paths:
//
//  1. Pair (one-shot bootstrap): TLS where the node's server cert is
//     validated against the SHA-256 fingerprint extracted from the
//     pairing token, NOT against a CA. The master does not yet have
//     the node CA — the whole point of Pair is to obtain it.
//
//  2. Steady-state (everything else): mTLS where the node's server cert
//     must validate against the pinned node CA stored in nodes.ca_cert_pem,
//     and the master presents its own client cert (issued by that CA at
//     Pair time).
package client

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/mesh"
	"github.com/mhsanaei/3x-ui/v2/mesh/pb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Client is a thin wrapper over a gRPC connection to a single node.
// Cheap to construct; reuse across calls.
type Client struct {
	conn *grpc.ClientConn
	mesh pb.MeshClient
	node *model.Node
}

// Dial opens a steady-state mTLS connection to the given node, using
// credentials already stored in the Node row. Returns an error if the
// node has not been paired yet.
func Dial(ctx context.Context, n *model.Node) (*Client, error) {
	if n.CaCertPem == "" || n.ClientCertPem == "" || n.ClientKeyPem == "" {
		return nil, fmt.Errorf("node %q is not paired", n.Name)
	}

	cert, err := tls.X509KeyPair([]byte(n.ClientCertPem), []byte(n.ClientKeyPem))
	if err != nil {
		return nil, fmt.Errorf("master client cert: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(n.CaCertPem)) {
		return nil, errors.New("failed to load node CA into pool")
	}

	// We pin the node CA exactly — it belongs to one node and only
	// signs that node's server cert. SAN verification against the
	// configured ApiAddress (often a raw IP that wasn't in the cert's
	// SAN at generation time) would reject perfectly valid certs, so
	// we disable Go's default verifier and run our own that just walks
	// the chain to the pinned CA.
	tlsCfg := &tls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true, //nolint:gosec — replaced by VerifyConnection
		MinVersion:         tls.VersionTLS13,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("node presented no certificate")
			}
			leaf := cs.PeerCertificates[0]
			intermediates := x509.NewCertPool()
			for _, c := range cs.PeerCertificates[1:] {
				intermediates.AddCert(c)
			}
			_, err := leaf.Verify(x509.VerifyOptions{
				Roots:         pool,
				Intermediates: intermediates,
				KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			})
			return err
		},
	}

	addr := fmt.Sprintf("%s:%d", n.ApiAddress, n.ApiPort)
	conn, err := grpc.DialContext(ctx, addr,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithBlock(),
	)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	return &Client{
		conn: conn,
		mesh: pb.NewMeshClient(conn),
		node: n,
	}, nil
}

// Close releases the underlying connection. Safe to call multiple times.
func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	c.mesh = nil
	return err
}

// ApplyConfig pushes a full xray config to the node.
func (c *Client) ApplyConfig(ctx context.Context, configHash string, configJSON []byte) (*pb.ApplyConfigResponse, error) {
	return c.mesh.ApplyConfig(ctx, &pb.ApplyConfigRequest{
		ConfigHash: configHash,
		ConfigJson: configJSON,
	})
}

// HashConfig returns the SHA-256 hex of payload, matching the format
// expected by ApplyConfigRequest.config_hash.
func HashConfig(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// ----- Pair (bootstrap) -----

// PairResult carries the credentials produced by a successful pairing,
// for the caller to persist in the Node row.
type PairResult struct {
	NodeCAPEM        []byte
	SignedClientCert []byte
	NodeName         string
	NodeVersion      string
	XrayVersion      string
}

// Pair performs the one-shot bootstrap handshake against a node that
// the master has not previously connected to. tokenStr is the bootstrap
// token shown on the node's UI.
//
// On success the caller (NodeService) persists the returned PairResult
// fields into the Node row, alongside the master's own private key
// that produced masterClientCSRPEM.
func Pair(
	ctx context.Context,
	addr string,
	tokenStr string,
	masterCAPEM []byte,
	masterClientCSRPEM []byte,
	masterDisplayName string,
) (*PairResult, error) {
	tok, err := mesh.ParseToken(tokenStr)
	if err != nil {
		return nil, err
	}

	// Custom TLS: skip CA verification but enforce that the leaf cert's
	// SHA-256 matches the fingerprint encoded in the token. Equivalent
	// to certificate pinning.
	tlsCfg := &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec — overridden by VerifyConnection
		MinVersion:         tls.VersionTLS13,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("pair: node presented no certificate")
			}
			leaf := cs.PeerCertificates[0]
			sum := sha256.Sum256(leaf.Raw)
			actual := hex.EncodeToString(sum[:])
			if actual != tok.NodeCertFingerprint {
				return fmt.Errorf("pair: node cert fingerprint %s does not match token %s", actual, tok.NodeCertFingerprint)
			}
			return nil
		},
	}

	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(dialCtx, addr,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithBlock(),
	)
	if err != nil {
		return nil, fmt.Errorf("pair dial %s: %w", addr, err)
	}
	defer conn.Close()

	cli := pb.NewMeshClient(conn)
	resp, err := cli.Pair(dialCtx, &pb.PairRequest{
		Token:              tokenStr,
		MasterCaPem:        masterCAPEM,
		MasterClientCsrPem: masterClientCSRPEM,
		MasterName:         masterDisplayName,
	})
	if err != nil {
		return nil, fmt.Errorf("pair rpc: %w", err)
	}
	if len(resp.GetNodeCaPem()) == 0 || len(resp.GetSignedMasterClientCertPem()) == 0 {
		return nil, errors.New("pair: node returned empty cert material")
	}
	if !verifyCertChain(resp.GetSignedMasterClientCertPem(), resp.GetNodeCaPem()) {
		return nil, errors.New("pair: returned client cert does not chain to returned node CA")
	}

	return &PairResult{
		NodeCAPEM:        resp.GetNodeCaPem(),
		SignedClientCert: resp.GetSignedMasterClientCertPem(),
		NodeName:         resp.GetNodeName(),
		NodeVersion:      resp.GetNodeVersion(),
		XrayVersion:      resp.GetXrayVersion(),
	}, nil
}

func verifyCertChain(leafPEM, caPEM []byte) bool {
	block, _ := pem.Decode(leafPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return false
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return false
	}
	_, err = leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	return err == nil
}
