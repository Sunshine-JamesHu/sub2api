package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

func TestPlaygroundBusyStickySelection(t *testing.T) {
	for _, tc := range []struct {
		name, platform, accountType string
		busySticky, busyFallback    bool
		wantID                      int64
		wantWait                    bool
	}{
		{"playground_uses_idle_fallback", PlatformGemini, AccountTypePlayground, true, false, 2, false},
		{"playground_keeps_idle_sticky", PlatformGemini, AccountTypePlayground, false, false, 1, false},
		{"playground_all_busy_still_waits", PlatformGemini, AccountTypePlayground, true, true, 1, true},
		{"gemini_api_key_preserves_sticky_wait", PlatformGemini, AccountTypeAPIKey, true, false, 1, true},
		{"anthropic_preserves_sticky_wait", PlatformAnthropic, AccountTypeAPIKey, true, false, 1, true},
		{"openai_preserves_sticky_wait", PlatformOpenAI, AccountTypeAPIKey, true, false, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accounts := []Account{
				{ID: 1, Platform: tc.platform, Type: tc.accountType, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0},
				{ID: 2, Platform: tc.platform, Type: tc.accountType, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5},
			}
			loads := map[int64]*AccountLoadInfo{
				1: {AccountID: 1}, 2: {AccountID: 2},
			}
			if tc.busySticky {
				loads[1].LoadRate = 100
			}
			if tc.busyFallback {
				loads[2].LoadRate = 100
			}
			svc := &GatewayService{
				accountRepo: stubOpenAIAccountRepo{accounts: accounts},
				cache:       &stickyGatewayCacheHotpathStub{stickyID: 1},
				cfg: &config.Config{RunMode: config.RunModeStandard, Gateway: config.GatewayConfig{
					Scheduling: config.GatewaySchedulingConfig{
						LoadBatchEnabled: true, StickySessionMaxWaiting: 3,
						StickySessionWaitTimeout: time.Minute, FallbackWaitTimeout: time.Minute, FallbackMaxWaiting: 10,
					},
				}},
				concurrencyService: NewConcurrencyService(stubConcurrencyCache{
					loadMap: loads, acquireResults: map[int64]bool{1: !tc.busySticky, 2: !tc.busyFallback},
				}),
			}
			ctx := context.WithValue(context.Background(), ctxkey.ForcePlatform, tc.platform)
			result, err := svc.SelectAccountWithLoadAwareness(ctx, nil, "busy-session", "", nil, "", 0)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, tc.wantID, result.Account.ID)
			require.Equal(t, tc.wantWait, result.WaitPlan != nil)
			require.Equal(t, !tc.wantWait, result.Acquired)
			if result.ReleaseFunc != nil {
				result.ReleaseFunc()
			}
		})
	}
}
