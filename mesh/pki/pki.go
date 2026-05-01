// Package pki provides minimal X.509 helpers for the mesh control plane.
//
// We deliberately keep this small and self-contained: only the few
// operations the master/node pairing flow needs, no CA-management UI,
// no revocation lists. mTLS roles:
//
//   - Each node generates its own CA at first switch into node mode.
//     This CA signs the node's gRPC server cert, and (during Pair)
//     signs the master's mTLS client CSR.
//   - Each master generates its own CA at first switch into master mode.
//     This CA signs the master's gRPC client cert that the master
//     presents to nodes.
//
// Pinning replaces classical PKI: master pins node CA on Pair, node
// pins master CA on Pair. There is no shared root.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"time"
)

const (
	// caValidity is how long a freshly generated CA is valid for. We
	// pick 10 years so operators don't have to think about rotation
	// during normal use; rotation is supported by re-pairing.
	caValidity = 10 * 365 * 24 * time.Hour
	// leafValidity is for server / client certs.
	leafValidity = 5 * 365 * 24 * time.Hour
)

// KeyPair holds PEM-encoded cert + key.
type KeyPair struct {
	CertPEM []byte
	KeyPEM  []byte
}

// CA is a generated certificate authority — cert and private key both PEM.
type CA struct {
	CertPEM []byte
	KeyPEM  []byte
}

// GenerateCA creates a new self-signed CA suitable for signing leaf
// certs in the mesh control plane. commonName is informational.
func GenerateCA(commonName string) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("ca key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("ca self-sign: %w", err)
	}

	return &CA{
		CertPEM: encodePEM("CERTIFICATE", der),
		KeyPEM:  marshalECKey(key),
	}, nil
}

// GenerateServerCert produces a TLS server cert signed by ca, with the
// given DNS names and IP addresses in the SAN. Used by nodes for their
// gRPC listener.
func GenerateServerCert(ca *CA, commonName string, dnsNames []string, ips []net.IP) (*KeyPair, error) {
	caCert, caKey, err := parseCA(ca)
	if err != nil {
		return nil, err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("server key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    now.Add(-1 * time.Hour),
		NotAfter:     now.Add(leafValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("server sign: %w", err)
	}

	return &KeyPair{
		CertPEM: encodePEM("CERTIFICATE", der),
		KeyPEM:  marshalECKey(key),
	}, nil
}

// GenerateClientCSR produces a CSR + matching private key. Used by the
// master at the start of pairing — the CSR travels to the node, which
// signs it and returns the cert. Master keeps the private key locally.
func GenerateClientCSR(commonName string) (csrPEM []byte, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("client key: %w", err)
	}

	tmpl := &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: commonName},
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		return nil, nil, fmt.Errorf("csr create: %w", err)
	}

	return encodePEM("CERTIFICATE REQUEST", der), marshalECKey(key), nil
}

// SignClientCSR is the node-side counterpart to GenerateClientCSR. It
// validates the CSR signature and emits a leaf cert signed by ca with
// ExtKeyUsageClientAuth. The returned cert is what the master presents
// on every subsequent gRPC call as its mTLS identity.
func SignClientCSR(ca *CA, csrPEM []byte, validityCommonName string) ([]byte, error) {
	caCert, caKey, err := parseCA(ca)
	if err != nil {
		return nil, err
	}

	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("csr: not a PEM CERTIFICATE REQUEST block")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("csr parse: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("csr signature: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}

	cn := validityCommonName
	if cn == "" {
		cn = csr.Subject.CommonName
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    now.Add(-1 * time.Hour),
		NotAfter:     now.Add(leafValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, csr.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("client sign: %w", err)
	}

	return encodePEM("CERTIFICATE", der), nil
}

// FingerprintSHA256 returns the lowercase-hex SHA-256 of a PEM cert's
// DER bytes — used by the bootstrap-token flow to bind a token to a
// specific node cert. Returns "" if the PEM can't be parsed.
func FingerprintSHA256(certPEM []byte) string {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return ""
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:])
}

// --- internals ---

func parseCA(ca *CA) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cb, _ := pem.Decode(ca.CertPEM)
	if cb == nil || cb.Type != "CERTIFICATE" {
		return nil, nil, errors.New("ca: bad cert PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("ca cert parse: %w", err)
	}

	kb, _ := pem.Decode(ca.KeyPEM)
	if kb == nil {
		return nil, nil, errors.New("ca: bad key PEM")
	}
	// Accept either SEC1 EC ("EC PRIVATE KEY") or PKCS#8 — we always
	// emit PKCS#8 ourselves but be tolerant of older blobs.
	var key *ecdsa.PrivateKey
	switch kb.Type {
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(kb.Bytes)
	case "PRIVATE KEY":
		anyKey, err2 := x509.ParsePKCS8PrivateKey(kb.Bytes)
		if err2 != nil {
			return nil, nil, fmt.Errorf("ca key pkcs8: %w", err2)
		}
		var ok bool
		key, ok = anyKey.(*ecdsa.PrivateKey)
		if !ok {
			return nil, nil, errors.New("ca key: not ECDSA")
		}
	default:
		return nil, nil, fmt.Errorf("ca key: unsupported PEM type %q", kb.Type)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("ca key parse: %w", err)
	}
	return cert, key, nil
}

func marshalECKey(key *ecdsa.PrivateKey) []byte {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		// PKCS#8 marshal of a freshly generated EC key cannot fail in
		// practice; if it does, the runtime is broken — panic is fine.
		panic(fmt.Sprintf("pki: marshal pkcs8: %v", err))
	}
	return encodePEM("PRIVATE KEY", der)
}

func encodePEM(blockType string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
}

func randomSerial() (*big.Int, error) {
	max := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return nil, fmt.Errorf("serial: %w", err)
	}
	return n, nil
}
