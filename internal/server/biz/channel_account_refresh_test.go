package biz

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/channelaccount"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/oauth"
)

// TestPersistAccountCredential_WritesBackToTheAccount is the guarantee behind
// per-account refresh: a refreshed token lands on its own account row, so one
// account refreshing can never overwrite another's grant.
func TestPersistAccountCredential_WritesBackToTheAccount(t *testing.T) {
	t.Parallel()

	client, ctx := newAccountTestClient(t)
	svc := newAccountServiceForTest(client)

	ch := newAccountTestChannel(ctx, client, channel.TypeClaudecode, "claudecode-persist")
	account := createAccount(t, ctx, client, ch.ID, "acct", "old-access", 50)

	refreshed := &oauth.OAuthCredentials{
		AccessToken:  "new-access",
		RefreshToken: "new-refresh",
		TokenType:    "Bearer",
		ExpiresAt:    time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second),
	}

	require.NoError(t, svc.PersistAccountCredential(ctx, account.ID, refreshed))

	reloaded := client.ChannelAccount.GetX(ctx, account.ID)

	projected, err := objects.ChannelCredentialsFromAccountCredential(reloaded.Credentials)
	require.NoError(t, err)
	require.NotNil(t, projected.OAuth)
	require.Equal(t, "new-access", projected.OAuth.AccessToken)
	require.Equal(t, "new-refresh", projected.OAuth.RefreshToken)

	// Freshness bookkeeping is updated so the next read can tell it is current.
	require.NotNil(t, reloaded.ExpiresAt)
	require.NotNil(t, reloaded.LastRefreshAt)
	require.Equal(t, channelaccount.AuthStateReady, reloaded.AuthState)

	// The identity fingerprint changed with the credential, which is what keeps
	// the per-channel uniqueness index accurate.
	require.NotEmpty(t, reloaded.CredentialFingerprint)
	require.NotEqual(t, account.CredentialFingerprint, reloaded.CredentialFingerprint)
}

// TestPersistAccountCredential_DoesNotTouchTheChannel is what makes accounts
// independent: refreshing one account must leave the channel's own credential,
// and every other account, alone.
func TestPersistAccountCredential_DoesNotTouchTheChannel(t *testing.T) {
	t.Parallel()

	client, ctx := newAccountTestClient(t)
	svc := newAccountServiceForTest(client)

	ch := newAccountTestChannel(ctx, client, channel.TypeClaudecode, "claudecode-isolation",
		objects.ChannelCredentials{OAuth: &oauth.OAuthCredentials{AccessToken: "inline", RefreshToken: "inline"}},
	)

	first := createAccount(t, ctx, client, ch.ID, "first", "first-access", 50)
	second := createAccount(t, ctx, client, ch.ID, "second", "second-access", 50)

	channelBefore := client.Channel.GetX(ctx, ch.ID).Credentials.OAuth.AccessToken
	secondBefore := second.Credentials

	require.NoError(t, svc.PersistAccountCredential(ctx, first.ID, &oauth.OAuthCredentials{
		AccessToken:  "first-rotated",
		RefreshToken: "first-rotated-refresh",
	}))

	// The channel's own credential is untouched.
	require.Equal(t, channelBefore, client.Channel.GetX(ctx, ch.ID).Credentials.OAuth.AccessToken)

	// The sibling account is untouched.
	secondAfter := client.ChannelAccount.GetX(ctx, second.ID)
	require.Equal(t, secondBefore["oauth"].(map[string]any)["access_token"],
		secondAfter.Credentials["oauth"].(map[string]any)["access_token"])

	// The refreshed account did change.
	firstAfter := client.ChannelAccount.GetX(ctx, first.ID)
	require.Equal(t, "first-rotated", firstAfter.Credentials["oauth"].(map[string]any)["access_token"])
}

// TestPersistAccountCredential_PreservesLegacyFieldShape covers a grant stored
// outside the OAuth object, whose extra field must survive a refresh.
func TestPersistAccountCredential_PreservesLegacyFieldShape(t *testing.T) {
	t.Parallel()

	client, ctx := newAccountTestClient(t)
	svc := newAccountServiceForTest(client)

	ch := newAccountTestChannel(ctx, client, channel.TypeAntigravity, "ag-legacy-shape")

	account := client.ChannelAccount.Create().
		SetChannelID(ch.ID).
		SetIdentity("ag").
		SetCredentials(map[string]any{"apiKey": "ag-refresh|project-123"}).
		SetCredentialFingerprint("cred-ag").
		SaveX(ctx)

	require.NoError(t, svc.PersistAccountCredential(ctx, account.ID, &oauth.OAuthCredentials{
		AccessToken:  "ag-access",
		RefreshToken: "ag-rotated",
	}))

	reloaded := client.ChannelAccount.GetX(ctx, account.ID)

	// The project id lived in the legacy field and must not be dropped.
	require.Equal(t, "ag-refresh|project-123", reloaded.Credentials["apiKey"])
	require.NotNil(t, reloaded.Credentials["oauth"])
}

func TestPersistAccountCredential_ClearsPreviousAuthFailure(t *testing.T) {
	t.Parallel()

	client, ctx := newAccountTestClient(t)
	svc := newAccountServiceForTest(client)

	ch := newAccountTestChannel(ctx, client, channel.TypeClaudecode, "claudecode-recovered")
	account := createAccount(t, ctx, client, ch.ID, "acct", "access", 50)

	// The account had been marked as needing re-authorization.
	account.Update().
		SetAuthState(channelaccount.AuthStateReauthorizationRequired).
		SetAuthErrorCode("refresh_rejected").
		SaveX(ctx)

	require.NoError(t, svc.PersistAccountCredential(ctx, account.ID, &oauth.OAuthCredentials{
		AccessToken:  "recovered",
		RefreshToken: "recovered-refresh",
	}))

	reloaded := client.ChannelAccount.GetX(ctx, account.ID)
	require.Equal(t, channelaccount.AuthStateReady, reloaded.AuthState,
		"a grant that just produced a token is usable again")
	require.Empty(t, reloaded.AuthErrorCode)
}

func TestPersistAccountCredential_IgnoresNil(t *testing.T) {
	t.Parallel()

	client, ctx := newAccountTestClient(t)
	svc := newAccountServiceForTest(client)

	ch := newAccountTestChannel(ctx, client, channel.TypeClaudecode, "claudecode-nil")
	account := createAccount(t, ctx, client, ch.ID, "acct", "access", 50)

	require.NoError(t, svc.PersistAccountCredential(ctx, account.ID, nil))

	// Nothing changed.
	require.Equal(t, account.Credentials["oauth"].(map[string]any)["access_token"],
		client.ChannelAccount.GetX(ctx, account.ID).Credentials["oauth"].(map[string]any)["access_token"])
}
