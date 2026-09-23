package biz

import (
	"context"
	"fmt"
	"sort"

	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channelaccount"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xtime"
)

// ChannelAccountService reads the subscription accounts of a channel.
//
// OAuth channels used to hold a single credential inline on the channel row.
// Accounts are now their own entity, and this service is the single place that
// answers "which grants does this channel have?".
//
// It deliberately does not select between accounts or refresh them yet: a
// migrated channel holds exactly one account, so for now the projected value is
// the same credential the channel already carries. That keeps the account table
// on the read path without changing any observable behaviour.
type ChannelAccountService struct {
	*AbstractService
}

// ChannelAccountServiceParams resolves the account service dependencies.
type ChannelAccountServiceParams struct {
	fx.In

	Ent *ent.Client
}

// NewChannelAccountService builds the account service.
func NewChannelAccountService(params ChannelAccountServiceParams) *ChannelAccountService {
	return &ChannelAccountService{
		AbstractService: &AbstractService{db: params.Ent},
	}
}

// ProjectAccountsOntoChannels replaces each channel's inline credential with the
// one from its preferred subscription account, in place.
//
// This is how the account table joins the read path. Projecting onto the entity
// before the channel snapshot is built means every existing reader of
// Channel.Credentials keeps working unchanged.
//
// Accounts are loaded for all channels at once rather than per channel, because
// this runs on every cache reload.
//
// It is intentionally a no-op in the cases that would otherwise break a running
// channel:
//   - the channel has no usable account (created before accounts existed, or an
//     API-key channel), so its inline credential stays authoritative;
//   - the account's grant cannot be projected, because losing a working
//     credential to a malformed row is a worse failure than ignoring the row.
func (svc *ChannelAccountService) ProjectAccountsOntoChannels(ctx context.Context, channels []*ent.Channel) {
	if len(channels) == 0 {
		return
	}

	byChannel, err := svc.routableAccountsByChannel(ctx, channelIDs(channels))
	if err != nil {
		log.Warn(ctx, "failed to load channel accounts, keeping inline credentials", log.Cause(err))

		return
	}

	if len(byChannel) == 0 {
		return
	}

	for _, ch := range channels {
		accounts := byChannel[ch.ID]
		if len(accounts) == 0 {
			continue
		}

		projected, err := objects.ChannelCredentialsFromAccountCredential(accounts[0].Credentials)
		if err != nil {
			log.Warn(ctx, "skipping account projection for channel with unreadable grant",
				log.Int("channel_id", ch.ID),
				log.Int("account_id", accounts[0].ID),
				log.Cause(err),
			)

			continue
		}

		// Never let an account empty out a working channel.
		if projected.APIKey == "" && projected.OAuth == nil {
			continue
		}

		ch.Credentials = projected

		if log.DebugEnabled(ctx) {
			log.Debug(ctx, "projected channel account onto credentials",
				log.Int("channel_id", ch.ID),
				log.String("channel", ch.Name),
				log.Int("account_id", accounts[0].ID),
			)
		}
	}
}

// routableAccountsByChannel loads the routable accounts of the given channels,
// grouped by channel and ordered most preferred first.
func (svc *ChannelAccountService) routableAccountsByChannel(ctx context.Context, ids []int) (map[int][]*ent.ChannelAccount, error) {
	accounts, err := svc.entFromContext(ctx).ChannelAccount.Query().
		Where(
			channelaccount.ChannelIDIn(ids...),
			channelaccount.EnabledEQ(true),
			channelaccount.AuthStateEQ(channelaccount.AuthStateReady),
		).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query routable accounts: %w", err)
	}

	byChannel := make(map[int][]*ent.ChannelAccount, len(accounts))
	for _, account := range accounts {
		byChannel[account.ChannelID] = append(byChannel[account.ChannelID], account)
	}

	// Highest weight first, then stable by id so selection is deterministic when
	// weights tie.
	for channelID := range byChannel {
		group := byChannel[channelID]
		sort.SliceStable(group, func(i, j int) bool {
			if group[i].Weight != group[j].Weight {
				return group[i].Weight > group[j].Weight
			}

			return group[i].ID < group[j].ID
		})
	}

	return byChannel, nil
}

// channelIDs extracts the ids of the given channels.
func channelIDs(channels []*ent.Channel) []int {
	ids := make([]int, 0, len(channels))

	for _, ch := range channels {
		if ch != nil {
			ids = append(ids, ch.ID)
		}
	}

	return ids
}

// RoutableAccounts returns the accounts of a channel that may serve traffic,
// most preferred first.
//
// An account is routable when it is enabled and its grant is ready. Accounts
// that need re-authorization are excluded rather than retried, because a grant
// the provider has already rejected will not start working again on its own.
func (svc *ChannelAccountService) RoutableAccounts(ctx context.Context, channelID int) ([]*ent.ChannelAccount, error) {
	byChannel, err := svc.routableAccountsByChannel(ctx, []int{channelID})
	if err != nil {
		return nil, err
	}

	return byChannel[channelID], nil
}

// PrimaryAccount returns the single grant a channel should present to its
// transformer, or nil when the channel has no usable account.
//
// Until account-level selection exists this is the only projection the runtime
// needs: a channel has one account, and that account's credential replaces the
// inline one. Returning nil means "fall back to the channel's own credential",
// which is what keeps channels created before accounts existed working.
func (svc *ChannelAccountService) PrimaryAccount(ctx context.Context, channelID int) (*ent.ChannelAccount, error) {
	accounts, err := svc.RoutableAccounts(ctx, channelID)
	if err != nil {
		return nil, err
	}

	if len(accounts) == 0 {
		return nil, nil
	}

	return accounts[0], nil
}

// TouchChannel advances a channel's updated_at so its cached snapshot is
// rebuilt.
//
// The channel cache reloads only when a channel row moves, and accounts live in
// their own table, so any account mutation has to bump the channel or the
// runtime keeps serving the previous set of accounts.
func (svc *ChannelAccountService) TouchChannel(ctx context.Context, channelID int) error {
	if _, err := svc.entFromContext(ctx).Channel.UpdateOneID(channelID).
		SetUpdatedAt(xtime.UTCNow()).
		Save(ctx); err != nil {
		return fmt.Errorf("touch channel %d after account change: %w", channelID, err)
	}

	return nil
}
