package biz

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/channelaccount"
	"github.com/looplj/axonhub/internal/objects"
)

func TestParseAccountCredentialRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		ref    string
		wantID int
		wantOK bool
	}{
		{"account:7", 7, true},
		{"account:1", 1, true},
		// The legacy single-credential sentinel is not an account reference.
		{objects.OAuthCredentialRef, 0, false},
		// A plain API key is not an account reference.
		{"sk-abc", 0, false},
		{"account:", 0, false},
		{"account:0", 0, false},
		{"account:-3", 0, false},
		{"account:abc", 0, false},
		{"", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			t.Parallel()

			id, ok := parseAccountCredentialRef(tt.ref)
			require.Equal(t, tt.wantOK, ok, "ref %q", tt.ref)

			if tt.wantOK {
				require.Equal(t, tt.wantID, id)
			}
		})
	}
}

// TestDisableAPIKey_KeepsChannelServingWhenAnotherAccountRemains is the point of
// per-account disable: losing one account must not take the channel out of
// service while other accounts can still serve.
func TestDisableAPIKey_KeepsChannelServingWhenAnotherAccountRemains(t *testing.T) {
	t.Parallel()

	svc, ctx, client := accountService(t)

	ch := newAccountTestChannel(ctx, client, channel.TypeClaudecode, "claudecode-two-accounts")
	first := createAccount(t, ctx, client, ch.ID, "first", "first-access", 50)
	createAccount(t, ctx, client, ch.ID, "second", "second-access", 50)

	// One account fails.
	require.NoError(t, svc.DisableAPIKey(ctx, ch.ID, accountCredentialRef(first.ID), 401, "unauthorized"))

	reloaded := client.Channel.GetX(ctx, ch.ID)

	// The channel stays in service.
	require.Equal(t, channel.StatusEnabled, reloaded.Status,
		"a channel with another usable account must keep serving")

	// The failing account is recorded as disabled.
	require.Len(t, reloaded.DisabledAPIKeys, 1)
	require.Equal(t, accountCredentialRef(first.ID), reloaded.DisabledAPIKeys[0].Key)
}

// TestDisableAPIKey_DisablesChannelWhenLastAccountFails is the other half: once
// every account is gone the channel has to stop, otherwise requests would keep
// hitting a channel with nothing to authenticate with.
func TestDisableAPIKey_DisablesChannelWhenLastAccountFails(t *testing.T) {
	t.Parallel()

	svc, ctx, client := accountService(t)

	ch := newAccountTestChannel(ctx, client, channel.TypeClaudecode, "claudecode-one-account")
	only := createAccount(t, ctx, client, ch.ID, "only", "only-access", 50)

	require.NoError(t, svc.DisableAPIKey(ctx, ch.ID, accountCredentialRef(only.ID), 401, "unauthorized"))

	reloaded := client.Channel.GetX(ctx, ch.ID)
	require.Equal(t, channel.StatusDisabled, reloaded.Status,
		"a channel whose every account failed must be disabled")
	require.NotNil(t, reloaded.AutoDisabledAt)
	require.NotNil(t, reloaded.ErrorMessage)
	require.Contains(t, *reloaded.ErrorMessage, allKeysDisabledErrorPrefix)
}

// TestDisableAPIKey_IgnoresUnknownAccount covers a stale reference: it must not
// be able to disable a channel it does not belong to.
func TestDisableAPIKey_IgnoresUnknownAccount(t *testing.T) {
	t.Parallel()

	svc, ctx, client := accountService(t)

	ch := newAccountTestChannel(ctx, client, channel.TypeClaudecode, "claudecode-stale")
	createAccount(t, ctx, client, ch.ID, "real", "real-access", 50)

	// A reference to an account on a different channel.
	other := newAccountTestChannel(ctx, client, channel.TypeClaudecode, "claudecode-other")
	foreign := createAccount(t, ctx, client, other.ID, "foreign", "foreign-access", 50)

	require.NoError(t, svc.DisableAPIKey(ctx, ch.ID, accountCredentialRef(foreign.ID), 401, "unauthorized"))

	reloaded := client.Channel.GetX(ctx, ch.ID)
	require.Equal(t, channel.StatusEnabled, reloaded.Status)
	require.Empty(t, reloaded.DisabledAPIKeys, "a foreign account reference must be ignored")
}

// TestDisableAPIKey_IgnoresAccountThatIsAlreadyUnroutable makes sure a disabled
// or re-authorization-required account cannot be counted as a fallback.
func TestDisableAPIKey_IgnoresAccountThatIsAlreadyUnroutable(t *testing.T) {
	t.Parallel()

	svc, ctx, client := accountService(t)

	ch := newAccountTestChannel(ctx, client, channel.TypeClaudecode, "claudecode-unroutable")
	failing := createAccount(t, ctx, client, ch.ID, "failing", "failing-access", 50)
	broken := createAccount(t, ctx, client, ch.ID, "broken", "broken-access", 50)

	// The other account already needs re-authorization, so it is not a fallback.
	broken.Update().
		SetAuthState(channelaccount.AuthStateReauthorizationRequired).
		SaveX(ctx)

	require.NoError(t, svc.DisableAPIKey(ctx, ch.ID, accountCredentialRef(failing.ID), 401, "unauthorized"))

	reloaded := client.Channel.GetX(ctx, ch.ID)
	require.Equal(t, channel.StatusDisabled, reloaded.Status,
		"an account that cannot serve must not count as a fallback")
}

// TestDisableAPIKey_IgnoresDisabledAccountAsFallback covers the operator switch:
// a disabled account is not a fallback either.
func TestDisableAPIKey_IgnoresDisabledAccountAsFallback(t *testing.T) {
	t.Parallel()

	svc, ctx, client := accountService(t)

	ch := newAccountTestChannel(ctx, client, channel.TypeClaudecode, "claudecode-disabled-fallback")
	failing := createAccount(t, ctx, client, ch.ID, "failing", "failing-access", 50)
	paused := createAccount(t, ctx, client, ch.ID, "paused", "paused-access", 50)

	paused.Update().SetEnabled(false).SaveX(ctx)

	require.NoError(t, svc.DisableAPIKey(ctx, ch.ID, accountCredentialRef(failing.ID), 401, "unauthorized"))

	require.Equal(t, channel.StatusDisabled, client.Channel.GetX(ctx, ch.ID).Status)
}

// TestDisableAPIKey_StillHandlesChannelLevelCredentials guards the pre-existing
// behaviour: a plain API key and the legacy single-credential sentinel must keep
// working exactly as before.
func TestDisableAPIKey_StillHandlesChannelLevelCredentials(t *testing.T) {
	t.Parallel()

	svc, ctx, client := accountService(t)

	ch := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("plain-openai-disable").
		SetStatus(channel.StatusEnabled).
		SetBaseURL("https://api.openai.com/v1").
		SetSupportedModels([]string{"m"}).
		SetDefaultTestModel("m").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{"sk-a", "sk-b"}}).
		SaveX(ctx)

	// Disabling one of two keys keeps the channel in service.
	require.NoError(t, svc.DisableAPIKey(ctx, ch.ID, "sk-a", 401, "unauthorized"))

	reloaded := client.Channel.GetX(ctx, ch.ID)
	require.Equal(t, channel.StatusEnabled, reloaded.Status)
	require.Len(t, reloaded.DisabledAPIKeys, 1)
	require.Equal(t, "sk-a", reloaded.DisabledAPIKeys[0].Key)

	// Disabling the last one takes the channel down.
	require.NoError(t, svc.DisableAPIKey(ctx, ch.ID, "sk-b", 401, "unauthorized"))

	require.Equal(t, channel.StatusDisabled, client.Channel.GetX(ctx, ch.ID).Status)
}

// TestAccountTokenGetter_RecordsTheChosenAccountOnTheContext is what makes the
// whole flow work: the account recorded here is the one auto-disable will act on.
func TestAccountTokenGetter_RecordsTheChosenAccountOnTheContext(t *testing.T) {
	t.Parallel()

	client, baseCtx := newAccountTestClient(t)

	ch := newAccountTestChannel(baseCtx, client, channel.TypeClaudecode, "claudecode-records")
	account := createAccount(t, baseCtx, client, ch.ID, "only", "only-access", 50)

	snap := accountTokenChannel(t, baseCtx, client, account)

	getter := NewChannelAccountTokenGetter(AccountTokenGetterParams{
		Channel:   snap,
		Persister: newAccountServiceForTest(client),
	})
	require.NotNil(t, getter)

	// The production request path already shares a mutable value container (the
	// trace middleware installs one), so mirror that here.
	ctx := contexts.EnsureContainer(baseCtx)

	_, err := getter.Get(ctx)
	require.NoError(t, err)

	recorded, ok := contexts.GetChannelAPIKey(ctx)
	require.True(t, ok, "the getter must record the account it chose")
	require.Equal(t, accountCredentialRef(account.ID), recorded)

	// And that recorded reference must be exactly what an account disable accepts.
	id, ok := parseAccountCredentialRef(recorded)
	require.True(t, ok)
	require.Equal(t, account.ID, id)
}
