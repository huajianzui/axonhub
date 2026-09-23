package objects

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/looplj/axonhub/llm/oauth"
)

// This file holds the round trip between a channel's credential object and the
// generic form an account row stores.
//
// A ChannelAccount persists its credential as JSON, so the typed OAuth value is
// not preserved across the database: it reads back as a generic map. These
// helpers convert in both directions so the account service can project an
// account back onto the ChannelCredentials shape the rest of the code already
// consumes, without every caller having to know about the JSON boundary.

// AccountCredentialFromChannelCredentials converts a channel's credential into
// the form stored on an account row.
//
// The OAuth value is serialized rather than kept as a typed value on purpose:
// the row is JSON, and passing the typed struct through would only be preserved
// until the first read.
func AccountCredentialFromChannelCredentials(creds ChannelCredentials) (map[string]any, error) {
	raw := map[string]any{}

	if creds.APIKey != "" {
		raw["apiKey"] = creds.APIKey
	}

	if creds.OAuth != nil {
		encoded, err := creds.OAuth.ToJSON()
		if err != nil {
			return nil, fmt.Errorf("encode oauth credentials: %w", err)
		}

		var decoded map[string]any
		if err := json.Unmarshal([]byte(encoded), &decoded); err != nil {
			return nil, fmt.Errorf("decode oauth credentials: %w", err)
		}

		raw["oauth"] = decoded
	}

	if len(raw) == 0 {
		return nil, errors.New("channel credentials hold no grant")
	}

	return raw, nil
}

// AccountCredentialExpiry reports when the stored credential expires, if it
// carries an expiry at all.
//
// A grant stored in the legacy field has no expiry to read, which is why the
// second return value exists rather than a zero time being meaningful.
func AccountCredentialExpiry(raw map[string]any) (time.Time, bool) {
	projected, err := ChannelCredentialsFromAccountCredential(raw)
	if err != nil || projected.OAuth == nil || projected.OAuth.ExpiresAt.IsZero() {
		return time.Time{}, false
	}

	return projected.OAuth.ExpiresAt, true
}

// ChannelCredentialsFromAccountCredential projects an account's stored
// credential back onto the channel credential shape.
// This is the inverse of AccountCredentialFromChannelCredentials and is what
// lets the rest of the code keep reading ChannelCredentials unchanged while the
// value originates from an account row.
func ChannelCredentialsFromAccountCredential(raw map[string]any) (ChannelCredentials, error) {
	if len(raw) == 0 {
		return ChannelCredentials{}, errors.New("account credential is empty")
	}

	out := ChannelCredentials{}

	if apiKey, ok := raw["apiKey"].(string); ok {
		out.APIKey = apiKey
	}

	if encoded, ok := raw["oauth"]; ok && encoded != nil {
		blob, err := json.Marshal(encoded)
		if err != nil {
			return ChannelCredentials{}, fmt.Errorf("encode oauth credential: %w", err)
		}

		parsed, err := oauth.ParseCredentialsJSON(string(blob))
		if err != nil {
			return ChannelCredentials{}, fmt.Errorf("parse oauth credential: %w", err)
		}

		out.OAuth = parsed
	}

	if out.APIKey == "" && out.OAuth == nil {
		return ChannelCredentials{}, errors.New("account credential holds no usable grant")
	}

	return out, nil
}
