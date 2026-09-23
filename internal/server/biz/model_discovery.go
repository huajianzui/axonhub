package biz

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/oauth"
	"github.com/looplj/axonhub/llm/transformer/antigravity"
)

// This file discovers a subscription channel's models from the provider itself,
// instead of reporting the static list compiled into the binary.
//
// The static list cannot be complete or current: which models an account may use
// depends on the account's plan and region, and providers add and retire models
// without a client release. The provider already answers "what may this account
// use?" through the same endpoint the quota checker calls, so the list is
// available for free -- AxonHub was parsing that response for quota and
// discarding the model IDs it contained.

// antigravityModelsURL lists the models an Antigravity account may use.
//
// It is the same endpoint the quota checker reads, which is why quota and
// availability never disagree.
const antigravityModelsURL = antigravity.EndpointProd + "/v1internal:fetchAvailableModels"

// antigravityAvailableModels mirrors the response of fetchAvailableModels. Only
// the fields needed to publish a model are read; quotaInfo is ignored here
// because the quota checker already consumes it.
type antigravityAvailableModels struct {
	Models map[string]struct {
		DisplayName string `json:"displayName"`
		IsInternal  bool   `json:"isInternal"`
		MaxTokens   int64  `json:"maxTokens"`
		MaxOutput   int64  `json:"maxOutputTokens"`
	} `json:"models"`
}

// fetchAntigravityUpstreamModels asks the provider which models this channel's
// account may use.
//
// A failure is returned so the caller can decide: a channel whose account is
// temporarily unreachable should keep whatever list it already had rather than
// being emptied, because an empty model list takes the channel out of service.
func (f *ModelFetcher) fetchAntigravityUpstreamModels(ctx context.Context, ch *ent.Channel) ([]ModelIdentify, error) {
	httpClient := f.httpClientForChannel(ch)

	accessToken, projectID, err := antigravityAccessToken(ctx, ch, httpClient)
	if err != nil {
		return nil, err
	}

	body := map[string]any{}
	if projectID != "" {
		body["project"] = projectID
	}

	request := httpclient.NewRequestBuilder().
		WithMethod(http.MethodPost).
		WithURL(antigravityModelsURL).
		WithBearerToken(accessToken).
		WithHeader("Content-Type", "application/json").
		WithHeader("User-Agent", antigravity.GetUserAgent()).
		WithHeader("X-Client-Name", "antigravity").
		WithHeader("X-Client-Version", antigravity.GetVersion()).
		WithBody(body).
		Build()

	response, err := httpClient.Do(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("fetch Antigravity models: %w", err)
	}

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch Antigravity models: upstream status %d", response.StatusCode)
	}

	var parsed antigravityAvailableModels
	if err := json.Unmarshal(response.Body, &parsed); err != nil {
		return nil, fmt.Errorf("decode Antigravity models response: %w", err)
	}

	models := make([]ModelIdentify, 0, len(parsed.Models))
	for id, model := range parsed.Models {
		// Internal entries are implementation details the account cannot select.
		if model.IsInternal || strings.TrimSpace(id) == "" {
			continue
		}

		models = append(models, ModelIdentify{ID: id})
	}

	if len(models) == 0 {
		return nil, fmt.Errorf("Antigravity account exposes no models")
	}

	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })

	return models, nil
}

// mergeDiscoveredModels combines the provider's list with the compiled one.
//
// Upstream wins on order because it reflects this account's plan, while any
// model only the binary knows about is appended: a model that upstream omits
// today may simply be gated for this plan, and dropping it would remove a model
// the account could still be granted later.
func mergeDiscoveredModels(discovered, fallback []ModelIdentify) []ModelIdentify {
	if len(discovered) == 0 {
		return fallback
	}

	seen := make(map[string]struct{}, len(discovered)+len(fallback))
	merged := make([]ModelIdentify, 0, len(discovered)+len(fallback))

	for _, model := range discovered {
		if _, ok := seen[model.ID]; ok {
			continue
		}

		seen[model.ID] = struct{}{}
		merged = append(merged, model)
	}

	for _, model := range fallback {
		if _, ok := seen[model.ID]; ok {
			continue
		}

		seen[model.ID] = struct{}{}
		merged = append(merged, model)
	}

	return merged
}

// httpClientForChannel returns the client that should carry the request,
// honouring the channel's proxy so discovery works from the same network
// position as inference.
func (f *ModelFetcher) httpClientForChannel(ch *ent.Channel) *httpclient.HttpClient {
	if ch.Settings != nil && ch.Settings.Proxy != nil {
		return f.httpClient.WithProxy(ch.Settings.Proxy)
	}

	return f.httpClient
}

// antigravityAccessToken resolves a usable access token for the channel,
// refreshing when the stored one has expired.
//
// Antigravity keeps its grant in two places depending on age: "<refreshToken>|
// <projectID>" in the legacy field, and an OAuth object. Both are accepted so a
// channel works whether or not it has been migrated.
func antigravityAccessToken(ctx context.Context, ch *ent.Channel, httpClient *httpclient.HttpClient) (string, string, error) {
	legacyParts := strings.SplitN(strings.TrimSpace(ch.Credentials.APIKey), "|", 2)

	refreshToken := ""
	projectID := ""
	if len(legacyParts) > 0 {
		refreshToken = strings.TrimSpace(legacyParts[0])
	}

	if len(legacyParts) == 2 {
		projectID = strings.TrimSpace(legacyParts[1])
	}

	credentials := ch.Credentials.OAuth
	if credentials == nil {
		credentials = &oauth.OAuthCredentials{}
	} else if credentials.AccessToken != "" &&
		(!credentials.IsExpired(time.Now()) || (credentials.RefreshToken == "" && refreshToken == "")) {
		return credentials.AccessToken, projectID, nil
	}

	refreshing := *credentials
	if refreshing.RefreshToken == "" {
		refreshing.RefreshToken = refreshToken
	}

	if refreshing.ClientID == "" {
		refreshing.ClientID = antigravity.ClientID
	}

	if len(refreshing.Scopes) == 0 {
		refreshing.Scopes = antigravity.Scopes
	}

	if refreshing.RefreshToken == "" {
		return "", "", fmt.Errorf("antigravity channel %s has no usable credential", ch.Name)
	}

	tokenProvider := antigravity.NewTokenProvider(oauth.TokenProviderParams{
		Credentials: &refreshing,
		HTTPClient:  httpClient,
	})

	refreshed, err := tokenProvider.Get(ctx)
	if err != nil {
		return "", "", fmt.Errorf("refresh Antigravity token for channel %s: %w", ch.Name, err)
	}

	return refreshed.AccessToken, projectID, nil
}

// discoverOrFallbackModels asks upstream for the model list and falls back to
// the compiled list when upstream cannot be reached.
//
// Falling back rather than failing is deliberate: sync runs on a schedule, and a
// transient upstream error must not empty a working channel.
func (f *ModelFetcher) discoverOrFallbackModels(
	ctx context.Context,
	ch *ent.Channel,
	fallback []ModelIdentify,
) []ModelIdentify {
	discovered, err := f.fetchAntigravityUpstreamModels(ctx, ch)
	if err != nil {
		log.Warn(ctx, "falling back to the compiled model list",
			log.String("channel", ch.Name),
			log.String("type", ch.Type.String()),
			log.Cause(err),
		)

		return fallback
	}

	log.Info(ctx, "discovered models from upstream",
		log.String("channel", ch.Name),
		log.Int("upstream_count", len(discovered)),
		log.Int("compiled_count", len(fallback)),
	)

	return mergeDiscoveredModels(discovered, fallback)
}

// supportsUpstreamModelDiscovery reports whether the fetcher can ask this
// channel's provider for its model list.
func supportsUpstreamModelDiscovery(typ channel.Type) bool {
	return typ == channel.TypeAntigravity
}
