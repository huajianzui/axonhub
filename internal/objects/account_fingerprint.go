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

// AccountCredentialFingerprint derives a stable digest identifying a grant.
//
// It exists to answer "is this the same authorization?" without comparing
// secrets, and it is what makes a grant unique per channel, so re-importing the
// same authorization updates the existing account instead of adding a duplicate.
//
// The digest is deliberately computed from the grant's stable identity -- the
// refresh token, or for a provider that has none, the legacy credential string
// -- rather than from the whole serialized credential. Two reasons:
//
//   - Parsing a credential injects an expiry when the provider supplied none, so
//     the serialized form differs between two parses of the same authorization.
//     Fingerprinting it would make a retried import look like a new account.
//   - An access token is replaced on every refresh, while the refresh token
//     identifies the authorization across refreshes. Keying on the whole
//     credential would make a refreshed account look like a different one.
func AccountCredentialFingerprint(credentials map[string]any) (string, error) {
	source, err := credentialStableIdentity(credentials)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256([]byte(credentialFingerprintPrefix + "\x00" + source))

	return hex.EncodeToString(sum[:]), nil
}

// credentialStableIdentity extracts the part of a credential that identifies the
// authorization rather than the current token.
func credentialStableIdentity(credentials map[string]any) (string, error) {
	if len(credentials) == 0 {
		return "", fmt.Errorf("cannot fingerprint empty credentials")
	}

	if oauthRaw, ok := credentials["oauth"]; ok && oauthRaw != nil {
		// The value round-trips through JSON, so it arrives as a generic map
		// rather than the typed struct.
		blob, err := json.Marshal(oauthRaw)
		if err == nil {
			var parsed struct {
				AccessToken  string `json:"access_token"`
				RefreshToken string `json:"refresh_token"`
			}

			if err := json.Unmarshal(blob, &parsed); err == nil {
				if parsed.RefreshToken != "" {
					return "refresh:" + parsed.RefreshToken, nil
				}

				if parsed.AccessToken != "" {
					return "access:" + parsed.AccessToken, nil
				}
			}
		}
	}

	// A credential held in the legacy field is already a stable identity, for
	// example antigravity's "<refreshToken>|<projectID>".
	if apiKey, ok := credentials["apiKey"].(string); ok && apiKey != "" {
		return "legacy:" + apiKey, nil
	}

	return "", fmt.Errorf("credentials hold no stable identity")
}
