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
}
