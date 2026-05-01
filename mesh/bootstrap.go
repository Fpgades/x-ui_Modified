// Package mesh holds the master/node control-plane glue. The lower-
// level helpers live in subpackages: pki for X.509, pb for generated
// gRPC stubs.
package mesh

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mhsanaei/3x-ui/v2/mesh/pki"
)

// BootstrapTokenTTL is how long a freshly minted pairing token is
// valid for. Short on purpose — the operator is expected to copy/paste
// the token from node to master within a few minutes.
const BootstrapTokenTTL = 10 * time.Minute

// Token format (one line, no padding):
//
//	XUIMESH1.<base64url(20 random bytes)>.<sha256-hex of node server cert>
//
// The version prefix lets us evolve the format without ambiguity.
// The node-cert fingerprint binds the token to a specific server
// identity, defeating MitM where an attacker who steals the token
// alone (without the node's TLS keypair) tries to pose as the node.
const tokenVersion = "XUIMESH1"

// Token is a parsed bootstrap token.
type Token struct {
	Raw                 string
	NodeCertFingerprint string // hex SHA-256 of node's gRPC server cert
}

// MintToken creates a fresh bootstrap token bound to the given node
// server cert (PEM). The caller stores the *raw* token (as well as an
// expiry time) in node_identities so that NodeIdentity.BootstrapToken
// can be compared on the incoming Pair call.
func MintToken(nodeServerCertPEM []byte) (string, error) {
	fp := pki.FingerprintSHA256(nodeServerCertPEM)
	if fp == "" {
		return "", errors.New("bootstrap: node cert PEM did not parse")
	}

	var raw [20]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("bootstrap: rand: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw[:])

	return fmt.Sprintf("%s.%s.%s", tokenVersion, encoded, fp), nil
}

// ParseToken splits a token into its parts and validates the format.
// It does NOT check expiry or compare against a stored value — those
// are the caller's responsibility (we don't have the storage layer
// here).
func ParseToken(s string) (*Token, error) {
	s = strings.TrimSpace(s)
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return nil, errors.New("bootstrap: malformed token (want 3 parts)")
	}
	if parts[0] != tokenVersion {
		return nil, fmt.Errorf("bootstrap: unsupported token version %q", parts[0])
	}
	if _, err := base64.RawURLEncoding.DecodeString(parts[1]); err != nil {
		return nil, fmt.Errorf("bootstrap: bad random payload: %w", err)
	}
	if len(parts[2]) != 64 { // SHA-256 hex
		return nil, errors.New("bootstrap: bad fingerprint length")
	}
	if _, err := hex.DecodeString(parts[2]); err != nil {
		return nil, fmt.Errorf("bootstrap: fingerprint not hex: %w", err)
	}
	return &Token{Raw: s, NodeCertFingerprint: parts[2]}, nil
}

// VerifyToken constant-time-compares two tokens. Use this on the node
// side when incoming Pair tokens are validated against the stored
// NodeIdentity.BootstrapToken — never compare with == directly.
func VerifyToken(stored, presented string) bool {
	if stored == "" {
		return false
	}
	a := []byte(stored)
	b := []byte(presented)
	if len(a) != len(b) {
		// constant-time compare requires equal lengths; pad and still
		// fail to avoid a length-revealing early return.
		dummy := make([]byte, len(a))
		subtle.ConstantTimeCompare(dummy, a)
		return false
	}
	return subtle.ConstantTimeCompare(a, b) == 1
}

// IsExpired returns true if expiry is non-zero and in the past.
// expiry==0 means "never expires" — used during tests; production
// code mints tokens with an expiry set.
func IsExpired(expiryUnixMs int64) bool {
	return expiryUnixMs > 0 && time.Now().UnixMilli() > expiryUnixMs
}

