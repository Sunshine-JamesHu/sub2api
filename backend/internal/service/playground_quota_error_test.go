package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestPlaygroundQuotaErrorPersistsOnlyRequestedModel(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusOK} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			repo, account := newPlaygroundTestAccount(t)
			repo.account.Status, repo.account.Schedulable = StatusActive, true
			gateway := NewPlaygroundGatewayService(repo)
			err := &aistudio.RPCError{Method: "GenerateContent", StatusCode: status, Code: 8, Message: "You've reached your usage limit for this model (resets in 4 hours, 17 minutes)."}
			gateway.markGenerationQuotaError(context.Background(), account, "flash", err)
			require.InDelta(t, (4*time.Hour + 17*time.Minute).Seconds(), repo.account.GetModelRateLimitRemainingTime("flash").Seconds(), 2)
			require.True(t, repo.account.IsSchedulableForModel("argon"))
			require.Equal(t, StatusActive, repo.account.Status)
			require.True(t, repo.account.Schedulable)
			require.Zero(t, repo.errorsRecorded)
			require.Equal(t, 1, repo.cooldownsRecorded)
		})
	}
}

func TestPlaygroundQuotaErrorIgnoresUnrelatedAndCancelledFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		cancel bool
	}{
		{"success", nil, false},
		{"network", errors.New("connection reset"), false},
		{"permission", &aistudio.RPCError{Method: "GenerateContent", StatusCode: 403, Code: 7}, false},
		{"bootstrap", &aistudio.RPCError{Method: "ListModels", StatusCode: 429, Code: 8}, false},
		{"cancelled", &aistudio.RPCError{Method: "GenerateContent", StatusCode: 429, Code: 8}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, account := newPlaygroundTestAccount(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			NewPlaygroundGatewayService(repo).markGenerationQuotaError(ctx, account, "flash", tc.err)
			require.Zero(t, repo.cooldownsRecorded)
			require.Zero(t, repo.errorsRecorded)
		})
	}
}

func TestPlaygroundQuotaFailoverPreserves429(t *testing.T) {
	for _, accountType := range []string{AccountTypePlayground, AccountTypeAPIKey} {
		t.Run(accountType, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx := context.Background()
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			svc := &GeminiMessagesCompatService{}
			err := svc.handleUpstreamTransportError(ctx, c, &Account{ID: 1, Platform: PlatformGemini, Type: accountType}, &aistudio.RPCError{Method: "GenerateContent", StatusCode: 429, Code: 8, Message: "model quota exhausted"})
			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			want := http.StatusBadGateway
			if accountType == AccountTypePlayground {
				want = http.StatusTooManyRequests
			}
			require.Equal(t, want, failover.StatusCode)
			require.False(t, c.Writer.Written())
		})
	}
}
