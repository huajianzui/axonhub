package biz

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/channelaccount"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/oauth"
)

// accountService builds a channel service with its schema migrated and the
// account service wired, enough to exercise account loading and projection.
func accountService(t *testing.T) (*ChannelService, context.Context, *ent.Client) {
	t.Helper()

	client, ctx := newAccountTestClient(t)
	svc := newTestChannelService(client)
	svc.accountService = &ChannelAccountService{
		AbstractService: &AbstractService{db: client},
	}

	return svc, ctx, client
}

// accountServiceWithoutAccounts builds the same service with the account service
// deliberately left unwired, which is the shape unit tests that construct a
// ChannelService directly rely on.
func accountServiceWithoutAccounts(t *testing.T) (*ChannelService, context.Context, *ent.Client) {
	t.Helper()

	client, ctx := newAccountTestClient(t)

	return newTestChannelService(client), ctx, client
}

func TestProjectAccountsReplacesInlineCredential(t *testing.T) {
	t.Parallel()

	svc, ctx, client := accountService(t)

	// The channel carries one grant inline, as it did before accounts existed.
	ch := client.Channel.Create().
		SetType(channel.TypeClaudecode).
		SetName("claude-project").
		SetStatus(channel.StatusEnabled).
		SetSupportedModels([]string{"test-model"}).
		SetDefaultTestModel("test-model").
		SetCredentials(objects.ChannelCredentials{
			OAuth: &oauth.OAuthCredentials{AccessToken: "inline-access", RefreshToken: "inline-refresh"},
		}).
		SaveX(ctx)

	// The account row holds a newer grant for the same channel.
	stored, err := objects.AccountCredentialFromChannelCredentials(objects.ChannelCredentials{
		OAuth: &oauth.OAuthCredentials{AccessToken: "account-access", RefreshToken: "account-refresh"},
	})
	require.NoError(t, err)

	client.ChannelAccount.Create().
		SetChannelID(ch.ID).
		SetIdentity("acct-1").
		SetCredentials(stored).
		SetCredentialFingerprint("cred-1").
		SaveX(ctx)

	entities := client.Channel.Query().AllX(ctx)
	svc.projectAccountsOntoChannels(ctx, entities)

	require.Len(t, entities, 1)
	projected := entities[0].Credentials
	require.True(t, projected.IsOAuth())
	require.Equal(t, "account-access", projected.OAuth.AccessToken)
	require.Equal(t, "account-refresh", projected.OAuth.RefreshToken)
}

func TestProjectAccountsIsNoOpWithoutAccounts(t *testing.T) {
	t.Parallel()

	svc, ctx, client := accountService(t)

	ch := client.Channel.Create().
		SetType(channel.TypeClaudecode).
		SetName("claude-inline-only").
		SetStatus(channel.StatusEnabled).
		SetSupportedModels([]string{"test-model"}).
		SetDefaultTestModel("test-model").
		SetCredentials(objects.ChannelCredentials{
			OAuth: &oauth.OAuthCredentials{AccessToken: "inline-access", RefreshToken: "inline-refresh"},
		}).
		SaveX(ctx)
	require.NotZero(t, ch.ID)

	entities := client.Channel.Query().AllX(ctx)
	svc.projectAccountsOntoChannels(ctx, entities)

	// A channel with no account keeps its inline credential: this is what makes
	// the projection a no-op for every existing installation.
	require.Len(t, entities, 1)
	require.True(t, entities[0].Credentials.IsOAuth())
	require.Equal(t, "inline-access", entities[0].Credentials.OAuth.AccessToken)
}

func TestProjectAccountsIgnoresIneligibleAccounts(t *testing.T) {
	t.Parallel()

	svc, ctx, client := accountService(t)

	ch := client.Channel.Create().
		SetType(channel.TypeClaudecode).
		SetName("claude-ineligible").
		SetStatus(channel.StatusEnabled).
		SetSupportedModels([]string{"test-model"}).
		SetDefaultTestModel("test-model").
		SetCredentials(objects.ChannelCredentials{
			OAuth: &oauth.OAuthCredentials{AccessToken: "inline-access", RefreshToken: "inline-refresh"},
		}).
		SaveX(ctx)

	stored, err := objects.AccountCredentialFromChannelCredentials(objects.ChannelCredentials{
		OAuth: &oauth.OAuthCredentials{AccessToken: "paused-access", RefreshToken: "paused-refresh"},
	})
	require.NoError(t, err)

	// Disabled.
	client.ChannelAccount.Create().
		SetChannelID(ch.ID).
		SetIdentity("disabled").
		SetCredentials(stored).
		SetCredentialFingerprint("cred-disabled").
		SetEnabled(false).
		SaveX(ctx)

	// Needs re-authorization.
	client.ChannelAccount.Create().
		SetChannelID(ch.ID).
		SetIdentity("reauth").
		SetCredentials(stored).
		SetCredentialFingerprint("cred-reauth").
		SetAuthState(channelaccount.AuthStateReauthorizationRequired).
		SaveX(ctx)

	entities := client.Channel.Query().AllX(ctx)
	svc.projectAccountsOntoChannels(ctx, entities)

	require.Equal(t, "inline-access", entities[0].Credentials.OAuth.AccessToken,
		"only enabled, ready accounts may replace the inline credential")
}

func TestProjectAccountsPrefersHeaviestAccount(t *testing.T) {
	t.Parallel()

	svc, ctx, client := accountService(t)

	ch := client.Channel.Create().
		SetType(channel.TypeClaudecode).
		SetName("claude-weighted").
		SetStatus(channel.StatusEnabled).
		SetSupportedModels([]string{"test-model"}).
		SetDefaultTestModel("test-model").
		SetCredentials(objects.ChannelCredentials{
			OAuth: &oauth.OAuthCredentials{AccessToken: "inline", RefreshToken: "inline"},
		}).
		SaveX(ctx)

	createAccount := func(identity, accessToken string, weight int) {
		stored, err := objects.AccountCredentialFromChannelCredentials(objects.ChannelCredentials{
			OAuth: &oauth.OAuthCredentials{AccessToken: accessToken, RefreshToken: "r"},
		})
		require.NoError(t, err)

		client.ChannelAccount.Create().
			SetChannelID(ch.ID).
			SetIdentity(identity).
			SetCredentials(stored).
			SetCredentialFingerprint("cred-" + identity).
			SetWeight(weight).
			SaveX(ctx)
	}

	createAccount("low", "low-access", 10)
	createAccount("high", "high-access", 90)
	createAccount("mid", "mid-access", 50)

	entities := client.Channel.Query().AllX(ctx)
	svc.projectAccountsOntoChannels(ctx, entities)

	require.Equal(t, "high-access", entities[0].Credentials.OAuth.AccessToken)
}

func TestProjectAccountsKeepsInlineCredentialOnUnusableAccount(t *testing.T) {
	t.Parallel()

	svc, ctx, client := accountService(t)

	ch := client.Channel.Create().
		SetType(channel.TypeClaudecode).
		SetName("claude-corrupt").
		SetStatus(channel.StatusEnabled).
		SetSupportedModels([]string{"test-model"}).
		SetDefaultTestModel("test-model").
		SetCredentials(objects.ChannelCredentials{
			OAuth: &oauth.OAuthCredentials{AccessToken: "inline-access", RefreshToken: "inline-refresh"},
		}).
		SaveX(ctx)

	// A row whose credential carries no usable grant.
	client.ChannelAccount.Create().
		SetChannelID(ch.ID).
		SetIdentity("corrupt").
		SetCredentials(map[string]any{"something": "else"}).
		SetCredentialFingerprint("cred-corrupt").
		SaveX(ctx)

	entities := client.Channel.Query().AllX(ctx)
	svc.projectAccountsOntoChannels(ctx, entities)

	// Losing a working credential because an account row is malformed would be a
	// worse failure than ignoring the row.
	require.Equal(t, "inline-access", entities[0].Credentials.OAuth.AccessToken)
}

func TestProjectAccountsLeavesOtherChannelsAlone(t *testing.T) {
	t.Parallel()

	svc, ctx, client := accountService(t)

	withAccount := client.Channel.Create().
		SetType(channel.TypeClaudecode).
		SetName("claude-with-account").
		SetStatus(channel.StatusEnabled).
		SetSupportedModels([]string{"test-model"}).
		SetDefaultTestModel("test-model").
		SetCredentials(objects.ChannelCredentials{
			OAuth: &oauth.OAuthCredentials{AccessToken: "inline-a", RefreshToken: "r"},
		}).
		SaveX(ctx)

	withoutAccount := client.Channel.Create().
		SetType(channel.TypeClaudecode).
		SetName("claude-without-account").
		SetStatus(channel.StatusEnabled).
		SetSupportedModels([]string{"test-model"}).
		SetDefaultTestModel("test-model").
		SetCredentials(objects.ChannelCredentials{
			OAuth: &oauth.OAuthCredentials{AccessToken: "inline-b", RefreshToken: "r"},
		}).
		SaveX(ctx)

	stored, err := objects.AccountCredentialFromChannelCredentials(objects.ChannelCredentials{
		OAuth: &oauth.OAuthCredentials{AccessToken: "account-access", RefreshToken: "r"},
	})
	require.NoError(t, err)

	client.ChannelAccount.Create().
		SetChannelID(withAccount.ID).
		SetIdentity("acct").
		SetCredentials(stored).
		SetCredentialFingerprint("cred").
		SaveX(ctx)

	entities := client.Channel.Query().AllX(ctx)
	svc.projectAccountsOntoChannels(ctx, entities)

	byID := make(map[int]*ent.Channel, len(entities))
	for _, c := range entities {
		byID[c.ID] = c
	}

	require.Equal(t, "account-access", byID[withAccount.ID].Credentials.OAuth.AccessToken)
	require.Equal(t, "inline-b", byID[withoutAccount.ID].Credentials.OAuth.AccessToken)
}

func TestProjectAccountsHandlesEmptyInput(t *testing.T) {
	t.Parallel()

	svc, ctx, _ := accountService(t)

	// Must not query or panic on an empty reload.
	svc.projectAccountsOntoChannels(ctx, nil)
	svc.projectAccountsOntoChannels(ctx, []*ent.Channel{})
}

// TestProjectAccountsSkippedWhenServiceUnwired guards every unit test that
// builds a ChannelService directly: without the account service the projection
// must be a silent no-op, not a panic.
func TestProjectAccountsSkippedWhenServiceUnwired(t *testing.T) {
	t.Parallel()

	svc, ctx, client := accountServiceWithoutAccounts(t)

	ch := client.Channel.Create().
		SetType(channel.TypeClaudecode).
		SetName("claude-unwired").
		SetStatus(channel.StatusEnabled).
		SetSupportedModels([]string{"test-model"}).
		SetDefaultTestModel("test-model").
		SetCredentials(objects.ChannelCredentials{
			OAuth: &oauth.OAuthCredentials{AccessToken: "inline-access", RefreshToken: "inline-refresh"},
		}).
		SaveX(ctx)

	stored, err := objects.AccountCredentialFromChannelCredentials(objects.ChannelCredentials{
		OAuth: &oauth.OAuthCredentials{AccessToken: "account-access", RefreshToken: "account-refresh"},
	})
	require.NoError(t, err)

	client.ChannelAccount.Create().
		SetChannelID(ch.ID).
		SetIdentity("acct").
		SetCredentials(stored).
		SetCredentialFingerprint("cred").
		SaveX(ctx)

	entities := client.Channel.Query().AllX(ctx)
	svc.projectAccountsOntoChannels(ctx, entities)

	require.Equal(t, "inline-access", entities[0].Credentials.OAuth.AccessToken,
		"an unwired account service must leave inline credentials alone")
}

// TestProjectAccountsProjectsThroughSingleChannelLoad covers the per-channel
// read path (GetChannel), which shares the same projection.
func TestProjectAccountsProjectsThroughSingleChannelLoad(t *testing.T) {
	t.Parallel()

	svc, ctx, client := accountService(t)

	ch := client.Channel.Create().
		SetType(channel.TypeAntigravity).
		SetName("ag-single").
		SetStatus(channel.StatusEnabled).
		SetSupportedModels([]string{"test-model"}).
		SetDefaultTestModel("test-model").
		SetCredentials(objects.ChannelCredentials{APIKey: "stale|project-1"}).
		SaveX(ctx)

	client.ChannelAccount.Create().
		SetChannelID(ch.ID).
		SetIdentity("acct-ag").
		SetCredentials(map[string]any{"apiKey": "fresh|project-1"}).
		SetCredentialFingerprint("cred-ag").
		SaveX(ctx)

	built, err := svc.GetChannel(ctx, ch.ID)
	require.NoError(t, err)
	require.NotNil(t, built)
	require.Equal(t, "fresh|project-1", built.Credentials.APIKey,
		"the single-channel read path must project too")
}

// TestProjectAccountsIgnoresLowerWeightedAccount is the negative half of the
// weight ordering: the heavier account wins and the lighter one is not used.
func TestProjectAccountsIgnoresLowerWeightedAccount(t *testing.T) {
	t.Parallel()

	svc, ctx, client := accountService(t)

	ch := client.Channel.Create().
		SetType(channel.TypeClaudecode).
		SetName("claude-order").
		SetStatus(channel.StatusEnabled).
		SetSupportedModels([]string{"test-model"}).
		SetDefaultTestModel("test-model").
		SetCredentials(objects.ChannelCredentials{
			OAuth: &oauth.OAuthCredentials{AccessToken: "inline", RefreshToken: "r"},
		}).
		SaveX(ctx)

	createAccount := func(identity, accessToken string, weight int) {
		stored, err := objects.AccountCredentialFromChannelCredentials(objects.ChannelCredentials{
			OAuth: &oauth.OAuthCredentials{AccessToken: accessToken, RefreshToken: "r"},
		})
		require.NoError(t, err)

		client.ChannelAccount.Create().
			SetChannelID(ch.ID).
			SetIdentity(identity).
			SetCredentials(stored).
			SetCredentialFingerprint("cred-" + identity).
			SetWeight(weight).
			SaveX(ctx)
	}

	createAccount("light", "light-access", 1)
	createAccount("heavy", "heavy-access", 100)

	entities := client.Channel.Query().AllX(ctx)
	svc.projectAccountsOntoChannels(ctx, entities)

	require.Equal(t, "heavy-access", entities[0].Credentials.OAuth.AccessToken)
	require.NotEqual(t, "light-access", entities[0].Credentials.OAuth.AccessToken)
}
