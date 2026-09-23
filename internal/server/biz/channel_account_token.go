package biz

import (
	"context"
	"fmt"
	"hash/fnv"
	"strconv"

	lru "github.com/hashicorp/golang-lru/v2"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/oauth"
)
// channelAccountTokenGetter is the TokenGetter a subscription channel's
// transformer is built with.
//
// It answers "which account serves this request?" per call, rather than being
// bound to one account when the channel is built. That mirrors how API-key
// channels already work here: the key is chosen per request from a snapshot
// cached on the channel, and the choice travels with the request, so a channel
// holding several credentials needs one transformer, not one per credential.
//
// Selection follows the same shape as the API-key provider so operators get
// consistent behaviour: sticky to the trace when a trace is present, weighted
// otherwise. With a single account -- which is every migrated channel -- both
// paths return that account, so behaviour is unchanged.
type channelAccountTokenGetter struct {
	channel *Channel

	// cache remembers traceID -> account ID so that, as long as the previously
	// chosen account is still routable, the same account is returned even when
	// the account set changes (for example when a new account is added).
	cache *lru.Cache[string, int]
}

// accountStickyLRUSize bounds the trace-to-account mapping.
const accountStickyLRUSize = 1024

// NewChannelAccountTokenGetter builds the getter for a channel, or returns nil
// when the channel has no accounts.
//
// Returning nil lets the caller keep the pre-account token provider, so a
// channel that has not been migrated to accounts behaves exactly as before.
func NewChannelAccountTokenGetter(ch *Channel) oauth.TokenGetter {
	if ch == nil || len(ch.cachedAccounts) == 0 {
		return nil
	}

	cache, _ := lru.New[string, int](accountStickyLRUSize)

	return &channelAccountTokenGetter{channel: ch, cache: cache}
}

// Get returns the credential of the account chosen for this request.
//
// It satisfies oauth.TokenGetter, which the transformers call with the request
// context, so the choice can be derived from the trace.
func (g *channelAccountTokenGetter) Get(ctx context.Context) (*oauth.OAuthCredentials, error) {
	account := g.selectAccount(ctx)
	if account == nil {
		return nil, fmt.Errorf("channel %s has no routable subscription account", g.channel.Name)
	}

	// Record the choice so downstream bookkeeping (auto-disable, recovery) can
	// address this account instead of the whole channel.
	contexts.WithChannelAPIKey(ctx, accountCredentialRef(account.ID))

	credentials, err := objects.ChannelCredentialsFromAccountCredential(account.Credentials)
	if err != nil {
		return nil, fmt.Errorf("read grant of account %d on channel %s: %w", account.ID, g.channel.Name, err)
	}

	if credentials.OAuth == nil {
		return nil, fmt.Errorf("account %d on channel %s holds no oauth grant", account.ID, g.channel.Name)
	}

	return credentials.OAuth, nil
}

// selectAccount picks the account for this request.
func (g *channelAccountTokenGetter) selectAccount(ctx context.Context) *ent.ChannelAccount {
	accounts := g.channel.cachedAccounts
	if len(accounts) == 0 {
		return nil
	}

	if len(accounts) == 1 {
		return accounts[0]
	}

	trace, ok := contexts.GetTrace(ctx)
	if !ok || trace == nil {
		// No trace to be sticky to. cachedAccounts is ordered most preferred
		// first, so take the head.
		return accounts[0]
	}

	// The trace identity is its database id, which is what the channel-level
	// cache also keys on.
	traceKey := strconv.Itoa(trace.ID)

	if cached, ok := g.cache.Get(traceKey); ok {
		if account := findAccount(accounts, cached); account != nil {
			return account
		}
	}

	selected := rendezvousSelectAccount(accounts, traceKey)
	g.cache.Add(traceKey, selected.ID)

	if log.DebugEnabled(ctx) {
		log.Debug(ctx, "trace sticky account selected",
			log.Int("channel_id", g.channel.ID),
			log.Int("account_id", selected.ID),
			log.Int("trace_id", trace.ID),
		)
	}

	return selected
}

// findAccount returns the account with the given id, or nil when it is no longer
// routable.
func findAccount(accounts []*ent.ChannelAccount, id int) *ent.ChannelAccount {
	for _, account := range accounts {
		if account.ID == id {
			return account
		}
	}

	return nil
}

// rendezvousSelectAccount picks an account using Highest Random Weight hashing,
// which is stable when the account set changes: adding an account only remaps
// the traces that would have chosen it.
func rendezvousSelectAccount(accounts []*ent.ChannelAccount, seed string) *ent.ChannelAccount {
	best := accounts[0]
	bestScore := hashAccountKey(seed, best)

	for _, account := range accounts[1:] {
		if score := hashAccountKey(seed, account); score > bestScore {
			bestScore = score
			best = account
		}
	}

	return best
}

func hashAccountKey(seed string, account *ent.ChannelAccount) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(seed))

	separator := []byte("|")
	_, _ = h.Write(separator)

	_, _ = h.Write([]byte(accountCredentialRef(account.ID)))

	return h.Sum64()
}

// accountCredentialRef is the stable per-account identity used by the
// auto-disable and recovery bookkeeping.
//
// It replaces the single OAuthCredentialRef sentinel that a one-account channel
// used: the sentinel cannot distinguish accounts, so it cannot express "this
// account is failing, keep serving on the others".
func accountCredentialRef(accountID int) string {
	return fmt.Sprintf("account:%d", accountID)
}
