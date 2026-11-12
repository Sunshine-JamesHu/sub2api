package service

import (
	"context"
	"errors"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"sync"
	"testing"
	"time"
)

type warmupTestRepo struct {
	AccountRepository
	accounts []Account
}

func (r *warmupTestRepo) ListSchedulableByPlatform(context.Context, string) ([]Account, error) {
	return append([]Account(nil), r.accounts...), nil
}
func (r *warmupTestRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	for _, a := range r.accounts {
		if a.ID == id {
			return &a, nil
		}
	}
	return nil, ErrAccountNotFound
}
func warmupAccount(id int64) Account {
	return Account{ID: id, Platform: PlatformGemini, Type: AccountTypePlayground, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"playground_profile_id": fmt.Sprintf("profile-%d", id), "playground_email": fmt.Sprintf("account-%d@example.test", id)}}
}
func newWarmupFixture(t *testing.T, n int) *PlaygroundWarmupService {
	t.Helper()
	r := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: r.Addr()})
	t.Cleanup(func() { c.Close() })
	repo := &warmupTestRepo{}
	for i := 1; i <= n; i++ {
		repo.accounts = append(repo.accounts, warmupAccount(int64(i)))
	}
	b := NewPlaygroundBrowserService(&aistudio.SessionCache{Client: c}, nil)
	b.recover = func(context.Context, string, string, string) (aistudio.StorageState, error) {
		return playgroundState("fixture"), nil
	}
	b.recoverWarm = b.recover
	gateway := NewPlaygroundGatewayService(repo)
	gateway.browser = b
	gateway.waaManager = &aistudio.WAAManager{}
	w := &PlaygroundWarmupService{browser: b, gateway: gateway, repo: repo, manager: gateway.waaManager, config: defaultPlaygroundWarmupConfig(), records: map[int64]PlaygroundWarmupAccount{}, wake: make(chan struct{}, 1), headroom: func() int64 { return 2 << 30 }}
	b.warmup = w
	t.Cleanup(func() { w.manager.EvictExcept(map[string]bool{}) })
	return w
}
func TestPlaygroundWarmupTargetAndCooldownProxyExclusion(t *testing.T) {
	w := newWarmupFixture(t, 12)
	repo := w.repo.(*warmupTestRepo)
	until := time.Now().Add(time.Hour)
	repo.accounts[0].RateLimitResetAt = &until
	repo.accounts[1].Schedulable = false
	repo.accounts[2].Credentials["playground_require_proxy"] = true
	repo.accounts[3].Type = AccountTypeAPIKey
	var ids []int64
	w.prepare = func(_ context.Context, a *Account) error { ids = append(ids, a.ID); return nil }
	require.NoError(t, w.Configure(context.Background(), PlaygroundWarmupConfig{Enabled: true, Target: 3, MaxCached: 5, IdleMinutes: 30}))
	w.pass(context.Background())
	require.Equal(t, []int64{5, 6, 7}, ids)
	require.Equal(t, 8, w.View().Eligible)
}
func TestPlaygroundWarmupRespectsMemoryAndPersistentConfiguration(t *testing.T) {
	w := newWarmupFixture(t, 10)
	called := 0
	w.prepare = func(context.Context, *Account) error { called++; return nil }
	require.NoError(t, w.Configure(context.Background(), PlaygroundWarmupConfig{Enabled: true, Target: 2, MaxCached: 4, IdleMinutes: 5}))
	w.config = defaultPlaygroundWarmupConfig()
	w.headroom = func() int64 { return 200 << 20 }
	w.pass(context.Background())
	require.Zero(t, called)
	require.Equal(t, "memory", w.View().PausedReason)
	require.Equal(t, 2, w.View().Config.Target)
	w.headroom = func() int64 { return 2 << 30 }
	w.pass(context.Background())
	require.Equal(t, 2, called)
	require.Error(t, w.Configure(context.Background(), PlaygroundWarmupConfig{Enabled: true, Target: 9, MaxCached: 4, IdleMinutes: 5}))
}
func TestPlaygroundWarmupYieldsToForegroundWithoutFailingAccount(t *testing.T) {
	w := newWarmupFixture(t, 1)
	entered := make(chan struct{})
	exited := make(chan struct{})
	w.prepare = func(ctx context.Context, _ *Account) error { close(entered); <-ctx.Done(); return ctx.Err() }
	go func() { w.pass(context.Background()); close(exited) }()
	<-entered
	finish := w.foreground(1)
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("warmup did not yield")
	}
	view := w.View()
	require.Equal(t, 1, view.ActiveRequests)
	require.Empty(t, view.PausedReason)
	require.Len(t, view.Accounts, 1)
	require.Empty(t, view.Accounts[0].Error)
	require.Zero(t, w.records[1].failures)
	finish()
	require.Zero(t, w.View().ActiveRequests)
}
func TestPlaygroundWarmupFailureBackoffAndBusyLease(t *testing.T) {
	w := newWarmupFixture(t, 1)
	calls := 0
	w.prepare = func(context.Context, *Account) error { calls++; return errors.New("synthetic failure") }
	w.pass(context.Background())
	w.pass(context.Background())
	require.Equal(t, 1, calls)
	require.Equal(t, 1, w.records[1].failures)
	a := w.repo.(*warmupTestRepo).accounts[0]
	lease, err := w.browser.Cache.Acquire(context.Background(), a.GetCredential("playground_profile_id"))
	require.NoError(t, err)
	defer lease.Release(context.Background())
	_, _, err = w.gateway.acquireSessionMode(context.Background(), &a, true)
	require.ErrorIs(t, err, aistudio.ErrSessionBusy)
}
func TestPlaygroundWarmupCancellationDoesNotRequireLogin(t *testing.T) {
	w := newWarmupFixture(t, 1)
	w.browser.recoverWarm = func(ctx context.Context, _, _, _ string) (aistudio.StorageState, error) {
		<-ctx.Done()
		return aistudio.StorageState{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := w.repo.(*warmupTestRepo).accounts[0]
	_, _, err := w.gateway.acquireSessionMode(ctx, &a, true)
	require.Error(t, err)
	session, release, err := w.gateway.acquireSession(context.Background(), &a)
	require.NoError(t, err)
	defer release()
	defer session.Close()
}
func TestPlaygroundAccountsAcquireConcurrently(t *testing.T) {
	w := newWarmupFixture(t, 20)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	acquired := make(chan error, 20)
	releaseAll := make(chan struct{})
	var wg sync.WaitGroup
	for _, account := range w.repo.(*warmupTestRepo).accounts {
		wg.Add(1)
		go func(a Account) {
			defer wg.Done()
			_, release, err := w.gateway.acquireSession(ctx, &a)
			acquired <- err
			if err == nil {
				<-releaseAll
				release()
			}
		}(account)
	}
	for i := 0; i < 20; i++ {
		require.NoError(t, <-acquired)
	}
	require.Equal(t, 20, w.View().ActiveRequests)
	close(releaseAll)
	wg.Wait()
	require.Zero(t, w.View().ActiveRequests)
}

func TestPlaygroundWarmupUsesExistingPriorityAndLRURotation(t *testing.T) {
	w := newWarmupFixture(t, 4)
	repo := w.repo.(*warmupTestRepo)
	recent, old := time.Now(), time.Now().Add(-time.Hour)
	repo.accounts[0].LastUsedAt = &recent
	repo.accounts[1].LastUsedAt = &old
	repo.accounts[2].Priority = 1 // A lower-priority account must not displace the next rotation.
	var ids []int64
	w.prepare = func(_ context.Context, a *Account) error { ids = append(ids, a.ID); return nil }
	require.NoError(t, w.Configure(context.Background(), PlaygroundWarmupConfig{Enabled: true, Target: 3, MaxCached: 4, IdleMinutes: 30}))
	w.pass(context.Background())
	require.Equal(t, []int64{4, 2, 1}, ids)
}

func TestPlaygroundWarmupRecoveryRetainsLowPriorityOnReauthentication(t *testing.T) {
	w := newWarmupFixture(t, 1)
	warm, foreground := 0, 0
	w.browser.recoverWarm = func(context.Context, string, string, string) (aistudio.StorageState, error) {
		warm++
		return playgroundState("warm"), nil
	}
	w.browser.recover = func(context.Context, string, string, string) (aistudio.StorageState, error) {
		foreground++
		return playgroundState("foreground"), nil
	}
	a := w.repo.(*warmupTestRepo).accounts[0]
	session, release, err := w.gateway.acquireSessionMode(context.Background(), &a, true)
	require.NoError(t, err)
	defer release()
	require.NoError(t, w.gateway.refreshSession(session))
	require.Equal(t, 2, warm)
	require.Zero(t, foreground)
}

func TestPlaygroundWarmupDoesNotStarveDuringOtherAccountTraffic(t *testing.T) {
	w := newWarmupFixture(t, 2)
	finish := w.foreground(99)
	defer finish()
	var prepared []int64
	w.prepare = func(_ context.Context, a *Account) error { prepared = append(prepared, a.ID); return nil }
	w.pass(context.Background())
	require.Equal(t, []int64{1, 2}, prepared)
	require.Empty(t, w.View().PausedReason)
}
func TestPlaygroundWarmupYieldsBrowserBudgetToRealRecovery(t *testing.T) {
	w := newWarmupFixture(t, 1)
	entered := make(chan struct{})
	exited := make(chan struct{})
	w.prepare = func(ctx context.Context, _ *Account) error { close(entered); <-ctx.Done(); return ctx.Err() }
	go func() { w.pass(context.Background()); close(exited) }()
	<-entered
	finish := w.foregroundRecovery()
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("warmup held the recovery budget")
	}
	require.Equal(t, "foreground", w.View().PausedReason)
	finish()
	require.Empty(t, w.View().PausedReason)
}

func TestPlaygroundWarmRoutingRespectsLoadAndRotatesReadyAccounts(t *testing.T) {
	now := time.Now()
	older := now.Add(-time.Minute)
	a, b, cold := warmupAccount(1), warmupAccount(2), warmupAccount(3)
	candidates := []accountWithLoad{{account: &a, loadInfo: &AccountLoadInfo{}}, {account: &b, loadInfo: &AccountLoadInfo{}}, {account: &cold, loadInfo: &AccountLoadInfo{}}}
	snapshot := []aistudio.CachedSessionInfo{{AccountID: "1", Ready: true, LastUsed: older}, {AccountID: "2", Ready: true, LastUsed: now}}
	preferred := preferReadyPlayground(filterByMinLoadRate(candidates), snapshot)
	require.Len(t, preferred, 2)
	require.Equal(t, int64(1), selectByLRU(preferred, true).account.ID)
	snapshot[0].LastUsed = now.Add(time.Second)
	require.Equal(t, int64(2), selectByLRU(preferReadyPlayground(candidates, snapshot), true).account.ID)
	// A cold idle account beats a hot loaded account; no new request queues on it.
	candidates[0].loadInfo.LoadRate = 50
	candidates[1].loadInfo.LoadRate = 100
	require.Equal(t, int64(3), selectByLRU(preferReadyPlayground(filterByMinLoadRate(candidates), snapshot), true).account.ID)
	require.False(t, shouldClearStickySession(&a, "gemini-3.8-flash"), "retain an idle sticky account; busy Playground accounts still yield to idle alternatives")
	require.Nil(t, a.LastUsedAt, "cached scheduler rows must remain immutable")
}

func TestPlaygroundWarmRoutingPreservesPriorityAndOtherPlatforms(t *testing.T) {
	cold, warm := warmupAccount(1), warmupAccount(2)
	warm.Priority = 10
	candidates := []accountWithLoad{{account: &cold, loadInfo: &AccountLoadInfo{}}, {account: &warm, loadInfo: &AccountLoadInfo{}}}
	snapshot := []aistudio.CachedSessionInfo{{AccountID: "2", Ready: true}}
	preferred := preferReadyPlayground(filterByMinLoadRate(filterByMinPriority(candidates)), snapshot)
	require.Equal(t, int64(1), selectByLRU(preferred, true).account.ID)
	// An account serving a response is not a ready idle alternative.
	snapshot[0].Busy = true
	require.Len(t, preferReadyPlayground(candidates, snapshot), 2)
	// Mixed account pools retain the original scheduler behavior.
	snapshot[0].Busy = false
	cold.Platform = PlatformOpenAI
	cold.Type = AccountTypeAPIKey
	require.Equal(t, candidates, preferReadyPlayground(candidates, snapshot))
}

func TestPlaygroundWarmupErrorKeepsUsefulReasonWithoutSecrets(t *testing.T) {
	require.Contains(t, playgroundWarmupFailure(&aistudio.RPCError{StatusCode: 403, Code: 7, Message: "secret-marker"}), "HTTP 403 / RPC 7")
	require.NotContains(t, playgroundWarmupFailure(&aistudio.RPCError{StatusCode: 403, Code: 7, Message: "secret-marker"}), "secret-marker")
	w := newWarmupFixture(t, 1)
	a := w.repo.(*warmupTestRepo).accounts[0]
	w.prepare = func(context.Context, *Account) error { return &aistudio.RPCError{StatusCode: 503, Code: 14} }
	w.pass(context.Background())
	require.Contains(t, w.View().Accounts[0].Error, "503")
	w.mu.Lock()
	r := w.records[a.ID]
	r.RetryAt = nil
	w.records[a.ID] = r
	w.mu.Unlock()
	w.prepare = func(context.Context, *Account) error { return nil }
	w.pass(context.Background())
	require.Empty(t, w.View().Accounts[0].Error)
}
