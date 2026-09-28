package provider_quota

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/antigravity"
)

func TestAntigravityQuotaChecker_CheckQuota(t *testing.T) {
	resetAt := "2099-09-04T08:00:00Z"

	httpClient := httpclient.NewHttpClientWithClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		require.Equal(t, antigravityQuotaSummaryURL, request.URL.String())
		require.Equal(t, "Bearer access-token", request.Header.Get("Authorization"))
		require.Equal(t, "antigravity", request.Header.Get("X-Client-Name"))
		require.Equal(t, antigravity.GetVersion(), request.Header.Get("X-Client-Version"))

		var requestBody map[string]any
		require.NoError(t, json.NewDecoder(request.Body).Decode(&requestBody))
		require.Equal(t, "project-id", requestBody["project"])

		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{
				"groups": [{
					"displayName": "Gemini Models",
					"buckets": [
						{"bucketId":"gemini-5h","window":"5h","remainingFraction":0.75,"resetTime":"` + resetAt + `"},
						{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.1,"resetTime":"` + resetAt + `"}
					]
				}]
			}`)),
		}, nil
	})})

	checker := NewAntigravityQuotaChecker(httpClient)
	channelEntity := &ent.Channel{
		Type: channel.TypeAntigravity,
		Credentials: objects.ChannelCredentials{
			APIKey: "refresh-token|project-id",
			OAuth: &objects.OAuthCredentials{
				AccessToken: "access-token",
				ExpiresAt:   time.Now().Add(time.Hour),
			},
		},
	}

	quota, err := checker.CheckQuota(t.Context(), channelEntity)

	require.NoError(t, err)
	// The 0.1-remaining weekly window is the tightest, so it drives the status.
	require.Equal(t, "warning", quota.Status)
	require.Equal(t, "antigravity", quota.ProviderType)
	require.True(t, quota.Ready)
	require.Len(t, quota.Limits, 2)
	require.Len(t, quota.RawData["windows"], 2)

	windows := map[string]QuotaLimitStatus{}
	for _, limit := range quota.Limits {
		windows[limit.Window] = limit
	}

	require.Contains(t, windows, QuotaWindow5h)
	require.Contains(t, windows, QuotaWindow7d)
	require.InDelta(t, 0.25, windows[QuotaWindow5h].UsageRatio, 1e-9)
	require.InDelta(t, 0.9, windows[QuotaWindow7d].UsageRatio, 1e-9)
	require.Equal(t, "Gemini", windows[QuotaWindow5h].Group)

	require.Equal(t, time.Date(2099, 9, 4, 8, 0, 0, 0, time.UTC), *quota.NextResetAt)
	require.True(t, checker.SupportsChannel(channelEntity))
	require.False(t, checker.SupportsChannel(&ent.Channel{Type: channel.TypeGemini}))
}

// When the summary endpoint is unavailable the checker must still report
// something, so it falls back to the model list.
// A refused summary must not be replaced by the model list. That list has no
// time dimension, so serving it as quota swapped the Gemini 5h/7d rows for one
// row per model, and an upstream refusal became indistinguishable from a change
// in AxonHub. Failing instead is what lets the console report the real reason.
func TestAntigravityQuotaChecker_FailsWhenTheSummaryIsRefused(t *testing.T) {
	var requested []string

	httpClient := httpclient.NewHttpClientWithClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requested = append(requested, request.URL.String())

		return &http.Response{
			StatusCode: http.StatusForbidden,
			Status:     "403 Forbidden",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(antigravityRefusalBody)),
		}, nil
	})})

	checker := NewAntigravityQuotaChecker(httpClient)

	_, err := checker.CheckQuota(t.Context(), &ent.Channel{
		Name: "Antigravity",
		Type: channel.TypeAntigravity,
		Credentials: objects.ChannelCredentials{
			APIKey: "refresh-token|project-id",
			OAuth: &objects.OAuthCredentials{
				AccessToken: "access-token",
				ExpiresAt:   time.Now().Add(time.Hour),
			},
		},
	})

	require.Error(t, err)
	// The quota source is the only endpoint touched; the model list is not a
	// fallback for quota.
	require.Equal(t, []string{antigravityQuotaSummaryURL}, requested)
	require.Contains(t, err.Error(), "VALIDATION_REQUIRED")
}

func TestAntigravityQuotaChecker_CheckQuotaRefreshesLegacyCredentials(t *testing.T) {
	httpClient := httpclient.NewHttpClientWithClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := `{"groups":[{"displayName":"Gemini Models","buckets":[
			{"bucketId":"gemini-5h","window":"5h","remainingFraction":0.5}]}]}`

		if request.URL.String() == antigravity.TokenURL {
			require.NoError(t, request.ParseForm())
			require.Equal(t, "refresh_token", request.Form.Get("grant_type"))
			require.Equal(t, "refresh-token", request.Form.Get("refresh_token"))
			body = `{"access_token":"fresh-access-token","expires_in":3600,"token_type":"Bearer"}`
		} else {
			require.Equal(t, antigravityQuotaSummaryURL, request.URL.String())
			require.Equal(t, "Bearer fresh-access-token", request.Header.Get("Authorization"))
		}

		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})})
	checker := NewAntigravityQuotaChecker(httpClient)

	quota, err := checker.CheckQuota(t.Context(), &ent.Channel{
		Type:        channel.TypeAntigravity,
		Credentials: objects.ChannelCredentials{APIKey: "refresh-token|project-id"},
	})

	require.NoError(t, err)
	require.Equal(t, "available", quota.Status)
}
