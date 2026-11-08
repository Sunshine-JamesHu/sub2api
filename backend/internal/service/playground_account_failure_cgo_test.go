//go:build cgo

package service

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	"github.com/Wei-Shaw/sub2api/internal/pkg/camoufox"
	"github.com/stretchr/testify/require"
)

func TestPlaygroundManualActionGenerationHTTPAndTrailer(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusOK} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			repo, account := newPlaygroundTestAccount(t)
			repo.account.Status, repo.account.Schedulable = StatusActive, true
			gateway, _ := newPlaygroundTestGateway(t, repo)
			gateway.generateContent = func(context.Context, *aistudio.Session, string, []byte) (*http.Response, error) {
				body := `[7,"Account not supported."]`
				if status == 200 {
					body = `[[],` + body + `]`
				}
				resp := playgroundResponse(body)
				resp.StatusCode = status
				return resp, nil
			}
			resp, err := gateway.DoGemini(context.Background(), account, "flash", []byte(playgroundTestRequest))
			if status == 200 {
				require.NoError(t, err)
				_, err = io.ReadAll(resp.Body)
				defer resp.Body.Close()
			}
			require.Error(t, err)
			repo.mu.Lock()
			defer repo.mu.Unlock()
			require.Equal(t, 1, repo.errorsRecorded)
			require.False(t, repo.account.Schedulable)
			require.Contains(t, repo.account.ErrorMessage, "unsupported_account")
			require.Zero(t, repo.cooldownsRecorded)
		})
	}
}

func TestPlaygroundManualActionLoginConfirmationAndCachedMarker(t *testing.T) {
	for _, mode := range []string{"cache miss", "refresh", "existing marker", "missing authorization"} {
		t.Run(mode, func(t *testing.T) {
			repo, account := newPlaygroundTestAccount(t)
			repo.account.Status, repo.account.Schedulable = StatusActive, true
			gateway, server := newPlaygroundTestGateway(t, repo)
			recoveries := 0
			gateway.browser.recover = func(context.Context, string, string, string) (aistudio.StorageState, error) {
				recoveries++
				return aistudio.StorageState{}, camoufox.ErrInteractiveLoginRequired
			}
			switch mode {
			case "cache miss":
				server.FastForward(aistudio.SessionCacheTTL + time.Minute)
			case "existing marker":
				profile := account.GetCredential("playground_profile_id")
				lease, err := gateway.browser.Cache.Acquire(context.Background(), profile)
				require.NoError(t, err)
				require.NoError(t, lease.RequireLogin(context.Background(), playgroundCacheKey(profile, "", "selected@example.test")))
				lease.Release(context.Background())
			case "missing authorization":
				delete(repo.account.Credentials, "playground_email")
			}
			if mode == "refresh" {
				calls := 0
				gateway.generateContent = func(context.Context, *aistudio.Session, string, []byte) (*http.Response, error) {
					calls++
					return nil, &aistudio.RPCError{Method: "GenerateContent", StatusCode: 401, Code: 16, Message: "expired"}
				}
				_, err := gateway.DoGemini(context.Background(), account, "flash", []byte(playgroundTestRequest))
				require.ErrorIs(t, err, camoufox.ErrInteractiveLoginRequired)
				require.Equal(t, 1, calls)
			} else {
				_, _, err := gateway.acquireSession(context.Background(), account)
				require.Error(t, err)
			}
			require.Equal(t, 1, repo.errorsRecorded)
			require.False(t, repo.account.Schedulable)
			if mode == "existing marker" || mode == "missing authorization" {
				require.Zero(t, recoveries)
			} else {
				require.Equal(t, 1, recoveries)
			}
		})
	}
}

func TestPlaygroundManualActionUsesAcquiredRevisionAndRejectsLateFailure(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(map[bool]string{false: "stale scheduler input", true: "late old session failure"}[late], func(t *testing.T) {
			repo, account := newPlaygroundTestAccount(t)
			repo.account.Status, repo.account.Schedulable = StatusActive, true
			repo.account.UpdatedAt = time.Now().UTC()
			gateway, _ := newPlaygroundTestGateway(t, repo)
			gateway.generateContent = func(context.Context, *aistudio.Session, string, []byte) (*http.Response, error) {
				if late {
					repo.mu.Lock()
					repo.account.UpdatedAt = repo.account.UpdatedAt.Add(time.Second)
					repo.account.Credentials["playground_profile_id"] = "reauthorized"
					repo.mu.Unlock()
				}
				return nil, &aistudio.RPCError{Method: "GenerateContent", StatusCode: 403, Code: 7, Message: "Account not supported."}
			}
			_, err := gateway.DoGemini(context.Background(), account, "flash", []byte(playgroundTestRequest))
			require.Error(t, err)
			if late {
				require.Zero(t, repo.errorsRecorded)
				require.True(t, repo.account.Schedulable)
			} else {
				require.Equal(t, 1, repo.errorsRecorded)
				require.False(t, repo.account.Schedulable)
			}
		})
	}
}
