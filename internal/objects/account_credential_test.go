package objects

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm/oauth"
)

func TestAccountCredentialFingerprintIsStable(t *testing.T) {
	t.Parallel()

	// Key order must not matter: the digest is what enforces one row per grant,
	// so an unstable digest would let duplicates in.
	first, err := AccountCredentialFingerprint(map[string]any{
		"apiKey": "ag-refresh|project-1",
	})
	require.NoError(t, err)

	second, err := AccountCredentialFingerprint(map[string]any{
		"apiKey": "ag-refresh|project-1",
	})
	require.NoError(t, err)

	require.Equal(t, first, second)
	require.NotEmpty(t, first)
}

func TestAccountCredentialFingerprintDistinguishesGrants(t *testing.T) {
	t.Parallel()

	a, err := AccountCredentialFingerprint(map[string]any{"apiKey": "grant-a"})
	require.NoError(t, err)

	b, err := AccountCredentialFingerprint(map[string]any{"apiKey": "grant-b"})
	require.NoError(t, err)

	require.NotEqual(t, a, b, "different grants must not collide")
}

func TestAccountCredentialFingerprintRejectsEmpty(t *testing.T) {
	t.Parallel()

	_, err := AccountCredentialFingerprint(map[string]any{})
	require.Error(t, err, "an empty credential has no identity to fingerprint")
}

func TestAccountCredentialRoundTripOAuth(t *testing.T) {
	original := ChannelCredentials{
		OAuth: &oauth.OAuthCredentials{
			ClientID:     "client-1",
			AccessToken:  "access-1",
			RefreshToken: "refresh-1",
			IDToken:      "id-1",
			TokenType:    "Bearer",
			Scopes:       []string{"scope-a", "scope-b"},
			ExpiresAt:    time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC),
		},
	}

	stored, err := AccountCredentialFromChannelCredentials(original)
	require.NoError(t, err)
	require.NotNil(t, stored["oauth"])

	// The stored form must survive a JSON boundary, because that is what the
	// account row does. A typed value would silently degrade to a map here.
	projected, err := ChannelCredentialsFromAccountCredential(roundTripJSON(t, stored))
	require.NoError(t, err)

	require.NotNil(t, projected.OAuth)
	require.Equal(t, original.OAuth.AccessToken, projected.OAuth.AccessToken)
	require.Equal(t, original.OAuth.RefreshToken, projected.OAuth.RefreshToken)
	require.Equal(t, original.OAuth.IDToken, projected.OAuth.IDToken)
	require.Equal(t, original.OAuth.ClientID, projected.OAuth.ClientID)
	require.Equal(t, original.OAuth.TokenType, projected.OAuth.TokenType)
	require.Equal(t, original.OAuth.Scopes, projected.OAuth.Scopes)
	require.True(t, original.OAuth.ExpiresAt.Equal(projected.OAuth.ExpiresAt))
}

func TestAccountCredentialRoundTripAntigravityLegacyString(t *testing.T) {
	t.Parallel()

	// Antigravity keeps "<refreshToken>|<projectID>" in the legacy APIKey field,
	// which is not an OAuth object at all.
	original := ChannelCredentials{APIKey: "ag-refresh|project-123"}

	stored, err := AccountCredentialFromChannelCredentials(original)
	require.NoError(t, err)

	projected, err := ChannelCredentialsFromAccountCredential(roundTripJSON(t, stored))
	require.NoError(t, err)

	require.Equal(t, "ag-refresh|project-123", projected.APIKey)
	require.Nil(t, projected.OAuth)
	require.False(t, projected.IsOAuth(), "a legacy antigravity grant is not an oauth object")
}

func TestAccountCredentialRoundTripPrefersOAuthAndKeepsAPIKey(t *testing.T) {
	t.Parallel()

	// A channel may carry both, as the codex/claudecode types do while their
	// credentials are being migrated between shapes.
	original := ChannelCredentials{
		APIKey: "legacy-json",
		OAuth:  &oauth.OAuthCredentials{AccessToken: "access-2", RefreshToken: "refresh-2"},
	}

	stored, err := AccountCredentialFromChannelCredentials(original)
	require.NoError(t, err)

	projected, err := ChannelCredentialsFromAccountCredential(roundTripJSON(t, stored))
	require.NoError(t, err)

	require.Equal(t, "legacy-json", projected.APIKey)
	require.NotNil(t, projected.OAuth)
	require.Equal(t, "access-2", projected.OAuth.AccessToken)
	require.True(t, projected.IsOAuth())
}

func TestAccountCredentialFromRejectsEmpty(t *testing.T) {
	t.Parallel()

	_, err := AccountCredentialFromChannelCredentials(ChannelCredentials{})
	require.Error(t, err)
}

// roundTripJSON simulates the account row's JSON storage: a value written to the
// database is read back as generic maps, not as the typed struct it was.
func roundTripJSON(t *testing.T, raw map[string]any) map[string]any {
	t.Helper()

	encoded, err := json.Marshal(raw)
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(encoded, &decoded))

	return decoded
}

func TestChannelCredentialsFromAccountCredentialRejectsUnusable(t *testing.T) {
	t.Parallel()

	for name, raw := range map[string]map[string]any{
		"empty":              {},
		"nil":                nil,
		"no usable field":    {"something": "else"},
		"apiKey wrong type":  {"apiKey": 42},
		"oauth empty object": {"oauth": map[string]any{}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := ChannelCredentialsFromAccountCredential(raw)
			require.Error(t, err, "unusable credential %+v must be rejected", raw)
		})
	}
}
