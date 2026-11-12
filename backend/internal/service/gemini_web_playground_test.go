package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestGeminiWebGrantCannotCrossPlaygroundSources(t *testing.T) {
	for _, tc := range []struct {
		grant, requested string
		valid            bool
	}{
		{"", "aistudio", true}, {"gemini_web", "gemini_web", true}, {"aistudio", "gemini_web", false}, {"gemini_web", "aistudio", false},
	} {
		t.Run(tc.grant+"->"+tc.requested, func(t *testing.T) {
			repo, _ := newPlaygroundTestAccount(t)
			gateway, _ := newPlaygroundTestGateway(t, repo)
			admin := &adminServiceImpl{playgroundBrowser: gateway.browser}
			token, err := gateway.browser.Cache.IssueGrant(context.Background(), aistudio.BrowserGrant{Profile: uuid.NewString(), Email: "owner@example.test", Proxy: aistudio.ComputePayloadDigest(""), Backend: tc.grant})
			require.NoError(t, err)
			next, err := admin.normalizePlaygroundCredentials(context.Background(), PlatformGemini, AccountTypePlayground, nil, map[string]any{"playground_backend": tc.requested, "playground_login_token": token}, nil)
			if !tc.valid {
				require.Equal(t, "PLAYGROUND_SOURCE_CHANGED", infraerrors.Reason(err))
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.requested, next["playground_backend"])
			require.Equal(t, tc.requested, next["playground_auth_source"].(map[string]any)["backend"])
		})
	}
}

func TestGeminiWebSourceChangeRequiresNewLogin(t *testing.T) {
	admin := &adminServiceImpl{}
	existing := map[string]any{"playground_backend": "gemini_web", "playground_profile_id": uuid.NewString(), "playground_email": "owner@example.test"}
	_, err := admin.normalizePlaygroundCredentials(context.Background(), PlatformGemini, AccountTypePlayground, nil, map[string]any{"playground_backend": "aistudio"}, existing)
	require.Equal(t, "PLAYGROUND_BROWSER_LOGIN_REQUIRED", infraerrors.Reason(err))
	next, err := admin.normalizePlaygroundCredentials(context.Background(), PlatformGemini, AccountTypePlayground, nil, map[string]any{"model_mapping": map[string]any{"alias": "gemini-flash"}}, existing)
	require.NoError(t, err)
	require.Equal(t, "gemini_web", next["playground_backend"])
	_, err = admin.normalizePlaygroundCredentials(context.Background(), PlatformGemini, AccountTypePlayground, nil, map[string]any{"playground_backend": "unknown"}, existing)
	require.Equal(t, "INVALID_PLAYGROUND_BACKEND", infraerrors.Reason(err))
}

func TestGeminiWebDoesNotEnterAIStudioWarmup(t *testing.T) {
	account := &Account{Platform: PlatformGemini, Type: AccountTypePlayground, Credentials: map[string]any{"playground_backend": "gemini_web", "playground_profile_id": uuid.NewString(), "playground_email": "owner@example.test"}}
	require.True(t, isGeminiWebPlayground(account))
	require.False(t, playgroundWarmupEligible(account))
}

func TestGeminiWebBatchDoesNotSkipAnAIStudioIdentity(t *testing.T) {
	s, admin, _ := batchFixture(t)
	admin.mu.Lock()
	admin.accounts = append(admin.accounts, Account{ID: 7, Platform: PlatformGemini, Type: AccountTypePlayground, Credentials: map[string]any{"playground_email": "owner@example.test"}})
	admin.mu.Unlock()
	s.mu.Lock()
	s.startGeminiBrowser = func(context.Context, string, string, bool, string) (batchBrowser, error) {
		return &batchFakeBrowser{manual: true}, nil
	}
	s.mu.Unlock()
	id, err := s.Create(context.Background(), 1, PlaygroundBatchRequest{Accounts: "owner@example.test----synthetic----", ProxyIDs: []int64{1}, Concurrency: 1, Backend: "gemini_web"})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return s.View(1, id).Batches[0].Counts["manual"] == 1 }, 5*time.Second, 10*time.Millisecond)
	view := s.View(1, id).Batches[0]
	require.Equal(t, "gemini_web", view.Backend)
	require.Zero(t, view.Counts["skipped"])
	found, err := s.existingAccount(context.Background(), "owner@example.test", "other", "gemini_web")
	require.NoError(t, err)
	require.Nil(t, found)
	_, err = s.Create(context.Background(), 1, PlaygroundBatchRequest{Backend: "invalid"})
	require.Error(t, err)
}
