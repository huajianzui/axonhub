package provider_quota

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// antigravitySummaryBody is the response captured from a live
// v1internal:retrieveUserQuotaSummary call. Both families publish the same two
// windows, which is what makes the grouping necessary.
const antigravitySummaryBody = `{
  "groups": [
    {
      "buckets": [
        {
          "bucketId": "gemini-weekly",
          "displayName": "Weekly Limit Remaining",
          "window": "weekly",
          "resetTime": "2026-09-30T07:50:22Z",
          "remainingFraction": 0.99997574
        },
        {
          "bucketId": "gemini-5h",
          "displayName": "Five Hour Limit Remaining",
          "window": "5h",
          "resetTime": "2026-09-24T10:33:24Z",
          "remainingFraction": 1
        }
      ],
      "displayName": "Gemini Models",
      "description": "Models within this group: Gemini Flash, Gemini Pro"
    },
    {
      "buckets": [
        {
          "bucketId": "3p-weekly",
          "displayName": "Weekly Limit Remaining",
          "window": "weekly",
          "resetTime": "2026-10-01T05:33:24Z",
          "remainingFraction": 0.5
        },
        {
          "bucketId": "3p-5h",
          "displayName": "Five Hour Limit Remaining",
          "window": "5h",
          "resetTime": "2026-09-24T10:33:24Z",
          "remainingFraction": 1
        }
      ],
      "displayName": "Claude and GPT models",
      "description": "Models within this group: Claude Opus, Claude Sonnet, GPT-OSS"
    }
  ]
}`

func parseSummaryForTest(t *testing.T, body string) QuotaData {
	t.Helper()

	summary, err := decodeAntigravityQuotaSummary([]byte(body))
	require.NoError(t, err)

	quota, err := parseAntigravityQuotaSummary(summary)
	require.NoError(t, err)

	return quota
}

// TestParseAntigravityQuotaSummary_KeepsBothFamilies is the regression guard for
// the reported defect: the model-list endpoint produced one row per model with no
// time windows. The summary endpoint must yield the four windows, and both
// families must survive normalization even though they share window names.
func TestParseAntigravityQuotaSummary_KeepsBothFamilies(t *testing.T) {
	quota := parseSummaryForTest(t, antigravitySummaryBody)

	require.Len(t, quota.Limits, 4, "both families publish a 5h and a weekly window")

	type key struct {
		group  string
		window string
	}

	got := make(map[key]float64, 4)
	for _, limit := range quota.Limits {
		require.Equal(t, QuotaLimitTypeToken, limit.Type)
		got[key{limit.Group, limit.Window}] = limit.UsageRatio
	}

	// Gemini is nearly untouched; Claude/GPT has burned half its weekly window.
	require.InDelta(t, 1-0.99997574, got[key{"Gemini", QuotaWindow7d}], 1e-6)
	require.InDelta(t, 0, got[key{"Gemini", QuotaWindow5h}], 1e-9)
	require.InDelta(t, 0.5, got[key{"Claude/GPT", QuotaWindow7d}], 1e-9)
	require.InDelta(t, 0, got[key{"Claude/GPT", QuotaWindow5h}], 1e-9)
}

func TestParseAntigravityQuotaSummary_WindowsAreNormalized(t *testing.T) {
	quota := parseSummaryForTest(t, antigravitySummaryBody)

	windows := make([]string, 0, len(quota.Limits))
	for _, limit := range quota.Limits {
		windows = append(windows, limit.Window)
	}

	// The console only labels the normalized identifiers, so "weekly" must not
	// reach the limit as-is.
	require.Contains(t, windows, QuotaWindow7d)
	require.Contains(t, windows, QuotaWindow5h)
	require.NotContains(t, windows, "weekly")
}

// The headline status has to follow the tightest window; a nearly full Gemini
// window must not mask an exhausted Claude/GPT one.
func TestParseAntigravityQuotaSummary_StatusFollowsWorstWindow(t *testing.T) {
	body := `{"groups":[{"displayName":"Gemini Models","buckets":[
	  {"bucketId":"gemini-5h","window":"5h","remainingFraction":1}]},
	  {"displayName":"Claude and GPT models","buckets":[
	  {"bucketId":"3p-5h","window":"5h","remainingFraction":0}]}]}`

	quota := parseSummaryForTest(t, body)

	require.Equal(t, "exhausted", quota.Status)
	require.False(t, quota.Ready)
}

func TestParseAntigravityQuotaSummary_ResetTimeIsParsed(t *testing.T) {
	quota := parseSummaryForTest(t, antigravitySummaryBody)

	require.NotNil(t, quota.NextResetAt)
	require.True(t, quota.NextResetAt.Before(time.Now().Add(30*24*time.Hour)),
		"the nearest reset is in the near future, not a zero time")
}

func TestParseAntigravityQuotaSummary_RejectsEmptySummary(t *testing.T) {
	_, err := decodeAntigravityQuotaSummary([]byte(`{"groups":[]}`))
	require.Error(t, err)

	// A group with no usable bucket cannot produce limits.
	_, err = parseAntigravityQuotaSummary(&antigravityQuotaSummary{
		Groups: []antigravityQuotaGroup{{DisplayName: "Gemini Models"}},
	})
	require.Error(t, err)
}

func TestAntigravityQuotaWindow(t *testing.T) {
	for _, tc := range []struct {
		window   string
		bucketID string
		want     string
	}{
		{"weekly", "gemini-weekly", QuotaWindow7d},
		{"5h", "gemini-5h", QuotaWindow5h},
		{"", "unknown-bucket", "unknown-bucket"},
		{"6h", "x", "6h"},
	} {
		t.Run(tc.window, func(t *testing.T) {
			require.Equal(t, tc.want, antigravityQuotaWindow(tc.window, tc.bucketID))
		})
	}
}

// A provider that does not report groups must keep merging duplicate windows, so
// adding the group to the identity cannot regress the other channels.
func TestNormalizeQuotaLimits_MergesWindowsWithoutGroups(t *testing.T) {
	limits := []QuotaLimitStatus{
		{
			Type: QuotaLimitTypeToken, Window: QuotaWindow5h,
			Status: "available", UsageRatio: 0.2,
		},
		{
			Type: QuotaLimitTypeToken, Window: QuotaWindow5h,
			Status: "exhausted", UsageRatio: 1,
		},
	}

	normalized := normalizeQuotaLimits(limits, time.Now())
	require.Len(t, normalized, 1, "ungrouped duplicates still merge")
	require.Equal(t, "exhausted", normalized[0].Status)
}

func TestNormalizeQuotaLimits_KeepsGroupsApart(t *testing.T) {
	limits := []QuotaLimitStatus{
		{
			Type: QuotaLimitTypeToken, Window: QuotaWindow5h, Group: "Gemini",
			Status: "available", UsageRatio: 0.2,
		},
		{
			Type: QuotaLimitTypeToken, Window: QuotaWindow5h, Group: "Claude/GPT",
			Status: "exhausted", UsageRatio: 1,
		},
	}

	normalized := normalizeQuotaLimits(limits, time.Now())
	require.Len(t, normalized, 2, "different groups are different limits")
}
