package aistudio

import (
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

var (
	usageResetRegex   = regexp.MustCompile(`resets in (?:(\d+) hours?)?(?:, )?(?:(\d+) minutes?)?`)
	retryInRegex      = regexp.MustCompile(`(?i)(?:try again in|retry in|wait) (\d+(?:\.\d+)?)\s*s`)
	geminiRetryInfoURL = "type.googleapis.com/google.rpc.RetryInfo"
)

// QuotaDecision contains parsed rate limit information.
type QuotaDecision struct {
	ResetAt         time.Time
	ModelOnly       bool
	IsDailyQuota    bool
	CapacityShortage bool
	Reason          string
}

// ResolveQuotaCooldown parses a 429 response body and returns the calculated reset time.
func ResolveQuotaCooldown(statusCode int, body []byte) QuotaDecision {
	now := time.Now()
	msg := strings.ToLower(string(body))

	// 1. Google server capacity shortage (MODEL_CAPACITY_EXHAUSTED)
	if strings.Contains(msg, "model_capacity_exhausted") || strings.Contains(msg, "no capacity available") {
		return QuotaDecision{
			ResetAt:          now.Add(20 * time.Second),
			ModelOnly:        true,
			CapacityShortage: true,
			Reason:           "MODEL_CAPACITY_EXHAUSTED",
		}
	}

	// 2. Check structured RetryInfo from details
	var explicitDelay *time.Duration
	gjson.GetBytes(body, "error.details").ForEach(func(_, detail gjson.Result) bool {
		if detail.Get("@type").String() == geminiRetryInfoURL {
			delayStr := detail.Get("retryDelay").String()
			if dur, err := time.ParseDuration(delayStr); err == nil && dur > 0 {
				explicitDelay = &dur
				return false
			}
		}
		if v := detail.Get("metadata.quotaResetDelay").String(); v != "" {
			if dur, err := time.ParseDuration(v); err == nil && dur > 0 {
				explicitDelay = &dur
				return false
			}
		}
		return true
	})

	if explicitDelay != nil {
		return QuotaDecision{
			ResetAt:   now.Add(*explicitDelay),
			ModelOnly: false,
			Reason:    "explicit_retry_delay",
		}
	}

	// 3. Natural language "resets in X hours, Y minutes"
	if matches := usageResetRegex.FindStringSubmatch(msg); len(matches) > 0 {
		hours, _ := strconv.Atoi(matches[1])
		mins, _ := strconv.Atoi(matches[2])
		dur := time.Duration(hours)*time.Hour + time.Duration(mins)*time.Minute
		if dur > 0 {
			return QuotaDecision{
				ResetAt:   now.Add(dur),
				ModelOnly: false,
				Reason:    "usage_limit_resets_in",
			}
		}
	}

	// 4. Natural language "retry in Xs"
	if matches := retryInRegex.FindStringSubmatch(msg); len(matches) == 2 {
		if sec, err := strconv.ParseFloat(matches[1], 64); err == nil && sec > 0 {
			dur := time.Duration(math.Ceil(sec)) * time.Second
			return QuotaDecision{
				ResetAt:   now.Add(dur),
				ModelOnly: false,
				Reason:    "retry_in_seconds",
			}
		}
	}

	// 5. Daily quota limit -> reset at PST midnight
	if strings.Contains(msg, "daily limit") || strings.Contains(msg, "per day") || strings.Contains(msg, "requests per day") || strings.Contains(msg, "try again tomorrow") {
		loc, err := time.LoadLocation("America/Los_Angeles")
		if err == nil {
			local := now.In(loc)
			nextMidnight := time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, loc)
			return QuotaDecision{
				ResetAt:      nextMidnight,
				ModelOnly:    false,
				IsDailyQuota: true,
				Reason:       "daily_quota_pst_midnight",
			}
		}
	}

	// 6. Minute quota / RPM limit -> 1 minute cooldown
	if strings.Contains(msg, "per minute") || strings.Contains(msg, "per_min") || strings.Contains(msg, "perprojectperuser") {
		return QuotaDecision{
			ResetAt:   now.Add(1 * time.Minute),
			ModelOnly: false,
			Reason:    "rpm_minute_limit",
		}
	}

	// 7. Fallback default: 1 minute
	fallback := 1 * time.Minute
	if statusCode == http.StatusTooManyRequests {
		fallback = 60 * time.Second
	}
	return QuotaDecision{
		ResetAt:   now.Add(fallback),
		ModelOnly: false,
		Reason:    "fallback_429",
	}
}
