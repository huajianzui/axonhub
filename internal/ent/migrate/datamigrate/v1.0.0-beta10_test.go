package datamigrate_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/migrate/datamigrate"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/oauth"
)

func newBeta10Client(t *testing.T, name string) (*ent.Client, context.Context) {
	t.Helper()

	client := enttest.NewEntClient(t, "sqlite3", "file:beta10-"+name+"?mode=memory&_fk=1")
	t.Cleanup(func() { _ = client.Close() })

	return client, authz.WithTestBypass(context.Background())
}

func newBeta10Channel(ctx context.Context, client *ent.Client, channelType channel.Type, name string, creds objects.ChannelCredentials) *ent.Channel {
	return client.Channel.Create().
		SetType(channelType).
		SetName(name).
		SetStatus(channel.StatusEnabled).
		SetSupportedModels([]string{"test-model"}).
		SetDefaultTestModel("test-model").
		SetCredentials(creds).
		SaveX(ctx)
}

func TestV1_0_0_Beta10_BackfillsOAuthChannel(t *testing.T) {
	client, ctx := newBeta10Client(t, "oauth")

	creds := &oauth.OAuthCredentials{
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		TokenType:    "Bearer",
	}
	ch := newBeta10Channel(ctx, client, channel.TypeClaudecode, "claude-main", objects.ChannelCredentials{OAuth: creds})

	require.NoError(t, datamigrate.NewV1_0_0_Beta10().Migrate(ctx, client))

	accounts := ch.QueryAccounts().AllX(ctx)
	require.Len(t, accounts, 1)

	account := accounts[0]
	require.True(t, account.Enabled)
	require.Equal(t, 50, account.Weight)
	require.NotEmpty(t, account.CredentialFingerprint)

	// The whole credential is preserved through the JSON boundary, so the
	// projection back onto the channel stays lossless.
	projected, err := objects.ChannelCredentialsFromAccountCredential(account.Credentials)
	require.NoError(t, err)
	require.NotNil(t, projected.OAuth)
	require.Equal(t, "access-1", projected.OAuth.AccessToken)
	require.Equal(t, "refresh-1", projected.OAuth.RefreshToken)

	// The channel's own credential is deliberately untouched: it is still the
	// authority until account reads are wired up, and this is what makes a
	// rollback safe.
	reloaded := client.Channel.GetX(ctx, ch.ID)
	require.True(t, reloaded.Credentials.IsOAuth())
	require.Equal(t, "access-1", reloaded.Credentials.OAuth.AccessToken)
}

func TestV1_0_0_Beta10_BackfillsAntigravityLegacyString(t *testing.T) {
	client, ctx := newBeta10Client(t, "antigravity")

	// Antigravity predates the OAuth object and stores "<refreshToken>|<projectID>"
	// in the legacy APIKey field, so IsOAuth is false for it.
	ch := newBeta10Channel(ctx, client, channel.TypeAntigravity, "ag-main", objects.ChannelCredentials{
		APIKey: "ag-refresh|project-123",
	})
	require.False(t, ch.Credentials.IsOAuth(), "antigravity credentials must not satisfy IsOAuth")

	require.NoError(t, datamigrate.NewV1_0_0_Beta10().Migrate(ctx, client))

	accounts := ch.QueryAccounts().AllX(ctx)
	require.Len(t, accounts, 1, "antigravity must not be skipped")
	require.Equal(t, "ag-refresh|project-123", accounts[0].Credentials["apiKey"])
}

func TestV1_0_0_Beta10_SkipsChannelsWithoutAGrant(t *testing.T) {
	client, ctx := newBeta10Client(t, "skip")

	// An API-key channel.
	apiKeyChannel := newBeta10Channel(ctx, client, channel.TypeOpenai, "openai-main", objects.ChannelCredentials{APIKey: "sk-abc"})
	// A subscription channel type that was never authorized.
	emptyOAuth := newBeta10Channel(ctx, client, channel.TypeClaudecode, "claude-empty", objects.ChannelCredentials{})

	require.NoError(t, datamigrate.NewV1_0_0_Beta10().Migrate(ctx, client))

	require.Empty(t, apiKeyChannel.QueryAccounts().AllX(ctx), "an api key channel must not get an account")
	require.Empty(t, emptyOAuth.QueryAccounts().AllX(ctx), "an unauthorized channel must not get an account")
	require.Empty(t, client.ChannelAccount.Query().AllX(ctx))
}

func TestV1_0_0_Beta10_IsIdempotent(t *testing.T) {
	client, ctx := newBeta10Client(t, "idempotent")

	ch := newBeta10Channel(ctx, client, channel.TypeCodex, "codex-main", objects.ChannelCredentials{
		OAuth: &oauth.OAuthCredentials{AccessToken: "a", RefreshToken: "r"},
	})

	require.NoError(t, datamigrate.NewV1_0_0_Beta10().Migrate(ctx, client))
	require.Len(t, ch.QueryAccounts().AllX(ctx), 1)

	// A second pass must not duplicate the grant.
	require.NoError(t, datamigrate.NewV1_0_0_Beta10().Migrate(ctx, client))
	require.Len(t, ch.QueryAccounts().AllX(ctx), 1)
}

func TestV1_0_0_Beta10_TouchesMigratedChannel(t *testing.T) {
	client, ctx := newBeta10Client(t, "touch")

	ch := newBeta10Channel(ctx, client, channel.TypeClaudecode, "claude-touch", objects.ChannelCredentials{
		OAuth: &oauth.OAuthCredentials{AccessToken: "a", RefreshToken: "r"},
	})

	// Pin updated_at to a clearly older value so the assertion cannot flake on
	// timer resolution when the migration lands in the same tick.
	past := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	_, err := client.Channel.UpdateOneID(ch.ID).SetUpdatedAt(past).Save(ctx)
	require.NoError(t, err)

	require.NoError(t, datamigrate.NewV1_0_0_Beta10().Migrate(ctx, client))

	// The channel cache only reloads when a channel row moves, and the account
	// lives in its own table, so the migration must bump the channel.
	after := client.Channel.GetX(ctx, ch.ID).UpdatedAt
	require.True(t, after.After(past), "migrated channel updated_at must advance past %s, got %s", past, after)
}

func TestV1_0_0_Beta10_LeavesUnmigratedChannelUntouched(t *testing.T) {
	client, ctx := newBeta10Client(t, "untouched")

	ch := newBeta10Channel(ctx, client, channel.TypeOpenai, "openai-untouched", objects.ChannelCredentials{APIKey: "sk-abc"})
	before := ch.UpdatedAt

	require.NoError(t, datamigrate.NewV1_0_0_Beta10().Migrate(ctx, client))

	after := client.Channel.GetX(ctx, ch.ID).UpdatedAt
	require.Equal(t, before, after, "a channel with no grant must not be touched")
}
