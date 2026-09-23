package biz

import (
	"context"
	"fmt"
	"hash/fnv"
	"strconv"
	"sync"

	lru "github.com/hashicorp/golang-lru/v2"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/oauth"
)

// accountStickyLRUSize bounds the trace-to-account mapping.
const accountStickyLRUSize = 1024

// ChannelAccountTokenGetter answers "which account serves this request?" per
// call, and hands the transformers that account's live credential.
//
// It replaces binding a channel to one grant at build time. A channel holding
// several accounts therefore needs one transformer, not one per account, which
// mirrors how API keys already work here and how gpt-load models credentials.
//
// Each account gets its own oauth.TokenProvider, so refresh, in-flight
// de-duplication and automatic refresh are per account: one account failing to
// refresh cannot disturb the others. A refreshed token is written back to that
// account's row only, never to the channel, which is what keeps the accounts
// independent.
type ChannelAccountTokenGetter struct {
	channel     *Channel
	persister   accountTokenPersister
	tokenRefreshed func(ctx context.Context, persisted *ent.ChannelAccount) error

	// cache remembers traceID -> account ID so a conversation stays on one
	// account while that account remains routable.
	cache *lru.Cache[string, int]

	mu        sync.Mutex
	providers map[int]*oauth.TokenProvider
}

// accountTokenPersister writes a refreshed credential back to its account row.
type accountTokenPersister interface {
	PersistAccountCredential(ctx context.Context, accountID int, refreshed *oauth.OAuthCredentials) error
}

// AccountTokenGetterParams configures the getter.
type AccountTokenGetterParams struct {
	// Channel is the loaded channel snapshot holding the routable accounts.
	Channel *Channel

	// HTTPClient is the channel's proxy-aware client, used for token refresh.
	HTTPClient *httpclient.HttpClient

	// Persister writes refreshed credentials back to the account row.
	Persister accountTokenPersister

	// OnRefreshed, when set, is notified after a credential is refreshed and
	// persisted, for provider-specific bookkeeping.
	OnRefreshed func(ctx context.Context, account *ent.ChannelAccount) error
}

// NewChannelAccountTokenGetter builds a getter for a channel holding accounts.
//
// It returns nil when the channel has no accounts, so the caller can keep the
// pre-account single-grant provider and a channel that predates accounts behaves
// exactly as before.
func NewChannelAccountTokenGetter(params AccountTokenGetterParams) oauth.TokenGetter {
	ch := params.Channel
	if ch == nil || len(ch.cachedAccounts) == 0 || params.Persister == nil {
		return nil
	}

	cache, _ := lru.New[string, int](accountStickyLRUSize)

	return &ChannelAccountTokenGetter{
		channel:        ch,
		persister:      params.Persister,
		tokenRefreshed: params.OnRefreshed,
		cache:          cache,
		providers:      make(map[int]*oauth.TokenProvider, len(ch.cachedAccounts)),
	}
}

// Get returns the credential of the account chosen for this request, refreshed
// if needed.
//
// It satisfies oauth.TokenGetter, which the transformers call with the request
// context, so the choice can be derived from the trace.
func (g *ChannelAccountTokenGetter) Get(ctx context.Context) (*oauth.OAuthCredentials, error) {
	account := g.selectAccount(ctx)
	if account == nil {
		return nil, fmt.Errorf("channel %s has no routable subscription account", g.channel.Name)
	}

	provider, err := g.providerFor(account)
	if err != nil {
		return nil, err
	}

	// The provider refreshes when the stored credential is near expiry and
	// de-duplicates concurrent refreshes, so a burst of requests on a cold
	// account results in one token exchange.
	creds, err := provider.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("get credential for account %d on channel %s: %w", account.ID, g.channel.Name, err)
	}

	// Record the choice so auto-disable, recovery and usage attribution can
	// address this account instead of the whole channel.
	contexts.WithChannelAPIKey(ctx, accountCredentialRef(account.ID))

	return creds, nil
}

// providerFor returns the account's token provider, building it on first use.
//
// Providers are created lazily so a channel with many accounts only pays for
// the ones that actually serve traffic.
func (g *ChannelAccountTokenGetter) providerFor(account *ent.ChannelAccount) (*oauth.TokenProvider, error) {
	g.mu.Lock()
	if provider, ok := g.providers[account.ID]; ok {
		g.mu.Unlock()

		return provider, nil
	}
	g.mu.Unlock()

	projected, err := objects.ChannelCredentialsFromAccountCredential(account.Credentials)
	if err != nil {
		return nil, fmt.Errorf("read grant of account %d on channel %s: %w", account.ID, g.channel.Name, err)
	}

	if projected.OAuth == nil {
		return nil, fmt.Errorf("account %d on channel %s holds no oauth grant", account.ID, g.channel.Name)
	}

	provider := g.channel.newAccountTokenProvider(projected.OAuth, account, g.persister, g.tokenRefreshed)

	g.mu.Lock()
	// Another request may have built it while we were outside the lock; keep the
	// first one so its in-memory credential stays the single source of truth.
	if existing, ok := g.providers[account.ID]; ok {
		g.mu.Unlock()

		return existing, nil
	}

	g.providers[account.ID] = provider
	g.mu.Unlock()

	return provider, nil
}

// StartAutoRefresh starts background refresh for every account with a provider
// already in use. Called when the channel snapshot becomes active.
func (g *ChannelAccountTokenGetter) StartAutoRefresh(ctx context.Context) {
	g.mu.Lock()
	providers := make([]*oauth.TokenProvider, 0, len(g.providers))
	for _, provider := range g.providers {
		providers = append(providers, provider)
	}
	g.mu.Unlock()

	for _, provider := range providers {
		provider.StartAutoRefresh(ctx, oauth.AutoRefreshOptions{})
	}
}

// StopAutoRefresh stops background refresh for every account.
func (g *ChannelAccountTokenGetter) StopAutoRefresh() {
	g.mu.Lock()
	providers := make([]*oauth.TokenProvider, 0, len(g.providers))
	for _, provider := range g.providers {
		providers = append(providers, provider)
	}
	g.mu.Unlock()

	for _, provider := range providers {
		provider.StopAutoRefresh()
	}
}

// selectAccount picks the account for this request.
//
// With one account -- every migrated channel -- it returns that account, so
// behaviour is unchanged. With several it is sticky to the trace and weighted
// otherwise, matching the API-key provider so operators see one behaviour.
func (g *ChannelAccountTokenGetter) selectAccount(ctx context.Context) *ent.ChannelAccount {
	accounts := g.channel.cachedAccounts
	if len(accounts) == 0 {
		return nil
	}

	if len(accounts) == 1 {
		return accounts[0]
	}

	trace, ok := contexts.GetTrace(ctx)
	if !ok || trace == nil {
		// Nothing to be sticky to. cachedAccounts is ordered most preferred
		// first, so take the head.
		return accounts[0]
	}

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
	_, _ = h.Write([]byte("|"))
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
	return "account:" + strconv.Itoa(accountID)
}
