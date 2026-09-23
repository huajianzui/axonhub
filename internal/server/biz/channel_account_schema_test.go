package biz

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/channelaccount"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
)

// newAccountTestClient builds an in-memory client with the full schema migrated,
// which is what proves the channel_accounts table is actually created.
func newAccountTestClient(t *testing.T) (*ent.Client, context.Context) {
	t.Helper()

	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	t.Cleanup(func() { _ = client.Close() })

	return client, authz.WithTestBypass(ent.NewContext(context.Background(), client))
}

// newAccountTestChannel creates a minimal enabled channel to hang accounts on.
func newAccountTestChannel(ctx context.Context, client *ent.Client, channelType channel.Type, name string, creds ...objects.ChannelCredentials) *ent.Channel {
	credential := objects.ChannelCredentials{}
	if len(creds) > 0 {
		credential = creds[0]
	}

	return client.Channel.Create().
		SetType(channelType).
		SetName(name).
		SetStatus(channel.StatusEnabled).
		SetSupportedModels([]string{"test-model"}).
		SetDefaultTestModel("test-model").
		SetCredentials(credential).
		SaveX(ctx)
}

func TestChannelAccountSchemaMigrates(t *testing.T) {
	t.Parallel()

	client, ctx := newAccountTestClient(t)
	ch := newAccountTestChannel(ctx, client, channel.TypeClaudecode, "claudecode-main")

	account, err := client.ChannelAccount.Create().
		SetChannelID(ch.ID).
		SetName("work").
		SetIdentity("acct-uuid/org-uuid").
		SetCredentialFingerprint("cred-1").
		SetCredentials(map[string]any{"access_token": "token-1", "refresh_token": "refresh-1"}).
		Save(ctx)
	require.NoError(t, err)

	// Defaults must be applied by the schema.
	require.True(t, account.Enabled, "a new account is enabled by default")
	require.Equal(t, 50, account.Weight, "a new account gets the default weight")
	require.Equal(t, channelaccount.AuthStateReady, account.AuthState, "a new account is ready by default")
	require.Empty(t, account.AuthErrorCode)
	require.Nil(t, account.ExpiresAt)
	require.Nil(t, account.LastRefreshAt)

	// The relation is addressable from both sides.
	fromChannel := ch.QueryAccounts().OnlyX(ctx)
	require.Equal(t, account.ID, fromChannel.ID)
	require.Equal(t, ch.ID, fromChannel.QueryChannel().OnlyX(ctx).ID)
}

func TestChannelAccountGrantIsUniquePerChannel(t *testing.T) {
	t.Parallel()

	client, ctx := newAccountTestClient(t)
	first := newAccountTestChannel(ctx, client, channel.TypeClaudecode, "channel-a")
	second := newAccountTestChannel(ctx, client, channel.TypeClaudecode, "channel-b")

	create := func(channelID int, credentialFingerprint string) error {
		_, err := client.ChannelAccount.Create().
			SetChannelID(channelID).
			SetIdentity("same-identity").
			SetCredentialFingerprint(credentialFingerprint).
			SetCredentials(map[string]any{"access_token": "t"}).
			Save(ctx)

		return err
	}

	require.NoError(t, create(first.ID, "same-grant"), "first account on a channel")

	// Re-importing the same grant on the same channel must collide, so the
	// account service can update in place instead of duplicating it. This holds
	// even when the provider never revealed an identity.
	require.Error(t, create(first.ID, "same-grant"), "duplicate grant on the same channel must fail")

	// A different channel may hold the same grant.
	require.NoError(t, create(second.ID, "same-grant"), "same grant on another channel is allowed")

	require.Len(t, first.QueryAccounts().AllX(ctx), 1)
	require.Len(t, second.QueryAccounts().AllX(ctx), 1)

	// Distinct grants on one channel are of course allowed.
	require.NoError(t, create(first.ID, "other-grant"))
	require.Len(t, first.QueryAccounts().AllX(ctx), 2)
}

func TestChannelAccountMultiplePerChannel(t *testing.T) {
	t.Parallel()

	client, ctx := newAccountTestClient(t)
	ch := newAccountTestChannel(ctx, client, channel.TypeAntigravity, "antigravity-pool")

	for _, identity := range []string{"acct-1", "acct-2", "acct-3"} {
		client.ChannelAccount.Create().
			SetChannelID(ch.ID).
			SetIdentity(identity).
			SetCredentialFingerprint("cred-" + identity).
			SetCredentials(map[string]any{"access_token": "token-" + identity}).
			SaveX(ctx)
	}

	// This is the whole point of the entity: one channel, several accounts.
	require.Len(t, ch.QueryAccounts().AllX(ctx), 3)

	enabled := client.ChannelAccount.Query().
		Where(
			channelaccount.ChannelIDEQ(ch.ID),
			channelaccount.EnabledEQ(true),
			channelaccount.AuthStateEQ(channelaccount.AuthStateReady),
		).
		AllX(ctx)
	require.Len(t, enabled, 3)
}

func TestChannelAccountDisabledAccountSurvives(t *testing.T) {
	t.Parallel()

	client, ctx := newAccountTestClient(t)
	ch := newAccountTestChannel(ctx, client, channel.TypeAntigravity, "antigravity-paused")

	account := client.ChannelAccount.Create().
		SetChannelID(ch.ID).
		SetIdentity("acct-paused").
		SetCredentialFingerprint("cred-paused").
		SetCredentials(map[string]any{"access_token": "t"}).
		SaveX(ctx)

	// Disabling and resetting the weight pause the account without dropping it,
	// which is what lets an operator resume it later.
	account.Update().
		SetEnabled(false).
		SetWeight(0).
		SetAuthState(channelaccount.AuthStateReauthorizationRequired).
		SetAuthErrorCode("refresh_rejected").
		SaveX(ctx)

	reloaded := client.ChannelAccount.GetX(ctx, account.ID)
	require.False(t, reloaded.Enabled)
	require.Zero(t, reloaded.Weight)
	require.Equal(t, channelaccount.AuthStateReauthorizationRequired, reloaded.AuthState)
	require.Equal(t, "refresh_rejected", reloaded.AuthErrorCode)

	// It is excluded from the routing set but still reachable for cleanup.
	require.Empty(t, client.ChannelAccount.Query().
		Where(
			channelaccount.ChannelIDEQ(ch.ID),
			channelaccount.EnabledEQ(true),
			channelaccount.AuthStateEQ(channelaccount.AuthStateReady),
		).
		AllX(ctx))

	require.Len(t, ch.QueryAccounts().AllX(ctx), 1)
}

func TestChannelAccountSoftDeleteKeepsTheRow(t *testing.T) {
	t.Parallel()

	client, ctx := newAccountTestClient(t)
	ch := newAccountTestChannel(ctx, client, channel.TypeCodex, "codex-pool")

	account := client.ChannelAccount.Create().
		SetChannelID(ch.ID).
		SetIdentity("acct-1").
		SetCredentialFingerprint("cred-1").
		SetCredentials(map[string]any{"access_token": "t"}).
		SaveX(ctx)

	require.NoError(t, client.ChannelAccount.DeleteOneID(account.ID).Exec(ctx))

	// The default query hides it; an operator can still recover the history.
	require.Empty(t, client.ChannelAccount.Query().AllX(ctx))
	require.Len(t, ch.QueryAccounts().AllX(ctx), 0)
}
