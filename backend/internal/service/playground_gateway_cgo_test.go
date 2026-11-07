//go:build cgo

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	"github.com/stretchr/testify/require"
)

const playgroundTestRequest = `{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`

func playgroundResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
func playgroundSuccessResponse() *http.Response {
	return playgroundFinishedResponse(1)
}
func playgroundFinishedResponse(code int) *http.Response {
	candidate := []any{[]any{[]any{[]any{nil, "OK"}}, "model"}, code}
	frame := []any{[]any{candidate}}
	raw, _ := json.Marshal([]any{[]any{frame}, []any{0}})
	return playgroundResponse(string(raw))
}

func TestPlaygroundConcurrentAuthFailureRenewsOnce(t *testing.T) {
	for _, viaTrailer := range []bool{false, true} {
		t.Run(map[bool]string{false: "HTTP401", true: "RPC16"}[viaTrailer], func(t *testing.T) {
			repo, account := newPlaygroundTestAccount(t)
			gateway, _ := newPlaygroundTestGateway(t, repo)
			gateway.waaManager = &aistudio.WAAManager{}
			var renewals, requests atomic.Int32
			gateway.browser.recover = func(context.Context, string, string, string) (aistudio.StorageState, error) {
				renewals.Add(1)
				return playgroundState("renewed"), nil
			}
			gateway.generateContent = func(_ context.Context, session *aistudio.Session, _ string, _ []byte) (*http.Response, error) {
				requests.Add(1)
				value, _ := session.StorageState().CookieValue("SAPISID", aistudio.AIStudioReferer, time.Now())
				if value != "renewed" {
					if viaTrailer {
						return playgroundResponse(`[[],[16,"expired"]]`), nil
					}
					return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				return playgroundSuccessResponse(), nil
			}
			var wg sync.WaitGroup
			failures := make(chan error, 8)
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					result, err := gateway.TestConnectionWithPrompt(context.Background(), account, "m", "Hi")
					if err == nil && result.Text != "OK" {
						err = errors.New("missing final text")
					}
					failures <- err
				}()
			}
			wg.Wait()
			close(failures)
			for err := range failures {
				require.NoError(t, err)
			}
			require.EqualValues(t, 1, renewals.Load())
			require.EqualValues(t, 9, requests.Load())
		})
	}
}

func TestPlaygroundTestRejectsTruncationAndPermissionWithoutRenewal(t *testing.T) {
	badFinish := playgroundFinishedResponse(10)
	badFinishBody, err := io.ReadAll(badFinish.Body)
	require.NoError(t, err)
	badFinish.Body.Close()
	for _, response := range []string{`[[],[7,"permission denied"]]`, `[[[[[[[null,"partial"]],"model"],1]]]]`, `[[]]`, string(badFinishBody)} {
		repo, account := newPlaygroundTestAccount(t)
		gateway, _ := newPlaygroundTestGateway(t, repo)
		gateway.waaManager = &aistudio.WAAManager{}
		var renewals atomic.Int32
		gateway.generateContent = func(context.Context, *aistudio.Session, string, []byte) (*http.Response, error) {
			return playgroundResponse(response), nil
		}
		gateway.browser.recover = func(context.Context, string, string, string) (aistudio.StorageState, error) {
			renewals.Add(1)
			return aistudio.StorageState{}, errors.New("unexpected renewal")
		}
		_, err := gateway.TestConnectionWithPrompt(context.Background(), account, "m", "Hi")
		require.Error(t, err)
		require.Zero(t, renewals.Load())
	}
}

func TestPlaygroundRegionFailureDoesNotRenew(t *testing.T) {
	repo, account := newPlaygroundTestAccount(t)
	gateway, _ := newPlaygroundTestGateway(t, repo)
	gateway.waaManager = &aistudio.WAAManager{}
	var renewals atomic.Int32
	gateway.generateContent = func(context.Context, *aistudio.Session, string, []byte) (*http.Response, error) {
		response := playgroundResponse(`[7,"Region not supported."]`)
		response.StatusCode = http.StatusForbidden
		return response, nil
	}
	gateway.browser.recover = func(context.Context, string, string, string) (aistudio.StorageState, error) {
		renewals.Add(1)
		return aistudio.StorageState{}, errors.New("unexpected renewal")
	}
	_, err := gateway.TestConnectionWithPrompt(context.Background(), account, "m", "Hi")
	var rpcErr *aistudio.RPCError
	require.ErrorAs(t, err, &rpcErr)
	require.True(t, rpcErr.RegionNotSupported())
	require.Zero(t, renewals.Load())
}

func TestPlaygroundPermissionFailureOnlyCoolsDownRequestedModel(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		expireFirst bool
	}{
		{"HTTP403", http.StatusForbidden, `[7,"The caller does not have permission"]`, false},
		{"RPC7 trailer", http.StatusOK, `[[],[7,"The caller does not have permission"]]`, false},
		{"HTTP403 after session recovery", http.StatusForbidden, `[7,"The caller does not have permission"]`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, account := newPlaygroundTestAccount(t)
			repo.account.Status = StatusActive
			repo.account.Schedulable = true
			gateway, _ := newPlaygroundTestGateway(t, repo)
			calls := 0
			gateway.generateContent = func(context.Context, *aistudio.Session, string, []byte) (*http.Response, error) {
				calls++
				if tc.expireFirst && calls == 1 {
					return playgroundResponse(`[[],[16,"session expired"]]`), nil
				}
				response := playgroundResponse(tc.body)
				response.StatusCode = tc.status
				return response, nil
			}
			_, err := gateway.TestConnectionWithPrompt(context.Background(), account, "tested-model", "Hi")
			var rpcErr *aistudio.RPCError
			require.ErrorAs(t, err, &rpcErr)
			require.Equal(t, 7, rpcErr.Code)
			require.Equal(t, StatusActive, repo.account.Status)
			require.True(t, repo.account.Schedulable)
			require.Zero(t, repo.errorsRecorded)
			require.Empty(t, repo.account.ErrorMessage)
			require.Equal(t, 1, repo.cooldownsRecorded)
			require.False(t, repo.account.IsSchedulableForModel("tested-model"))
			require.True(t, repo.account.IsSchedulableForModel("other-model"))
			require.InDelta(t, time.Minute.Seconds(), repo.account.GetModelRateLimitRemainingTime("tested-model").Seconds(), 2)
			reason := repo.account.modelRateLimitReason("tested-model")
			require.Contains(t, reason, "RPC 7")
			require.Contains(t, reason, "The caller does not have permission")
			setAccountModelRateLimitSnapshot(&repo.account, "tested-model", time.Now().Add(-time.Second), reason, time.Now())
			require.True(t, repo.account.IsSchedulableForModel("tested-model"), "cooldown must expire automatically")
		})
	}
}
