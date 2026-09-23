package biz

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/oauth"
)

// accountTokenChannel builds a channel snapshot carrying the given accounts, as
// the projection would leave it.
func accountTokenChannel(t *testing.T, ctx context.Context, client *ent.Client, accounts ...*ent.ChannelAccount) *Channel {
	t.Helper()

	ch := newAccountTestChannel(ctx, client, channel.TypeClaudecode, "claudecode-tokens",
		objects.ChannelCredentials{OAuth: &oauth.OAuthCredentials{AccessToken: "inline", RefreshToken: "r"}},
	)

	snap := &Channel{
		Channel:        ch,
		cachedAccounts: accounts,
	}

	return snap
}

// createAccount persists an account whose grant carries the given access token.
func createAccount(t *testing.T, ctx context.Context, client *ent.Client, channelID int, identity, accessToken string, weight int) *ent.ChannelAccount {
	t.Helper()

	stored, err := objects.AccountCredentialFromChannelCredentials(objects.ChannelCredentials{
		OAuth: &oauth.OAuthCredentials{AccessToken: accessToken, RefreshToken: "refresh-" + identity},
	})
	require.NoError(t, err)

	return client.ChannelAccount.Create().
		SetChannelID(channelID).
		SetIdentity(identity).
		SetCredentialFingerprint("cred-" + identity).
		SetCredentials(stored).
		SetWeight(weight).
		SaveX(ctx)
}

func TestAccountTokenGetter_NilWithoutAccounts(t *testing.T) {
	t.Parallel()

	client, ctx := newAccountTestClient(t)
	ch := newAccountTestChannel(ctx, client, channel.TypeClaudecode, "claudecode-none",
		objects.ChannelCredentials{OAuth: &oauth.OAuthCredentials{AccessToken: "inline", RefreshToken: "r"}},
	)

	// A channel with no accounts must keep the pre-account token provider.
	require.Nil(t, NewChannelAccountTokenGetter(AccountTokenGetterParams{Channel: &Channel{Channel: ch}, Persister: newAccountServiceForTest(client)}))
	require.Nil(t, NewChannelAccountTokenGetter(AccountTokenGetterParams{}))
}

func TestAccountTokenGetter_SingleAccountIsUnchanged(t *testing.T) {
	t.Parallel()

	client, ctx := newAccountTestClient(t)
	ch := newAccountTestChannel(ctx, client, channel.TypeClaudecode, "claudecode-single")
	account := createAccount(t, ctx, client, ch.ID, "only", "only-access", 50)

	snap := accountTokenChannel(t, ctx, client, account)

	getter := NewChannelAccountTokenGetter(AccountTokenGetterParams{Channel: snap, Persister: newAccountServiceForTest(client)})
	require.NotNil(t, getter)

	// With one account the credential must be identical to what the inline path
	// produced, so a migrated channel is behaviourally unchanged.
	creds, err := getter.Get(ctx)
	require.NoError(t, err)
	require.NotNil(t, creds)
	require.Equal(t, "only-access", creds.AccessToken)
	require.Equal(t, "refresh-only", creds.RefreshToken)
}

// TestAccountTokenGetter_DifferentAccountsServeDifferentRequests is the whole
// point of the feature: one channel, several accounts, and the account is chosen
// per request.
func TestAccountTokenGetter_DifferentAccountsServeDifferentRequests(t *testing.T) {
	t.Parallel()

	client, ctx := newAccountTestClient(t)
	ch := newAccountTestChannel(ctx, client, channel.TypeClaudecode, "claudecode-multi")

	first := createAccount(t, ctx, client, ch.ID, "acct-a", "access-a", 50)
	second := createAccount(t, ctx, client, ch.ID, "acct-b", "access-b", 50)

	snap := accountTokenChannel(t, ctx, client, first, second)

	getter := NewChannelAccountTokenGetter(AccountTokenGetterParams{Channel: snap, Persister: newAccountServiceForTest(client)})
	require.NotNil(t, getter)

	seen := map[string]bool{}
	for traceIdx := 1; traceIdx <= 6; traceIdx++ {
		traceCtx := contexts.WithTrace(ctx, &ent.Trace{ID: traceIdx})

		creds, err := getter.Get(traceCtx)
		require.NoError(t, err)

		seen[creds.AccessToken] = true
	}

	// Different traces must be able to reach different accounts; with one
	// account per channel this set could only ever hold a single value.
	require.Greater(t, len(seen), 1,
		"expected traces to spread across both accounts, saw %v", seen)
}

func TestAccountTokenGetter_IsStickyPerTrace(t *testing.T) {
	t.Parallel()

	client, ctx := newAccountTestClient(t)
	ch := newAccountTestChannel(ctx, client, channel.TypeClaudecode, "claudecode-sticky")

	first := createAccount(t, ctx, client, ch.ID, "acct-a", "access-a", 50)
	second := createAccount(t, ctx, client, ch.ID, "acct-b", "access-b", 50)

	snap := accountTokenChannel(t, ctx, client, first, second)
	getter := NewChannelAccountTokenGetter(AccountTokenGetterParams{Channel: snap, Persister: newAccountServiceForTest(client)})

	traceCtx := contexts.WithTrace(ctx, &ent.Trace{ID: 1001})

	initial, err := getter.Get(traceCtx)
	require.NoError(t, err)

	// The same trace must keep landing on the same account: a conversation that
	// switched accounts mid-flight would break upstream prompt caching.
	for i := 0; i < 10; i++ {
		again, err := getter.Get(traceCtx)
		require.NoError(t, err)
		require.Equal(t, initial.AccessToken, again.AccessToken,
			"the same trace must stay on one account")
	}
}

func TestAccountTokenGetter_StickyAccountSurvivesASetChange(t *testing.T) {
	t.Parallel()

	client, ctx := newAccountTestClient(t)
	ch := newAccountTestChannel(ctx, client, channel.TypeClaudecode, "claudecode-survive")

	first := createAccount(t, ctx, client, ch.ID, "acct-a", "access-a", 50)
	second := createAccount(t, ctx, client, ch.ID, "acct-b", "access-b", 50)

	snap := accountTokenChannel(t, ctx, client, first, second)
	getter := NewChannelAccountTokenGetter(AccountTokenGetterParams{Channel: snap, Persister: newAccountServiceForTest(client)})

	traceCtx := contexts.WithTrace(ctx, &ent.Trace{ID: 1002})

	initial, err := getter.Get(traceCtx)
	require.NoError(t, err)

	// A third account appears, as it would after an operator adds one.
	third := createAccount(t, ctx, client, ch.ID, "acct-c", "access-c", 50)
	snap.cachedAccounts = []*ent.ChannelAccount{first, second, third}

	again, err := getter.Get(traceCtx)
	require.NoError(t, err)
	require.Equal(t, initial.AccessToken, again.AccessToken,
		"adding an account must not move an existing trace")
}

func TestAccountTokenGetter_FallsBackWhenStickyAccountLeaves(t *testing.T) {
	t.Parallel()

	client, ctx := newAccountTestClient(t)
	ch := newAccountTestChannel(ctx, client, channel.TypeClaudecode, "claudecode-leave")

	first := createAccount(t, ctx, client, ch.ID, "acct-a", "access-a", 50)
	second := createAccount(t, ctx, client, ch.ID, "acct-b", "access-b", 50)

	snap := accountTokenChannel(t, ctx, client, first, second)
	getter := NewChannelAccountTokenGetter(AccountTokenGetterParams{Channel: snap, Persister: newAccountServiceForTest(client)})

	traceCtx := contexts.WithTrace(ctx, &ent.Trace{ID: 1003})

	initial, err := getter.Get(traceCtx)
	require.NoError(t, err)

	// The sticky account becomes unroutable, so it drops out of the snapshot.
	survivor := first
	if initial.AccessToken == first.Credentials["oauth"].(map[string]any)["access_token"] {
		survivor = second
	}

	snap.cachedAccounts = []*ent.ChannelAccount{survivor}

	again, err := getter.Get(traceCtx)
	require.NoError(t, err)
	require.NotNil(t, again, "a trace must still be served when its account leaves")
	require.NotEqual(t, initial.AccessToken, again.AccessToken)
}

func TestAccountTokenGetter_ErrorsWhenAccountHoldsNoGrant(t *testing.T) {
	t.Parallel()

	client, ctx := newAccountTestClient(t)
	ch := newAccountTestChannel(ctx, client, channel.TypeClaudecode, "claudecode-badgrant")

	// An account row whose credential is not an OAuth grant.
	bad := client.ChannelAccount.Create().
		SetChannelID(ch.ID).
		SetIdentity("bad").
		SetCredentialFingerprint("cred-bad").
		SetCredentials(map[string]any{"something": "else"}).
		SaveX(ctx)

	snap := accountTokenChannel(t, ctx, client, bad)

	_, err := NewChannelAccountTokenGetter(AccountTokenGetterParams{Channel: snap, Persister: newAccountServiceForTest(client)}).Get(ctx)
	require.Error(t, err, "an account with no oauth grant must be reported, not silently used")
}

func TestAccountCredentialRefIsPerAccount(t *testing.T) {
	t.Parallel()

	// The ref must distinguish accounts, because auto-disable uses it to keep
	// serving on the other accounts of a channel.
	require.NotEqual(t, accountCredentialRef(1), accountCredentialRef(2))
	require.Equal(t, "account:7", accountCredentialRef(7))

	// And it must not collide with the legacy single-account sentinel.
	require.NotEqual(t, objects.OAuthCredentialRef, accountCredentialRef(1))
}
