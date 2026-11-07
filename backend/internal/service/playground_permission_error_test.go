package service

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	"github.com/stretchr/testify/require"
)

func TestPlaygroundTemporaryAndUnrelatedFailuresDoNotDisableAccount(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"success", nil},
		{"timeout", context.DeadlineExceeded},
		{"network", errors.New("proxy connection interrupted")},
		{"unconfirmed authentication", aistudio.ErrAuthenticationRequired},
		{"quota", &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8, Message: "quota exhausted"}},
		{"unknown model", &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusNotFound, Code: 5, Message: "not found"}},
		{"region", &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusForbidden, Code: 7, Message: "Region not supported."}},
		{"region in trailer", &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusOK, Code: 7, Message: "Region not supported."}},
		{"bootstrap permission", &aistudio.RPCError{Method: "ListPrompts", StatusCode: http.StatusForbidden, Code: 7, Message: "permission denied"}},
		{"server failure", &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusServiceUnavailable, Code: 7, Message: "unavailable"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, account := newPlaygroundTestAccount(t)
			repo.account.Status = StatusActive
			repo.account.Schedulable = true
			gateway := NewPlaygroundGatewayService(repo)
			gateway.markGenerationPermissionError(context.Background(), account, "m", tc.err)
			require.Equal(t, StatusActive, repo.account.Status)
			require.True(t, repo.account.Schedulable)
			require.Zero(t, repo.errorsRecorded)
			require.Zero(t, repo.cooldownsRecorded)
		})
	}
}

func TestPlaygroundCancelledRequestDoesNotPersistPermissionError(t *testing.T) {
	repo, account := newPlaygroundTestAccount(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	gateway := NewPlaygroundGatewayService(repo)
	gateway.markGenerationPermissionError(ctx, account, "m", &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusForbidden, Code: 7})
	require.Zero(t, repo.errorsRecorded)
	require.Zero(t, repo.cooldownsRecorded)
}

func TestPlaygroundPermissionCooldownRespectsModelMapping(t *testing.T) {
	repo, account := newPlaygroundTestAccount(t)
	repo.account.Status, repo.account.Schedulable = StatusActive, true
	repo.account.Credentials["model_mapping"] = map[string]any{"alias": "gemini-4-argon", "gemini-4-argon": "different-upstream"}
	gateway := NewPlaygroundGatewayService(repo)
	gateway.markGenerationPermissionError(context.Background(), account, "gemini-4-argon", &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusForbidden, Code: 7, Message: "The caller does not have permission"})
	require.Zero(t, repo.errorsRecorded)
	require.Equal(t, 1, repo.cooldownsRecorded)
	require.False(t, repo.account.IsSchedulableForModel("alias"))
	require.True(t, repo.account.IsSchedulableForModel("gemini-4-argon"), "the upstream model must not be mapped a second time when recording cooldown")
	require.True(t, repo.account.IsSchedulableForModel("gemini-3.8-flash"))
}

func TestPlaygroundPermissionDoesNotAffectOtherAccountTypes(t *testing.T) {
	for _, tc := range []struct{ platform, accountType string }{
		{PlatformGemini, AccountTypeOAuth},
		{PlatformGemini, AccountTypeAPIKey},
		{PlatformOpenAI, AccountTypePlayground},
		{PlatformAnthropic, AccountTypePlayground},
		{PlatformAntigravity, AccountTypePlayground},
	} {
		t.Run(tc.platform+"/"+tc.accountType, func(t *testing.T) {
			repo, account := newPlaygroundTestAccount(t)
			account.Platform, account.Type = tc.platform, tc.accountType
			gateway := NewPlaygroundGatewayService(repo)
			gateway.markGenerationPermissionError(context.Background(), account, "m", &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusForbidden, Code: 7})
			require.Zero(t, repo.errorsRecorded)
			require.Zero(t, repo.cooldownsRecorded)
		})
	}
}

func TestPlaygroundPermissionWithoutModelDoesNotDisableAccount(t *testing.T) {
	repo, account := newPlaygroundTestAccount(t)
	gateway := NewPlaygroundGatewayService(repo)
	gateway.markGenerationPermissionError(context.Background(), account, " ", &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusForbidden, Code: 7})
	require.Zero(t, repo.errorsRecorded)
	require.Zero(t, repo.cooldownsRecorded)
}
