package biz

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent/channel"
)

// TestTryReturnDefaultModels_SkipsAntigravity pins the regression that made
// upstream model discovery silently do nothing.
//
// tryReturnDefaultModels runs before the channel-aware path in FetchModels. When
// it answers, FetchModels returns immediately, so any discovery further down is
// unreachable. Antigravity was in that position: the discovery was implemented,
// compiled and tested, yet never executed because this function answered first.
//
// A test cannot observe an unreachable branch, which is why this pins the gate
// itself.
func TestTryReturnDefaultModels_SkipsAntigravity(t *testing.T) {
	t.Parallel()

	fetcher := &ModelFetcher{}

	_, ok := fetcher.tryReturnDefaultModels(t.Context(), channel.TypeAntigravity.String())
	require.False(t, ok,
		"antigravity must not be answered from the compiled list, or upstream discovery is unreachable")
}

// Same gate for xAI: it now has a discovery path, so the compiled-list short
// circuit must not answer first and hide it.
func TestTryReturnDefaultModels_SkipsXaiSubscription(t *testing.T) {
	t.Parallel()

	fetcher := &ModelFetcher{}

	_, ok := fetcher.tryReturnDefaultModels(t.Context(), channel.TypeXaiSubscription.String())
	require.False(t, ok,
		"xai_subscription must not be answered from the compiled list, or upstream discovery is unreachable")
}

// TestIsOfficialOnlyType_GatesEveryTypeWithADiscoveryPath records which types are
// deliberately kept out of the compiled-list short circuit.
//
// Membership means "report defaults only once the channel is confirmed
// official", which is the precondition for asking the provider instead. A type
// that has a discovery path but is missing here will quietly never use it.
func TestIsOfficialOnlyType_GatesEveryTypeWithADiscoveryPath(t *testing.T) {
	t.Parallel()

	// Every subscription type: all of them authenticate as an account, so all of
	// them are better served by asking the provider than by a compiled list.
	for _, typ := range []channel.Type{
		channel.TypeClaudecode,
		channel.TypeCodex,
		channel.TypeXaiSubscription,
		channel.TypeAntigravity,
	} {
		require.True(t, isOfficialOnlyType(typ),
			"type %s must be gated so its channel-aware path can run", typ)
	}

	// A plain API-key type has no account to ask, so it keeps the short circuit.
	require.False(t, isOfficialOnlyType(channel.TypeOpenai))
	require.False(t, isOfficialOnlyType(channel.TypeAnthropic))
}
