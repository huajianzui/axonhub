package biz

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMergeDiscoveredModels_UpstreamLeadsAndCompiledFillsGaps is the behaviour
// that makes the model list match the account: what the provider reports comes
// first, and anything only the binary knows about is kept rather than dropped.
func TestMergeDiscoveredModels_UpstreamLeadsAndCompiledFillsGaps(t *testing.T) {
	t.Parallel()

	discovered := []ModelIdentify{
		{ID: "gemini-3-pro-high"},
		{ID: "claude-sonnet-4-5"},
	}
	compiled := []ModelIdentify{
		{ID: "claude-sonnet-4-5"}, // already discovered, must not duplicate
		{ID: "gemini-2.5-flash"},  // only the binary knows it
	}

	merged := mergeDiscoveredModels(discovered, compiled)

	require.Len(t, merged, 3, "duplicates must collapse, gaps must be filled")
	require.Equal(t, "gemini-3-pro-high", merged[0].ID, "upstream models come first")
	require.Equal(t, "claude-sonnet-4-5", merged[1].ID)
	require.Equal(t, "gemini-2.5-flash", merged[2].ID, "compiled-only models are preserved")
}

func TestMergeDiscoveredModels_FallsBackWhenNothingDiscovered(t *testing.T) {
	t.Parallel()

	compiled := []ModelIdentify{{ID: "a"}, {ID: "b"}}

	// An empty discovery means upstream told us nothing usable, so the compiled
	// list must survive intact rather than being replaced with emptiness -- an
	// empty model list takes the channel out of service.
	require.Equal(t, compiled, mergeDiscoveredModels(nil, compiled))
	require.Equal(t, compiled, mergeDiscoveredModels([]ModelIdentify{}, compiled))
}

func TestMergeDiscoveredModels_HandlesNoFallback(t *testing.T) {
	t.Parallel()

	discovered := []ModelIdentify{{ID: "only"}}
	require.Equal(t, discovered, mergeDiscoveredModels(discovered, nil))
}

func TestSupportsUpstreamModelDiscovery_CoversAntigravity(t *testing.T) {
	t.Parallel()

	require.True(t, supportsUpstreamModelDiscovery("antigravity"))

	// Types without a discovery endpoint must keep the compiled path, so the
	// caller never issues a request that cannot work.
	require.False(t, supportsUpstreamModelDiscovery("claudecode"))
	require.False(t, supportsUpstreamModelDiscovery("codex"))
	require.False(t, supportsUpstreamModelDiscovery("openai"))
}
