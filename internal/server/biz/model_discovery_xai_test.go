package biz

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/httpclient"
)

// xaiGrantJSON is the shape an xAI channel stores in its api_key field.
const xaiGrantJSON = `{"client_id":"c","access_token":"live-token","refresh_token":"r","expires_at":"2099-01-01T00:00:00Z"}`

// A regression guard for the credential layout: xAI keeps its grant as JSON in
// the api_key field, not in the OAuth object. Reading only the OAuth object finds
// nothing, which silently disables discovery for every existing channel.
func TestFetchXaiUpstreamModels_ReadsTheGrantFromTheApiKeyField(t *testing.T) {
	t.Parallel()

	var seenAuth string

	httpClient := httpclient.NewHttpClientWithClient(&http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		seenAuth = r.Header.Get("Authorization")

		// The subscription proxy only answers CLI-identified callers.
		require.Equal(t, "xai-grok-cli", r.Header.Get("x-xai-token-auth"))
		require.Equal(t, "grok-shell", r.Header.Get("x-grok-client-identifier"))
		require.NotEmpty(t, r.Header.Get("x-grok-client-version"))
		require.Equal(t, "/v1/models", r.URL.Path)

		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"object":"list","data":[{"id":"grok-4.7"},{"id":"grok-4.6"}]}`)),
		}, nil
	})})

	fetcher := &ModelFetcher{httpClient: httpClient}

	models, err := fetcher.fetchXaiUpstreamModels(t.Context(), &ent.Channel{
		Type:        channel.TypeXaiSubscription,
		Name:        "grok",
		BaseURL:     "https://cli-chat-proxy.grok.com/v1",
		Credentials: objects.ChannelCredentials{APIKey: xaiGrantJSON},
	})

	require.NoError(t, err)
	require.Equal(t, "Bearer live-token", seenAuth)
	require.Len(t, models, 2)
	require.Equal(t, "grok-4.6", models[0].ID, "results are sorted")
	require.Equal(t, "grok-4.7", models[1].ID)
}

func TestFetchXaiUpstreamModels_ReportsUpstreamFailure(t *testing.T) {
	t.Parallel()

	httpClient := httpclient.NewHttpClientWithClient(&http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":"unauthorized"}`)),
		}, nil
	})})

	fetcher := &ModelFetcher{httpClient: httpClient}

	_, err := fetcher.fetchXaiUpstreamModels(t.Context(), &ent.Channel{
		Type:        channel.TypeXaiSubscription,
		Name:        "grok",
		BaseURL:     "https://cli-chat-proxy.grok.com/v1",
		Credentials: objects.ChannelCredentials{APIKey: xaiGrantJSON},
	})

	// The error is returned rather than swallowed so the caller falls back to the
	// compiled list instead of emptying the channel.
	require.Error(t, err)
	require.Contains(t, err.Error(), "fetch xAI models")
}

func TestXaiAccessToken_RejectsAChannelWithoutAGrant(t *testing.T) {
	t.Parallel()

	_, err := xaiAccessToken(t.Context(), &ent.Channel{
		Type:        channel.TypeXaiSubscription,
		Name:        "broken",
		Credentials: objects.ChannelCredentials{},
	}, httpclient.NewHttpClient())

	// The failure is reported rather than yielding an empty token, so discovery
	// falls back to the compiled list.
	require.Error(t, err)
	require.Contains(t, err.Error(), "broken")
}

// The discovery path must merge rather than replace: a model upstream omits today
// may simply be gated for the current plan.
func TestMergeDiscoveredModels_XaiKeepsCompiledOnlyModels(t *testing.T) {
	t.Parallel()

	discovered := []ModelIdentify{{ID: "grok-4.7"}}
	compiled := []ModelIdentify{{ID: "grok-4.6"}, {ID: "grok-3-mini"}}

	merged := mergeDiscoveredModels(discovered, compiled)

	require.Equal(t, []string{"grok-4.7", "grok-4.6", "grok-3-mini"}, discoveredModelIDs(merged))
}

func discoveredModelIDs(models []ModelIdentify) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}

	return ids
}

// Guard the credential fixture against drifting away from the real layout.
func TestXaiGrantFixtureMatchesStoredLayout(t *testing.T) {
	t.Parallel()

	var parsed struct {
		AccessToken string `json:"access_token"`
	}

	require.NoError(t, json.Unmarshal([]byte(xaiGrantJSON), &parsed))
	require.Equal(t, "live-token", parsed.AccessToken)
}
