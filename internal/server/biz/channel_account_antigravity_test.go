package biz

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
)

func TestProjectIDFromAntigravityCredential(t *testing.T) {
	t.Parallel()

	tests := []struct {
		credential string
		want       string
		wantErr    bool
	}{
		{"ag-refresh|project-123", "project-123", false},
		{"ag-refresh|  spaced-project  ", "spaced-project", false},
		// A credential with no separator carries no project.
		{"just-a-token", "", true},
		// An empty project half is not a usable project.
		{"ag-refresh|", "", true},
		{"", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.credential, func(t *testing.T) {
			t.Parallel()

			got, err := projectIDFromAntigravityCredential(tt.credential)
			if tt.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

// createAgAccount provisions an Antigravity account holding a
// "<refreshToken>|<projectID>" grant.
func createAgAccount(t *testing.T, ctx context.Context, client *ent.Client, channelID int, identity, credential string) *ent.ChannelAccount {
	t.Helper()

	return client.ChannelAccount.Create().
		SetChannelID(channelID).
		SetIdentity(identity).
		SetCredentials(map[string]any{"apiKey": credential}).
		SetCredentialFingerprint("cred-" + identity).
		SaveX(ctx)
}

// TestGrantForRequest_ResolvesProjectAndTokensPerAccount is what makes
// Antigravity's multi-account refresh correct: both halves of the credential
// come from the same account, so the project billed is the account's own.
func TestGrantForRequest_ResolvesProjectAndTokensPerAccount(t *testing.T) {
	t.Parallel()

	client, ctx := newAccountTestClient(t)

	ch := newAccountTestChannel(ctx, client, channel.TypeAntigravity, "ag-grant",
		objects.ChannelCredentials{APIKey: "inline-refresh|inline-project"},
	)

	// Two accounts, each with its own project.
	createAgAccount(t, ctx, client, ch.ID, "first", "first-refresh|first-project")
	createAgAccount(t, ctx, client, ch.ID, "second", "second-refresh|second-project")

	snap := &Channel{Channel: client.Channel.GetX(ctx, ch.ID)}
	accounts, err := newAccountServiceForTest(client).RoutableAccounts(ctx, ch.ID)
	require.NoError(t, err)
	require.Len(t, accounts, 2)
	snap.cachedAccounts = accounts

	getter := NewChannelAccountTokenGetter(AccountTokenGetterParams{
		Channel:   snap,
		Persister: newAccountServiceForTest(client),
	})
	require.NotNil(t, getter)

	accountGetter, ok := getter.(*ChannelAccountTokenGetter)
	require.True(t, ok)

	grant, ok := accountGetter.GrantForRequest(ctx)
	require.True(t, ok, "an antigravity channel with accounts must resolve a grant")
	require.NotNil(t, grant.Tokens, "the grant must carry a token provider")

	// The project comes from an account, never from the channel's inline value,
	// which is what lets several accounts bill different projects.
	require.Contains(t, []string{"first-project", "second-project"}, grant.Project)
	require.NotEqual(t, "inline-project", grant.Project)
}

// TestGrantForRequest_ReportsFalseWhenProjectIsMissing keeps a malformed account
// from producing a request that upstream would reject without explanation.
func TestGrantForRequest_ReportsFalseWhenProjectIsMissing(t *testing.T) {
	t.Parallel()

	client, ctx := newAccountTestClient(t)

	ch := newAccountTestChannel(ctx, client, channel.TypeAntigravity, "ag-bad-project",
		objects.ChannelCredentials{APIKey: "inline-refresh|inline-project"},
	)

	// An account whose credential carries no project id.
	createAgAccount(t, ctx, client, ch.ID, "broken", "no-separator-here")

	snap := &Channel{Channel: client.Channel.GetX(ctx, ch.ID)}
	accounts, err := newAccountServiceForTest(client).RoutableAccounts(ctx, ch.ID)
	require.NoError(t, err)
	snap.cachedAccounts = accounts

	accountGetter := NewChannelAccountTokenGetter(AccountTokenGetterParams{
		Channel:   snap,
		Persister: newAccountServiceForTest(client),
	}).(*ChannelAccountTokenGetter)

	_, ok := accountGetter.GrantForRequest(ctx)
	require.False(t, ok, "an account without a project must not produce a grant")
}

// TestGrantForRequest_ReportsFalseWithoutAccounts keeps the fallback honest: a
// channel with nothing to resolve must let the transformer use its own
// credential rather than fail the request.
func TestGrantForRequest_ReportsFalseWithoutAccounts(t *testing.T) {
	t.Parallel()

	client, ctx := newAccountTestClient(t)

	ch := newAccountTestChannel(ctx, client, channel.TypeAntigravity, "ag-no-accounts",
		objects.ChannelCredentials{APIKey: "inline-refresh|inline-project"},
	)

	getter := NewChannelAccountTokenGetter(AccountTokenGetterParams{
		Channel:   &Channel{Channel: ch},
		Persister: newAccountServiceForTest(client),
	})
	require.Nil(t, getter, "no accounts means the caller keeps the built-in provider")
}
