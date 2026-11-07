package service

import (
	"context"
	"errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	"github.com/Wei-Shaw/sub2api/internal/pkg/camoufox"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type playgroundTestRepository struct {
	AccountRepository
	mu             sync.Mutex
	account        Account
	errorsRecorded int
}

func (r *playgroundTestRepository) SetError(_ context.Context, id int64, message string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id != r.account.ID {
		return errors.New("unexpected account")
	}
	r.account.Status = StatusError
	r.account.ErrorMessage = message
	r.account.Schedulable = false
	r.errorsRecorded++
	return nil
}

func (r *playgroundTestRepository) GetByID(context.Context, int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	clone := r.account
	var err error
	clone.Credentials, err = cloneAccountJSONMap(clone.Credentials)
	return &clone, err
}
func playgroundState(value string) aistudio.StorageState {
	return aistudio.StorageState{Cookies: []aistudio.StateCookie{{Name: "SAPISID", Value: value, Domain: ".google.com", Path: "/", Expires: -1}}}
}
func newPlaygroundTestAccount(t *testing.T) (*playgroundTestRepository, *Account) {
	t.Helper()
	repo := &playgroundTestRepository{account: Account{ID: 9187, Platform: PlatformGemini, Type: AccountTypePlayground, Credentials: map[string]any{"playground_profile_id": uuid.NewString(), "playground_email": "selected@example.test"}}}
	account, err := repo.GetByID(context.Background(), 9187)
	require.NoError(t, err)
	return repo, account
}
func newPlaygroundTestGateway(t *testing.T, repo *playgroundTestRepository) (*PlaygroundGatewayService, *miniredis.Miniredis) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	browser := NewPlaygroundBrowserService(&aistudio.SessionCache{Client: client}, nil)
	browser.recover = func(context.Context, string, string, string) (aistudio.StorageState, error) {
		return playgroundState("renewed"), nil
	}
	gateway := NewPlaygroundGatewayService(repo)
	gateway.browser = browser
	gateway.waaManager = &aistudio.WAAManager{}
	profile := repo.account.GetCredential("playground_profile_id")
	lease, err := browser.Cache.Acquire(context.Background(), profile)
	require.NoError(t, err)
	require.NoError(t, lease.Put(context.Background(), playgroundCacheKey(profile, "", "selected@example.test"), playgroundState("old")))
	lease.Release(context.Background())
	return gateway, server
}
func TestPlaygroundCacheMissRecoveryAndSlidingExpiry(t *testing.T) {
	repo, account := newPlaygroundTestAccount(t)
	gateway, server := newPlaygroundTestGateway(t, repo)
	var recoveries atomic.Int32
	gateway.browser.recover = func(context.Context, string, string, string) (aistudio.StorageState, error) {
		recoveries.Add(1)
		return playgroundState("renewed"), nil
	}
	session, release, err := gateway.acquireSession(context.Background(), account)
	require.NoError(t, err)
	release()
	server.FastForward(9 * time.Minute)
	session, release, err = gateway.acquireSession(context.Background(), account)
	require.NoError(t, err)
	release()
	require.Zero(t, recoveries.Load())
	server.FastForward(9 * time.Minute)
	session, release, err = gateway.acquireSession(context.Background(), account)
	require.NoError(t, err)
	release()
	require.Zero(t, recoveries.Load())
	server.FastForward(10*time.Minute + time.Second)
	session, release, err = gateway.acquireSession(context.Background(), account)
	require.NoError(t, err)
	defer release()
	defer session.Close()
	require.EqualValues(t, 1, recoveries.Load())
	require.Equal(t, "renewed", session.StorageState().Cookies[0].Value)
	require.NotContains(t, repo.account.Credentials, "cookies")
	require.NotContains(t, repo.account.Credentials, "playground_storage_state")
}
func TestPlaygroundConcurrentRequestsReadRedisUpdates(t *testing.T) {
	repo, account := newPlaygroundTestAccount(t)
	gateway, _ := newPlaygroundTestGateway(t, repo)
	first, release, err := gateway.acquireSession(context.Background(), account)
	require.NoError(t, err)
	secondGateway := NewPlaygroundGatewayService(repo)
	secondGateway.browser = gateway.browser
	secondGateway.waaManager = &aistudio.WAAManager{}
	result := make(chan error, 1)
	go func() {
		next, done, err := secondGateway.acquireSession(context.Background(), account)
		if err != nil {
			result <- err
			return
		}
		defer done()
		defer next.Close()
		if next.StorageState().Cookies[0].Value != "updated" {
			result <- errors.New("stale cookie")
		} else {
			result <- nil
		}
	}()
	require.NoError(t, first.ReplaceState(playgroundState("updated")))
	release()
	first.Close()
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent acquisition deadlocked")
	}
}
func TestPlaygroundExpiredLeaseCannotWriteCookies(t *testing.T) {
	repo, account := newPlaygroundTestAccount(t)
	gateway, server := newPlaygroundTestGateway(t, repo)
	session, release, err := gateway.acquireSession(context.Background(), account)
	require.NoError(t, err)
	defer release()
	defer session.Close()
	server.FastForward(31 * time.Second)
	require.Error(t, session.Commit())
}

func TestPlaygroundLoggedOutProfileDoesNotLaunchOnEveryRequest(t *testing.T) {
	repo, account := newPlaygroundTestAccount(t)
	gateway, server := newPlaygroundTestGateway(t, repo)
	server.FastForward(11 * time.Minute)
	var count atomic.Int32
	gateway.browser.recover = func(context.Context, string, string, string) (aistudio.StorageState, error) {
		count.Add(1)
		return aistudio.StorageState{}, camoufox.ErrInteractiveLoginRequired
	}
	for i := 0; i < 2; i++ {
		_, _, err := gateway.acquireSession(context.Background(), account)
		require.ErrorIs(t, err, aistudio.ErrAuthenticationRequired)
	}
	require.EqualValues(t, 1, count.Load())
}

func TestPlaygroundTransientRecoveryFailureAllowsNextRequest(t *testing.T) {
	for _, failure := range []struct {
		name string
		err  error
	}{
		{"timeout", context.DeadlineExceeded},
		{"cancellation", context.Canceled},
		{"unconfirmed authentication", aistudio.ErrAuthenticationRequired},
		{"network", errors.New("proxy unavailable")},
	} {
		for _, refresh := range []bool{false, true} {
			name := failure.name + "/cache_miss"
			if refresh {
				name = failure.name + "/upstream_authentication"
			}
			t.Run(name, func(t *testing.T) {
				repo, account := newPlaygroundTestAccount(t)
				gateway, server := newPlaygroundTestGateway(t, repo)
				calls := 0
				gateway.browser.recover = func(context.Context, string, string, string) (aistudio.StorageState, error) {
					calls++
					if calls == 1 {
						return aistudio.StorageState{}, failure.err
					}
					return playgroundState("recovered"), nil
				}
				if refresh {
					session, release, err := gateway.acquireSession(context.Background(), account)
					require.NoError(t, err)
					err = gateway.refreshSession(session)
					release()
					session.Close()
					require.ErrorIs(t, err, failure.err)
				} else {
					server.FastForward(11 * time.Minute)
					_, _, err := gateway.acquireSession(context.Background(), account)
					require.ErrorIs(t, err, failure.err)
				}
				session, release, err := gateway.acquireSession(context.Background(), account)
				require.NoError(t, err, "transient failure must not block the next recovery")
				defer release()
				defer session.Close()
				require.Equal(t, 2, calls)
				require.Equal(t, "recovered", session.StorageState().Cookies[0].Value)
			})
		}
	}
}

func TestPlaygroundLoginCannotIssueTwoGrants(t *testing.T) {
	service := &PlaygroundBrowserService{jobs: map[string]*playgroundLoginJob{"completed": {completed: true, expires: time.Now().Add(time.Minute)}}}
	_, _, err := service.Finish(context.Background(), "completed")
	require.ErrorContains(t, err, "already completed")
}
func TestPlaygroundInvalidImportReturnsActionableBadRequest(t *testing.T) {
	for _, credentials := range []map[string]any{{}, {"cookies": "secret"}, {"playground_storage_state": map[string]any{}}, {"playground_profile_id": uuid.NewString()}} {
		service := &adminServiceImpl{}
		_, err := service.normalizePlaygroundCredentials(context.Background(), PlatformGemini, AccountTypePlayground, nil, credentials, nil)
		require.Error(t, err)
		require.True(t, infraerrors.IsBadRequest(err))
		require.Equal(t, "PLAYGROUND_BROWSER_LOGIN_REQUIRED", infraerrors.Reason(err))
		require.NotContains(t, err.Error(), "secret")
	}
}
func TestPlaygroundLoginGrantBindsIdentityAndCannotBeReused(t *testing.T) {
	repo, _ := newPlaygroundTestAccount(t)
	gateway, _ := newPlaygroundTestGateway(t, repo)
	service := &adminServiceImpl{playgroundBrowser: gateway.browser}
	profile := uuid.NewString()
	token, err := gateway.browser.Cache.IssueGrant(context.Background(), aistudio.BrowserGrant{Profile: profile, Email: "test@example.test", Proxy: aistudio.ComputePayloadDigest("")})
	require.NoError(t, err)
	input := map[string]any{"playground_login_token": token, "playground_profile_id": "attacker"}
	next, err := service.normalizePlaygroundCredentials(context.Background(), PlatformGemini, AccountTypePlayground, nil, input, nil)
	require.NoError(t, err)
	require.Equal(t, profile, next["playground_profile_id"])
	require.NotContains(t, next, "playground_login_token")
	_, err = service.normalizePlaygroundCredentials(context.Background(), PlatformGemini, AccountTypePlayground, nil, input, nil)
	require.Error(t, err)
	edit, err := service.normalizePlaygroundCredentials(context.Background(), PlatformGemini, AccountTypePlayground, nil, map[string]any{"playground_profile_id": "replacement"}, next)
	require.NoError(t, err)
	require.Equal(t, profile, edit["playground_profile_id"])
}
func TestPlaygroundSSECollectPreservesEveryPartAndFinalStatus(t *testing.T) {
	source := strings.NewReader("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"thinking\",\"thought\":true}]}}]}\n\n" +
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"first\"},{\"functionCall\":{\"name\":\"tool\",\"args\":{\"x\":1}}}]}}]}\n\n" +
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"last\"}]},\"finishReason\":\"STOP\"}]}\n\n")
	body := &playgroundBody{ReadCloser: io.NopCloser(source), done: func() {}}
	response, _, err := collectGeminiSSE(body, false)
	require.NoError(t, err)
	parts := extractGeminiParts(response)
	require.Len(t, parts, 4)
	require.Equal(t, true, parts[0]["thought"])
	require.Equal(t, "last", parts[3]["text"])
	require.Equal(t, "STOP", response["candidates"].([]any)[0].(map[string]any)["finishReason"])
}

func TestPlaygroundSSECollectPropagatesLateReadError(t *testing.T) {
	reader, writer := io.Pipe()
	expected := errors.New("RPC trailer denied")
	go func() {
		_, _ = io.WriteString(writer, "data: {\"candidates\":[{\"finishReason\":\"STOP\"}]}\n\n")
		_ = writer.CloseWithError(expected)
	}()
	_, _, _, err := collectPlaygroundSSE(reader, nil)
	require.ErrorIs(t, err, expected)
}
