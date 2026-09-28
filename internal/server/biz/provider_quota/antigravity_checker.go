package provider_quota

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/oauth"
	"github.com/looplj/axonhub/llm/transformer/antigravity"
)

// antigravityQuotaURL lists an Antigravity account's models together with a
// per-model remaining fraction.
//
// It serves model discovery only. Its quotaInfo carries no time dimension, so it
// cannot describe the account's rate-limit windows, and reporting it as quota
// produced one row per model.
const antigravityQuotaURL = antigravity.EndpointProd + "/v1internal:fetchAvailableModels"

type AntigravityQuotaChecker struct {
	httpClient *httpclient.HttpClient
}

func NewAntigravityQuotaChecker(httpClient *httpclient.HttpClient) *AntigravityQuotaChecker {
	return &AntigravityQuotaChecker{httpClient: httpClient}
}

func (c *AntigravityQuotaChecker) CheckQuota(ctx context.Context, ch *ent.Channel) (QuotaData, error) {
	httpClient := c.httpClient
	if ch.Settings != nil && ch.Settings.Proxy != nil {
		httpClient = c.httpClient.WithProxy(ch.Settings.Proxy)
	}

	accessToken, projectID, err := c.credentials(ctx, ch, httpClient)
	if err != nil {
		return QuotaData{}, err
	}

	// The summary is the only endpoint that reports this account's rate-limit
	// windows, so it must not be backed by the model list. That list carries a
	// per-model remaining fraction with no time dimension: reporting it as quota
	// swaps the Gemini 5h/7d rows for one row per model, which makes an upstream
	// refusal look like a change in AxonHub.
	//
	// Failing instead leaves the console reporting that the quota is unavailable
	// and why, which is the honest answer while the upstream refuses the call.
	summary, err := c.fetchAntigravityQuotaSummary(ctx, httpClient, accessToken, projectID)
	if err != nil {
		return QuotaData{}, err
	}

	quota, err := parseAntigravityQuotaSummary(summary)
	if err != nil {
		return QuotaData{}, err
	}

	log.Debug(ctx, "reported Antigravity quota windows",
		log.String("channel", ch.Name),
		log.Int("windows", len(quota.Limits)),
	)

	return quota, nil
}

func (c *AntigravityQuotaChecker) SupportsChannel(ch *ent.Channel) bool {
	return ch.Type == channel.TypeAntigravity
}

func (c *AntigravityQuotaChecker) credentials(
	ctx context.Context,
	ch *ent.Channel,
	httpClient *httpclient.HttpClient,
) (string, string, error) {
	legacyParts := strings.SplitN(strings.TrimSpace(ch.Credentials.APIKey), "|", 2)
	refreshToken := ""
	projectID := ""
	if len(legacyParts) > 0 {
		refreshToken = legacyParts[0]
	}
	if len(legacyParts) == 2 {
		projectID = legacyParts[1]
	}

	credentials := ch.Credentials.OAuth
	if credentials == nil {
		credentials = &oauth.OAuthCredentials{}
	} else if credentials.AccessToken != "" &&
		(!credentials.IsExpired(time.Now()) || (credentials.RefreshToken == "" && refreshToken == "")) {
		return credentials.AccessToken, projectID, nil
	}

	credentialsCopy := *credentials
	if credentialsCopy.RefreshToken == "" {
		credentialsCopy.RefreshToken = refreshToken
	}
	if credentialsCopy.ClientID == "" {
		credentialsCopy.ClientID = antigravity.ClientID
	}
	if len(credentialsCopy.Scopes) == 0 {
		credentialsCopy.Scopes = antigravity.Scopes
	}
	if credentialsCopy.RefreshToken == "" {
		return "", "", fmt.Errorf("channel has no Antigravity OAuth credentials")
	}

	tokenProvider := antigravity.NewTokenProvider(oauth.TokenProviderParams{
		Credentials: &credentialsCopy,
		HTTPClient:  httpClient,
	})
	refreshed, err := tokenProvider.Get(ctx)
	if err != nil {
		return "", "", fmt.Errorf("refresh Antigravity OAuth token: %w", err)
	}

	return refreshed.AccessToken, projectID, nil
}

// antigravityQuotaStatus classifies a usage ratio the same way every other
// provider does, so Antigravity limits map onto the shared thresholds.
func antigravityQuotaStatus(usageRatio float64) string {
	if usageRatio >= 1 {
		return "exhausted"
	}
	if usageRatio >= WarningThresholdRatio {
		return "warning"
	}

	return "available"
}
