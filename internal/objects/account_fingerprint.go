// Package objects holds value types shared across the storage, service and
// transport layers.
package objects

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// credentialFingerprintPrefix namespaces the digest so a fingerprint computed
// for one purpose can never be confused with another.
const credentialFingerprintPrefix = "axonhub/channel-account/credential/v1"

// AccountCredentialFingerprint derives a stable digest of a stored credential.
//
// It exists to answer "is this the same grant?" without comparing secrets. The
// digest is computed from a canonical JSON encoding, which sorts map keys, so
// the same credential always yields the same fingerprint regardless of how the
// provider happened to order its fields.
//
// Two different grants never share a fingerprint, so this is what makes the
// per-channel uniqueness of a grant enforceable in the database. Unlike the
// account identity, it is always computable: a credential imported before the
// provider ever reported an identity still gets one.
func AccountCredentialFingerprint(credentials map[string]any) (string, error) {
	if len(credentials) == 0 {
		return "", fmt.Errorf("cannot fingerprint empty credentials")
	}

	// encoding/json sorts object keys, which makes this canonical.
	canonical, err := json.Marshal(credentials)
	if err != nil {
		return "", fmt.Errorf("canonicalize credentials: %w", err)
	}

	sum := sha256.Sum256(append([]byte(credentialFingerprintPrefix+"\x00"), canonical...))

	return hex.EncodeToString(sum[:]), nil
}
