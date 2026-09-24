package provider_quota

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/antigravity"
)

// antigravityQuotaSummaryURL reports the account's rate limits as a small set of
// time windows per model family.
//
// This is a different endpoint from the one that lists models
// (v1internal:fetchAvailableModels). That endpoint answers "which models may
// this account use?" and carries only a per-model remaining fraction with no
// time dimension, so rendering it produced one row per model. This one answers
// "how much of each rate limit is left?", which is what the console shows for
// every other subscription channel.
//
// The response looks like:
//
//	groups: [
//	  { displayName: "Gemini Models", buckets: [
//	      { bucketId: "gemini-weekly", window: "weekly", remainingFraction: 0.99 },
//	      { bucketId: "gemini-5h",     window: "5h",     remainingFraction: 1 } ] },
//	  { displayName: "Claude and GPT models", buckets: [ ... ] } ]
//
// Both families publish the same two windows, so the group name is what keeps
// them apart once normalized.
const antigravityQuotaSummaryURL = antigravity.EndpointProd + "/v1internal:retrieveUserQuotaSummary"

// antigravityQuotaSummary mirrors retrieveUserQuotaSummary. Only the fields used
// for display are read.
type antigravityQuotaSummary struct {
	Groups []antigravityQuotaGroup `json:"groups"`
}

type antigravityQuotaGroup struct {
	DisplayName string                   `json:"displayName"`
	Buckets     []antigravityQuotaBucket `json:"buckets"`
}

type antigravityQuotaBucket struct {
	BucketID          string   `json:"bucketId"`
	DisplayName       string   `json:"displayName"`
	Window            string   `json:"window"`
	ResetTime         string   `json:"resetTime"`
	RemainingFraction *float64 `json:"remainingFraction"`
}

// fetchAntigravityQuotaSummary asks the provider for the account's rate limits.
func (c *AntigravityQuotaChecker) fetchAntigravityQuotaSummary(
	ctx context.Context,
	httpClient *httpclient.HttpClient,
	accessToken string,
	projectID string,
) (*antigravityQuotaSummary, error) {
	body := map[string]any{}
	if projectID != "" {
		body["project"] = projectID
	}

	request := httpclient.NewRequestBuilder().
		WithMethod(http.MethodPost).
		WithURL(antigravityQuotaSummaryURL).
		WithBearerToken(accessToken).
		WithHeader("Content-Type", "application/json").
		WithHeader("User-Agent", antigravity.GetUserAgent()).
		WithHeader("X-Client-Name", "antigravity").
		WithHeader("X-Client-Version", antigravity.GetVersion()).
		WithBody(body).
		Build()

	response, err := httpClient.Do(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("fetch Antigravity quota summary: %w", err)
	}

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch Antigravity quota summary: upstream status %d", response.StatusCode)
	}

	parsed, err := decodeAntigravityQuotaSummary(response.Body)
	if err != nil {
		return nil, err
	}

	return parsed, nil
}

// decodeAntigravityQuotaSummary validates a raw summary body. It is separate from
// the fetch so the parsing rules stay testable without a live endpoint.
func decodeAntigravityQuotaSummary(body []byte) (*antigravityQuotaSummary, error) {
	var parsed antigravityQuotaSummary
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode Antigravity quota summary: %w", err)
	}

	if len(parsed.Groups) == 0 {
		return nil, fmt.Errorf("Antigravity quota summary has no groups")
	}

	return &parsed, nil
}

// parseAntigravityQuotaSummary turns the summary into the provider-neutral quota
// shape the console renders: one windowed limit per family, labelled "5h"/"7d"
// with the family carried separately.
func parseAntigravityQuotaSummary(summary *antigravityQuotaSummary) (QuotaData, error) {
	limits := make([]QuotaLimitStatus, 0, 4)
	raw := make(map[string]any, 4)
	seen := make(map[string]struct{}, 4)

	worstUsage := 0.0
	var nextResetAt *time.Time

	for _, group := range summary.Groups {
		groupName := antigravityQuotaGroupLabel(group.DisplayName)

		for _, bucket := range group.Buckets {
			if bucket.RemainingFraction == nil {
				continue
			}

			if math.IsNaN(*bucket.RemainingFraction) || math.IsInf(*bucket.RemainingFraction, 0) {
				continue
			}

			window := antigravityQuotaWindow(bucket.Window, bucket.BucketID)
			if window == "" {
				continue
			}

			key := groupName + "\x00" + window
			if _, exists := seen[key]; exists {
				continue
			}

			seen[key] = struct{}{}

			remaining := math.Max(0, math.Min(1, *bucket.RemainingFraction))
			usageRatio := 1 - remaining
			status := antigravityQuotaStatus(usageRatio)
			resetAt := antigravityQuotaSummaryResetAt(bucket.ResetTime)

			limit := QuotaLimitStatus{
				Type:        QuotaLimitTypeToken,
				Status:      status,
				UsageRatio:  usageRatio,
				Ready:       IsReadyStatus(status),
				NextResetAt: resetAt,
				Window:      window,
				Group:       groupName,
			}
			limits = append(limits, limit)

			if usageRatio > worstUsage {
				worstUsage = usageRatio
			}

			if resetAt != nil && (nextResetAt == nil || resetAt.Before(*nextResetAt)) {
				nextResetAt = resetAt
			}

			raw[groupName+"\x00"+window] = map[string]any{
				"group":               groupName,
				"window":              window,
				"remainingPercentage": remaining * 100,
				"status":              status,
			}
		}
	}

	if len(limits) == 0 {
		return QuotaData{}, fmt.Errorf("Antigravity quota summary has no usable buckets")
	}

	// Stable presentation: family first, then shortest window.
	sort.SliceStable(limits, func(i, j int) bool {
		if limits[i].Group != limits[j].Group {
			return limits[i].Group < limits[j].Group
		}

		return antigravityQuotaWindowSeconds(limits[i].Window) < antigravityQuotaWindowSeconds(limits[j].Window)
	})

	// The headline status must reflect the tightest window, not the loosest.
	overallStatus := antigravityQuotaStatus(worstUsage)

	return NormalizeQuotaData(QuotaData{
		Status:       overallStatus,
		ProviderType: "antigravity",
		RawData:      map[string]any{"windows": raw},
		NextResetAt:  nextResetAt,
		Ready:        IsReadyStatus(overallStatus),
		Limits:       limits,
	}), nil
}

// antigravityQuotaGroupLabel shortens an upstream group name to the family the
// windows belong to.
func antigravityQuotaGroupLabel(displayName string) string {
	normalized := strings.ToLower(strings.TrimSpace(displayName))

	switch {
	case strings.Contains(normalized, "gemini"):
		return "Gemini"
	case strings.Contains(normalized, "claude") || strings.Contains(normalized, "gpt"):
		return "Claude/GPT"
	case normalized == "":
		return "Antigravity"
	default:
		return strings.TrimSpace(displayName)
	}
}

// antigravityQuotaWindow maps an upstream window name onto the normalized window
// identifiers the console already knows how to label. Unrecognized names fall
// back to the bucket id so an unknown window is still surfaced rather than
// silently dropped.
func antigravityQuotaWindow(window string, bucketID string) string {
	normalized := strings.ToLower(strings.TrimSpace(window))

	switch normalized {
	case "weekly", "7d", "week":
		return QuotaWindow7d
	case "5h", "five_hour", "five-hour":
		return QuotaWindow5h
	case "daily", "24h", "day":
		return QuotaWindowDaily
	case "monthly", "30d":
		return QuotaWindow30d
	case "":
		return strings.TrimSpace(bucketID)
	}

	// A bare duration such as "6h" is already a usable window identifier.
	if seconds := antigravityQuotaWindowSeconds(normalized); seconds > 0 {
		if period := quotaPeriodLabel(seconds); period != "" {
			return period
		}
	}

	return normalized
}

// antigravityQuotaWindowSeconds converts an upstream window name to seconds.
//
// Upstream sends either a bare name ("weekly") or a duration ("5h"), so both
// forms are accepted.
func antigravityQuotaWindowSeconds(window string) int64 {
	normalized := strings.ToLower(strings.TrimSpace(window))

	const (
		hour = int64(3600)
		day  = 24 * hour
	)

	switch normalized {
	case "weekly", "7d", "week":
		return 7 * day
	case "daily", "24h", "day":
		return day
	case "hourly", "1h", "5h", "five_hour", "five-hour":
		if normalized == "5h" || normalized == "five_hour" || normalized == "five-hour" {
			return 5 * hour
		}

		return hour
	case "monthly", "30d":
		return 30 * day
	}

	duration, err := time.ParseDuration(normalized)
	if err != nil || duration <= 0 {
		return 0
	}

	return int64(duration / time.Second)
}

// quotaPeriodLabel renders a duration as the shortest exact unit, mirroring the
// labels the console already shows for time windows: 604800 -> "7d".
func quotaPeriodLabel(seconds int64) string {
	if seconds <= 0 {
		return ""
	}

	const (
		minute = int64(60)
		hour   = 60 * minute
		day    = 24 * hour
	)

	switch {
	case seconds%day == 0:
		return fmt.Sprintf("%dd", seconds/day)
	case seconds%hour == 0:
		return fmt.Sprintf("%dh", seconds/hour)
	case seconds%minute == 0:
		return fmt.Sprintf("%dmin", seconds/minute)
	default:
		return fmt.Sprintf("%ds", seconds)
	}
}

func antigravityQuotaSummaryResetAt(raw string) *time.Time {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return nil
	}

	return &parsed
}
