package aistudio

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestResolveQuotaCooldown_ModelCapacityExhausted(t *testing.T) {
	body := []byte(`{"error":{"code":429,"message":"No capacity available for model gemini-2.5-pro","status":"RESOURCE_EXHAUSTED"}}`)
	decision := ResolveQuotaCooldown(http.StatusTooManyRequests, body)
	require.True(t, decision.ModelOnly)
	require.True(t, decision.CapacityShortage)
	require.WithinDuration(t, time.Now().Add(20*time.Second), decision.ResetAt, 2*time.Second)
}

func TestResolveQuotaCooldown_ResetsIn(t *testing.T) {
	body := []byte(`{"error":{"code":429,"message":"You have exceeded your quota. Resets in 2 hours, 15 minutes."}}`)
	decision := ResolveQuotaCooldown(http.StatusTooManyRequests, body)
	require.False(t, decision.ModelOnly)
	expectedDur := 2*time.Hour + 15*time.Minute
	require.WithinDuration(t, time.Now().Add(expectedDur), decision.ResetAt, 2*time.Second)
}

func TestResolveQuotaCooldown_DailyQuota(t *testing.T) {
	body := []byte(`{"error":{"code":429,"message":"Daily limit reached. Try again tomorrow."}}`)
	decision := ResolveQuotaCooldown(http.StatusTooManyRequests, body)
	require.False(t, decision.ModelOnly)
	require.True(t, decision.IsDailyQuota)
	loc, _ := time.LoadLocation("America/Los_Angeles")
	require.Equal(t, 0, decision.ResetAt.In(loc).Hour())
	require.Equal(t, 0, decision.ResetAt.In(loc).Minute())
}

func TestResolveQuotaCooldown_RpmLimit(t *testing.T) {
	body := []byte(`{"error":{"code":429,"message":"Quota exceeded per minute for this project."}}`)
	decision := ResolveQuotaCooldown(http.StatusTooManyRequests, body)
	require.False(t, decision.ModelOnly)
	require.WithinDuration(t, time.Now().Add(1*time.Minute), decision.ResetAt, 2*time.Second)
}
