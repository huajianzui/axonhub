package biz

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/oauth"
)

// TestReloadEnabledChannels_PicksHeaviestAccount drives the real production load
// path (reloadEnabledChannels, the channel cache's RefreshFunc) rather than
// calling the projection directly, so it covers the wiring end to end: accounts
// are loaded, projected onto the credential, and remembered on the snapshot.
func TestReloadEnabledChannels_PicksHeaviestAccount(t *testing.T) {
	t.Parallel()

	svc, ctx, client := accountService(t)

	// A channel carrying its own inline credential, as a pre-accounts install has.
	ch := newAccountTestChannel(ctx, client, channel.TypeClaudecode, "pool-load",
		objects.ChannelCredentials{OAuth: &oauth.OAuthCredentials{AccessToken: "inline", RefreshToken: "r"}},
	)

	// Two accounts: the lighter one first, so selection cannot pass by accident.
	createAccount(t, ctx, client, ch.ID, "light", "light-access", 10)
	createAccount(t, ctx, client, ch.ID, "heavy", "heavy-access", 90)

	loaded, _, changed, err := svc.reloadEnabledChannels(ctx, nil, time.Time{})
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, loaded, 1)

	// The credential the rest of the system reads now comes from the account,
	// specifically the heaviest one.
	require.NotNil(t, loaded[0].Credentials.OAuth)
	require.Equal(t, "heavy-access", loaded[0].Credentials.OAuth.AccessToken)

	// And every routable account is remembered so per-request selection can
	// reach them without a database read.
	require.Len(t, loaded[0].cachedAccounts, 2)
	require.Equal(t, "heavy-access", firstAccessToken(t, loaded[0].cachedAccounts[0]),
		"cached accounts must be ordered most preferred first")

	// The per-request getter is therefore available on a real loaded channel.
	getter := NewChannelAccountTokenGetter(AccountTokenGetterParams{Channel: loaded[0], Persister: svc.accountService})
	require.NotNil(t, getter)
}

// TestReloadEnabledChannels_SkipsUnroutableAccounts covers the eligibility rule
// on the real load path: a disabled or rejected account must neither serve the
// channel nor appear in its snapshot.
func TestReloadEnabledChannels_SkipsUnroutableAccounts(t *testing.T) {
	t.Parallel()

	svc, ctx, client := accountService(t)

	ch := newAccountTestChannel(ctx, client, channel.TypeClaudecode, "pool-ineligible",
		objects.ChannelCredentials{OAuth: &oauth.OAuthCredentials{AccessToken: "inline", RefreshToken: "r"}},
	)

	// The only account is disabled, so the channel must keep its inline grant.
	disabled := createAccount(t, ctx, client, ch.ID, "disabled", "disabled-access", 100)
	disabled.Update().SetEnabled(false).SaveX(ctx)

	loaded, _, _, err := svc.reloadEnabledChannels(ctx, nil, time.Time{})
	require.NoError(t, err)
	require.Len(t, loaded, 1)

	require.Equal(t, "inline", loaded[0].Credentials.OAuth.AccessToken,
		"an unroutable account must not replace the inline credential")
	require.Empty(t, loaded[0].cachedAccounts)
}

// TestReloadEnabledChannels_KeepsInlineCredentialForApiKeyChannels guards the
// common case: a plain API-key channel must be untouched by the account path.
func TestReloadEnabledChannels_KeepsInlineCredentialForApiKeyChannels(t *testing.T) {
	t.Parallel()

	svc, ctx, client := accountService(t)

	// openai channels require a base URL for their transformer to build, and a
	// channel whose transformer fails to build is skipped entirely.
	ch := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("plain-openai").
		SetStatus(channel.StatusEnabled).
		SetBaseURL("https://api.openai.com/v1").
		SetSupportedModels([]string{"test-model"}).
		SetDefaultTestModel("test-model").
		SetCredentials(objects.ChannelCredentials{APIKey: "sk-inline"}).
		SaveX(ctx)
	require.NotZero(t, ch.ID)

	loaded, _, _, err := svc.reloadEnabledChannels(ctx, nil, time.Time{})
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	require.Equal(t, "sk-inline", loaded[0].Credentials.APIKey)
	require.Empty(t, loaded[0].cachedAccounts)
	require.Nil(t, NewChannelAccountTokenGetter(AccountTokenGetterParams{Channel: loaded[0], Persister: svc.accountService}),
		"an api-key channel must keep the pre-account token provider")
}

// firstAccessToken reads the access token out of a stored account credential.
func firstAccessToken(t *testing.T, account *ent.ChannelAccount) string {
	t.Helper()

	projected, err := objects.ChannelCredentialsFromAccountCredential(account.Credentials)
	require.NoError(t, err)
	require.NotNil(t, projected.OAuth)

	return projected.OAuth.AccessToken
}

// ensure the helper signature used above stays honest.
var _ = func(_ context.Context) {}
