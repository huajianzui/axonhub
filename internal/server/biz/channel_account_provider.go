package biz

import (
	"context"
	"fmt"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/channelaccount"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xtime"
	"github.com/looplj/axonhub/llm/oauth"
	"github.com/looplj/axonhub/llm/transformer/anthropic/claudecode"
	"github.com/looplj/axonhub/llm/transformer/openai/codex"
	xaisubscription "github.com/looplj/axonhub/llm/transformer/xai/subscription"
)
// newAccountTokenProvider builds the provider-specific token provider for one
// account of this channel.
//
// Every subscription provider funnels into oauth.TokenProvider, so the
// differences are only the OAuth endpoints and the exchange encoding. Sharing
// that constructor gives each account the same refresh, in-flight de-duplication
// and automatic-refresh behaviour the single-grant path already had.
//
// The refresh callback writes to the account, never to the channel, so one
// account refreshing cannot overwrite another's grant.
//
// Antigravity is deliberately absent: its transformer takes a
// "<refreshToken>|<projectID>" string and builds its own token provider at
// construction time rather than resolving a credential per request, so it does
// not fit this seam. Wiring it will need a change in the transformer instead.
func (c *Channel) newAccountTokenProvider(
	credentials *oauth.OAuthCredentials,
	account *ent.ChannelAccount,
	persister accountTokenPersister,
	onRefreshed func(ctx context.Context, account *ent.ChannelAccount) error,
) *oauth.TokenProvider {
	onRefresh := func(ctx context.Context, refreshed *oauth.OAuthCredentials) error {
		if err := persister.PersistAccountCredential(ctx, account.ID, refreshed); err != nil {
			return err
		}

		if onRefreshed != nil {
			return onRefreshed(ctx, account)
		}

		return nil
	}

	switch c.Type {
	case channel.TypeCodex, channel.TypeFenno:
		return codex.NewTokenProvider(codex.TokenProviderParams{
			Credentials: credentials,
			HTTPClient:  c.HTTPClient,
			OnRefreshed: onRefresh,
		})
	case channel.TypeClaudecode:
		return claudecode.NewTokenProvider(oauth.TokenProviderParams{
			Credentials: credentials,
			HTTPClient:  c.HTTPClient,
			OnRefreshed: onRefresh,
		})
	case channel.TypeXaiSubscription:
		return xaisubscription.NewTokenProvider(xaisubscription.TokenProviderParams{
			Credentials: credentials,
			HTTPClient:  c.HTTPClient,
			OnRefreshed: onRefresh,
		})
	default:
		// A subscription type without a dedicated provider keeps the generic
		// endpoints, so refresh fails loudly instead of silently misbehaving.
		return oauth.NewTokenProvider(oauth.TokenProviderParams{
			Credentials: credentials,
			HTTPClient:  c.HTTPClient,
			OnRefreshed: onRefresh,
		})
	}
}

// SetAccountEnabled pauses or resumes one account.
//
// Disabling is an operator action, independent of the grant's authorization
// state: a paused account keeps its credential and its history, so it can be
// resumed later. The channel is touched either way so the runtime picks up the
// change, because the account cache is only rebuilt when a channel row moves.
func (svc *ChannelAccountService) SetAccountEnabled(ctx context.Context, channelID, accountID int, enabled bool) error {
	account, err := svc.accountInChannel(ctx, channelID, accountID)
	if err != nil {
		return err
	}

	if _, err := account.Update().SetEnabled(enabled).Save(ctx); err != nil {
		return fmt.Errorf("set account %d enabled=%t: %w", accountID, enabled, err)
	}

	return svc.TouchChannel(ctx, channelID)
}

// DeleteAccount removes one account from a channel.
//
// The row is soft-deleted so its history survives. Deleting the last account
// leaves the channel with no account, which makes it fall back to its own
// inline credential; that is deliberate, because removing an account should not
// be able to take a channel out of service.
func (svc *ChannelAccountService) DeleteAccount(ctx context.Context, channelID, accountID int) error {
	account, err := svc.accountInChannel(ctx, channelID, accountID)
	if err != nil {
		return err
	}

	if err := svc.entFromContext(ctx).ChannelAccount.DeleteOneID(account.ID).Exec(ctx); err != nil {
		return fmt.Errorf("delete account %d: %w", accountID, err)
	}

	return svc.TouchChannel(ctx, channelID)
}

// accountInChannel loads an account, refusing one that belongs to another
// channel so a mistyped id cannot reach across channels.
func (svc *ChannelAccountService) accountInChannel(ctx context.Context, channelID, accountID int) (*ent.ChannelAccount, error) {
	account, err := svc.entFromContext(ctx).ChannelAccount.Query().
		Where(
			channelaccount.IDEQ(accountID),
			channelaccount.ChannelIDEQ(channelID),
		).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("account %d not found on channel %d", accountID, channelID)
		}

		return nil, fmt.Errorf("load account %d on channel %d: %w", accountID, channelID, err)
	}

	return account, nil
}

// PersistAccountCredential writes a refreshed credential back to its account row.
//
// Only the credential and its freshness are touched: the account's identity,
// weight and enabled state belong to the operator.
//
// A successful refresh also clears any previous auth failure, because a grant
// that just produced a token is by definition usable again.
func (svc *ChannelAccountService) PersistAccountCredential(ctx context.Context, accountID int, refreshed *oauth.OAuthCredentials) error {
	if refreshed == nil {
		return nil
	}

	account, err := svc.entFromContext(ctx).ChannelAccount.Get(ctx, accountID)
	if err != nil {
		return fmt.Errorf("load account %d to persist refreshed credential: %w", accountID, err)
	}

	projected, err := objects.ChannelCredentialsFromAccountCredential(account.Credentials)
	if err != nil {
		return fmt.Errorf("read grant of account %d: %w", accountID, err)
	}

	// Preserve the shape the account was stored in, so a provider whose grant
	// lives in the legacy field keeps that field populated.
	updated := objects.ChannelCredentials{APIKey: projected.APIKey, OAuth: refreshed}

	stored, err := objects.AccountCredentialFromChannelCredentials(updated)
	if err != nil {
		return fmt.Errorf("encode refreshed credential for account %d: %w", accountID, err)
	}

	fingerprint, err := objects.AccountCredentialFingerprint(stored)
	if err != nil {
		return fmt.Errorf("fingerprint refreshed credential for account %d: %w", accountID, err)
	}

	update := svc.entFromContext(ctx).ChannelAccount.UpdateOneID(accountID).
		SetCredentials(stored).
		SetCredentialFingerprint(fingerprint).
		SetAuthState(channelaccount.AuthStateReady).
		SetAuthErrorCode("").
		SetLastRefreshAt(xtime.UTCNow())

	if !refreshed.ExpiresAt.IsZero() {
		update = update.SetExpiresAt(refreshed.ExpiresAt)
	}

	if _, err := update.Save(ctx); err != nil {
		return fmt.Errorf("persist refreshed credential for account %d: %w", accountID, err)
	}

	if log.DebugEnabled(ctx) {
		log.Debug(ctx, "persisted refreshed account credential", log.Int("account_id", accountID))
	}

	return nil
}
