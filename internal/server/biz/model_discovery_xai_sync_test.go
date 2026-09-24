package biz

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/httpclient"
	xaisubscription "github.com/looplj/axonhub/llm/transformer/xai/subscription"
)

// xaiGrant is the credential layout an xAI channel stores: a JSON grant in the
// api_key field rather than an OAuth object.
const xaiGrant = `{"client_id":"c","access_token":"live-token","refresh_token":"r","expires_at":"2099-01-01T00:00:00Z"}`

// The console's "sync models" action calls SyncChannelModels, so this exercises
// the whole path that action takes. xAI channels previously reported the compiled
// list only, which is why the console kept advertising a model the account no
// longer served and omitted the one it did.
func TestSyncChannelModels_XaiTakesUpstreamModelsAndKeepsCompiledOnes(t *testing.T) {
	t.Parallel()

	var seenPaths []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPaths = append(seenPaths, r.URL.Path)

		// The subscription proxy identifies callers by these headers.
		require.Equal(t, xaisubscription.CLITokenAuth, r.Header.Get(xaisubscription.CLITokenAuthHeader))
		require.Equal(t, xaisubscription.CLIClientIdentifier, r.Header.Get(xaisubscription.CLIClientIdentifierHeader))
		require.Equal(t, "Bearer live-token", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"grok-4.7"}]}`))
	}))
	defer server.Close()

	svc, client := setupTestChannelService(t)
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	svc.httpClient = httpclient.NewHttpClientWithClient(server.Client())

	ch, err := client.Channel.Create().
		SetType(channel.TypeXaiSubscription).
		SetName("grok").
		SetBaseURL(server.URL).
		SetCredentials(objects.ChannelCredentials{APIKey: xaiGrant}).
		SetSupportedModels([]string{"grok-4.6"}).
		SetDefaultTestModel("grok-4.6").
		Save(ctx)
	require.NoError(t, err)

	updated, err := svc.SyncChannelModels(ctx, ch.ID, nil)
	require.NoError(t, err)

	require.Equal(t, []string{"/models"}, seenPaths, "the provider must be asked, not the compiled list read")

	models := updated.SupportedModels
	require.NotEmpty(t, models)
	require.Equal(t, "grok-4.7", models[0], "the upstream model must lead the list")
	require.Contains(t, models, "grok-4.6", "a compiled-only model must survive the merge")
	require.Contains(t, models, "grok-4.5", "the rest of the compiled list must survive too")
}

// A failing provider must not empty the channel: an empty model list takes it out
// of service, so the sync falls back to the compiled list instead of failing.
func TestSyncChannelModels_XaiFailureFallsBackToTheCompiledList(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer server.Close()

	svc, client := setupTestChannelService(t)
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	svc.httpClient = httpclient.NewHttpClientWithClient(server.Client())

	ch, err := client.Channel.Create().
		SetType(channel.TypeXaiSubscription).
		SetName("grok").
		SetBaseURL(server.URL).
		SetCredentials(objects.ChannelCredentials{APIKey: xaiGrant}).
		SetSupportedModels([]string{"grok-4.6"}).
		SetDefaultTestModel("grok-4.6").
		Save(ctx)
	require.NoError(t, err)

	// The sync succeeds on the fallback rather than failing, and the channel keeps
	// a usable list instead of being emptied by a transient upstream error.
	_, err = svc.SyncChannelModels(ctx, ch.ID, nil)
	require.NoError(t, err)

	persisted, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.NotEmpty(t, persisted.SupportedModels, "a failed upstream fetch must not empty the channel")
	require.Contains(t, persisted.SupportedModels, "grok-4.6")
	require.Contains(t, persisted.SupportedModels, "grok-4.5", "the compiled fallback should be applied")
	// The model upstream served is not invented locally, so it must not appear.
	require.NotContains(t, persisted.SupportedModels, "grok-4.7")
}

// A channel whose stored token has lapsed must refresh before asking, or
// discovery fails for every account that has not been used recently.
func TestSyncChannelModels_XaiRefreshesAnExpiredToken(t *testing.T) {
	t.Parallel()

	var tokenCalls int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/token" {
			tokenCalls++

			require.NoError(t, r.ParseForm())
			require.Equal(t, "refresh_token", r.Form.Get("grant_type"))
			require.Equal(t, "r", r.Form.Get("refresh_token"))

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"fresh-token","expires_in":3600,"token_type":"Bearer"}`))

			return
		}

		require.Equal(t, "Bearer fresh-token", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"grok-4.7"}]}`))
	}))
	defer server.Close()

	svc, client := setupTestChannelService(t)
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	svc.httpClient = httpclient.NewHttpClientWithClient(server.Client())

	// Point the refresh at the test server so no real network call is made.
	originalTokenURL := xaisubscription.TokenURL
	xaisubscription.DefaultTokenURLs.TokenUrl = server.URL + "/oauth2/token"
	t.Cleanup(func() { xaisubscription.DefaultTokenURLs.TokenUrl = originalTokenURL })

	ch, err := client.Channel.Create().
		SetType(channel.TypeXaiSubscription).
		SetName("grok").
		SetBaseURL(server.URL).
		SetCredentials(objects.ChannelCredentials{
			APIKey: `{"client_id":"c","access_token":"stale","refresh_token":"r","expires_at":"2020-01-01T00:00:00Z"}`,
		}).
		SetSupportedModels([]string{"grok-4.6"}).
		SetDefaultTestModel("grok-4.6").
		Save(ctx)
	require.NoError(t, err)

	updated, err := svc.SyncChannelModels(ctx, ch.ID, nil)
	require.NoError(t, err)

	require.Equal(t, 1, tokenCalls, "the expired token must be refreshed once")
	require.Equal(t, "grok-4.7", updated.SupportedModels[0])
}
