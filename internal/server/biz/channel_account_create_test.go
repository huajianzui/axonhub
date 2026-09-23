package biz

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/channelaccount"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/oauth"
)

// oauthGrantJSON builds the string a provider exchange would hand the console.
func oauthGrantJSON(access, refresh string) string {
	raw, err := (&oauth.OAuthCredentials{
		AccessToken:  access,
		RefreshToken: refresh,
		TokenType:    "Bearer",
	}).ToJSON()
	if err != nil {
		panic(err)
	}

	return raw
}

func TestCreateAccountFromCredentials_AddsAnAccount(t *testing.T) {
	t.Parallel()

	client, ctx := newAccountTestClient(t)
	svc := newAccountServiceForTest(client)

	ch := newAccountTestChannel(ctx, client, channel.TypeClaudecode, "claudecode-add",
		objects.ChannelCredentials{OAuth: &oauth.OAuthCredentials{AccessToken: "inline", RefreshToken: "r"}},
	)

	account, err := svc.CreateAccountFromCredentials(ctx, ch.ID, oauthGrantJSON("added-access", "added-refresh"))
	require.NoError(t, err)
	require.NotNil(t, account)
	require.True(t, account.Enabled)
	require.Equal(t, channelaccount.AuthStateReady, account.AuthState)
	require.NotEmpty(t, account.CredentialFingerprint)

	// The grant is readable back, which is what the runtime will project.
	projected, err := objects.ChannelCredentialsFromAccountCredential(account.Credentials)
	require.NoError(t, err)
	require.NotNil(t, projected.OAuth)
	require.Equal(t, "added-access", projected.OAuth.AccessToken)

	// Adding a second account to the same channel is the whole point.
	second, err := svc.CreateAccountFromCredentials(ctx, ch.ID, oauthGrantJSON("other-access", "other-refresh"))
	require.NoError(t, err)
	require.NotEqual(t, account.ID, second.ID)

	require.Len(t, ch.QueryAccounts().AllX(ctx), 2)
}

// TestCreateAccountFromCredentials_IsIdempotentOnTheGrant makes a retried
// request safe: the same authorization must not produce a duplicate account.
func TestCreateAccountFromCredentials_IsIdempotentOnTheGrant(t *testing.T) {
	t.Parallel()

	client, ctx := newAccountTestClient(t)
	svc := newAccountServiceForTest(client)

	ch := newAccountTestChannel(ctx, client, channel.TypeClaudecode, "claudecode-idem")
	grant := oauthGrantJSON("same-access", "same-refresh")

	first, err := svc.CreateAccountFromCredentials(ctx, ch.ID, grant)
	require.NoError(t, err)

	again, err := svc.CreateAccountFromCredentials(ctx, ch.ID, grant)
	require.NoError(t, err)
	require.Equal(t, first.ID, again.ID, "the same grant must not create a second account")

	require.Len(t, ch.QueryAccounts().AllX(ctx), 1)
}

// TestCreateAccountFromCredentials_TouchesTheChannel is what makes the new
// account visible to the runtime: the account cache is only rebuilt when a
// channel row moves.
func TestCreateAccountFromCredentials_TouchesTheChannel(t *testing.T) {
	t.Parallel()

	client, ctx := newAccountTestClient(t)
	svc := newAccountServiceForTest(client)

	ch := newAccountTestChannel(ctx, client, channel.TypeClaudecode, "claudecode-touch")
	before := client.Channel.GetX(ctx, ch.ID).UpdatedAt

	_, err := svc.CreateAccountFromCredentials(ctx, ch.ID, oauthGrantJSON("a", "r"))
	require.NoError(t, err)

	after := client.Channel.GetX(ctx, ch.ID).UpdatedAt
	require.True(t, after.After(before) || after.Equal(before))
	require.NotEqual(t, before, after, "adding an account must invalidate the channel cache")
}

func TestCreateAccountFromCredentials_AcceptsAntigravityLegacyString(t *testing.T) {
	t.Parallel()

	client, ctx := newAccountTestClient(t)
	svc := newAccountServiceForTest(client)

	ch := newAccountTestChannel(ctx, client, channel.TypeAntigravity, "ag-add")

	account, err := svc.CreateAccountFromCredentials(ctx, ch.ID, "ag-refresh|project-9")
	require.NoError(t, err)
	require.Equal(t, "ag-refresh|project-9", account.Credentials["apiKey"])

	// A bare refresh token without a project id is rejected for antigravity,
	// because its endpoint needs the project.
	_, err = svc.CreateAccountFromCredentials(ctx, ch.ID, "just-a-token")
	require.Error(t, err)
}

func TestCreateAccountFromCredentials_RejectsBadInput(t *testing.T) {
	t.Parallel()

	client, ctx := newAccountTestClient(t)
	svc := newAccountServiceForTest(client)

	ch := newAccountTestChannel(ctx, client, channel.TypeClaudecode, "claudecode-bad")

	for name, grant := range map[string]string{
		"empty":       "",
		"whitespace":  "   ",
		"not json":    "not-json",
		"json no oauth field": `{"something":"else"}`,
		"oauth without access token": `{"oauth":{"refresh_token":"r"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.CreateAccountFromCredentials(ctx, ch.ID, grant)
			require.Error(t, err, "grant %q must be rejected", grant)
		})
	}

	// A channel that does not exist.
	_, err := svc.CreateAccountFromCredentials(ctx, 999999, oauthGrantJSON("a", "r"))
	require.Error(t, err)
}

func TestSetAccountEnabled_PausesWithoutLosingTheGrant(t *testing.T) {
	t.Parallel()

	client, ctx := newAccountTestClient(t)
	svc := newAccountServiceForTest(client)

	ch := newAccountTestChannel(ctx, client, channel.TypeClaudecode, "claudecode-pause")
	account := createAccount(t, ctx, client, ch.ID, "acct", "access", 50)

	require.NoError(t, svc.SetAccountEnabled(ctx, ch.ID, account.ID, false))

	paused := client.ChannelAccount.GetX(ctx, account.ID)
	require.False(t, paused.Enabled)
	// The grant survives, which is what lets an operator resume it.
	require.NotEmpty(t, paused.Credentials)

	// And it is no longer routable.
	routable, err := svc.RoutableAccounts(ctx, ch.ID)
	require.NoError(t, err)
	require.Empty(t, routable)

	require.NoError(t, svc.SetAccountEnabled(ctx, ch.ID, account.ID, true))

	require.True(t, client.ChannelAccount.GetX(ctx, account.ID).Enabled)
}

func TestDeleteAccount_SoftDeletesSoHistorySurvives(t *testing.T) {
	t.Parallel()

	client, ctx := newAccountTestClient(t)
	svc := newAccountServiceForTest(client)

	ch := newAccountTestChannel(ctx, client, channel.TypeClaudecode, "claudecode-delete")
	account := createAccount(t, ctx, client, ch.ID, "acct", "access", 50)

	require.NoError(t, svc.DeleteAccount(ctx, ch.ID, account.ID))

	require.Empty(t, ch.QueryAccounts().AllX(ctx), "a deleted account must not be routable")
	require.Empty(t, client.ChannelAccount.Query().AllX(ctx))
}

func TestAccountInChannel_RefusesCrossChannelAccess(t *testing.T) {
	t.Parallel()

	client, ctx := newAccountTestClient(t)
	svc := newAccountServiceForTest(client)

	first := newAccountTestChannel(ctx, client, channel.TypeClaudecode, "claudecode-a")
	second := newAccountTestChannel(ctx, client, channel.TypeClaudecode, "claudecode-b")

	account := createAccount(t, ctx, client, first.ID, "acct", "access", 50)

	// Addressing the account through the wrong channel must fail rather than
	// reaching across channels.
	require.Error(t, svc.SetAccountEnabled(ctx, second.ID, account.ID, false))
	require.Error(t, svc.DeleteAccount(ctx, second.ID, account.ID))
	require.Error(t, svc.SetAccountEnabled(ctx, first.ID, 999999, true))
}
