package oauthcallback

import (
	"context"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/llm/transformer/antigravity"
	"github.com/looplj/axonhub/llm/transformer/anthropic/claudecode"
	"github.com/looplj/axonhub/llm/transformer/openai/codex"
	"github.com/looplj/axonhub/llm/transformer/xai/subscription"
)

// Provider identifiers exposed to the console. They match the console's
// provider key so the frontend can look up its callback listener directly.
const (
	ProviderCodex       = "codex"
	ProviderClaudeCode  = "claudecode"
	ProviderAntigravity = "antigravity"
	ProviderXAI         = "xai"
)

// DefaultProviders returns the built-in subscription providers whose callback
// addresses are fixed by the upstream authorization clients.
//
// A provider whose redirect URI is missing or is not a loopback HTTP address is
// skipped: the console still works, it just falls back to pasting the callback
// URL by hand.
func DefaultProviders() []Provider {
	ctx := context.Background()

	candidates := []struct {
		id          string
		redirectURI string
	}{
		{ProviderCodex, codex.RedirectURI},
		{ProviderClaudeCode, claudecode.RedirectURI},
		{ProviderAntigravity, antigravity.RedirectURI},
		{ProviderXAI, subscription.RedirectURI},
	}

	providers := make([]Provider, 0, len(candidates))

	for _, candidate := range candidates {
		p, err := ParseProvider(candidate.id, candidate.redirectURI)
		if err != nil {
			log.Warn(ctx, "skipping oauth callback provider with unusable redirect uri",
				log.String("provider", candidate.id),
				log.String("redirect_uri", candidate.redirectURI),
				log.Cause(err),
			)

			continue
		}

		providers = append(providers, p)
	}

	return providers
}
