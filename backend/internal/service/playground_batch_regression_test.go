package service

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/camoufox"
	"github.com/stretchr/testify/require"
)

func TestReviewBatchCleanupWorkerProcessesZeroAccountIntent(t *testing.T) {
	admin, repo, _, profile, path := playgroundCleanupFixture(t)
	require.NoError(t, admin.playgroundBrowser.prepareProfileCleanup(profile, 0))
	admin.playgroundBrowser.retryProfileCleanup(context.Background(), repo)
	require.DirExists(t, path, "committed accounts must keep their profile")
	repo.account = nil
	require.NoError(t, admin.playgroundBrowser.prepareProfileCleanup(profile, 0))
	admin.playgroundBrowser.retryProfileCleanup(context.Background(), repo)
	_, err := os.Stat(path)
	require.True(t, os.IsNotExist(err), "batch cleanup worker left abandoned profile on disk")
}

func TestReviewBatchRetryKeepsSubmittedProxy(t *testing.T) {
	s, _, proxies := batchFixture(t)
	proxies.mu.Lock()
	proxies.proxies = append(proxies.proxies, Proxy{ID: 2, Protocol: "http", Host: "127.0.0.2", Port: 32124, Status: StatusActive})
	proxies.mu.Unlock()
	s.startBrowser = func(context.Context, string, string, bool, string) (batchBrowser, error) {
		return &batchFakeBrowser{manual: true}, nil
	}
	id, err := s.Create(context.Background(), 1, PlaygroundBatchRequest{Accounts: "owner@example.test----synthetic----", ProxyIDs: []int64{1, 2}, Concurrency: 1})
	require.NoError(t, err)
	require.Eventually(t, func() bool { v := s.View(1, id); return v.Batches[0].Counts["manual"] == 1 && v.Running == 0 }, 5*time.Second, 10*time.Millisecond)
	job := s.View(1, id).Batches[0].Jobs[0]
	s.mu.Lock()
	_, j := s.jobLocked(id, job.ID)
	profile := j.Profile
	j.Progress = camoufox.GoogleLoginProgress{Submitted: map[string]time.Time{"password": time.Now()}}
	err = s.saveLocked()
	s.mu.Unlock()
	require.NoError(t, err)
	require.NoError(t, s.Action(1, id, job.ID, "retry"))
	require.Eventually(t, func() bool { v := s.View(1, id); return v.Batches[0].Counts["manual"] == 1 && v.Running == 0 }, 5*time.Second, 10*time.Millisecond)
	s.mu.Lock()
	_, j = s.jobLocked(id, job.ID)
	actualProfile, actualProxy := j.Profile, j.ProxyID
	s.mu.Unlock()
	require.Equal(t, profile, actualProfile)
	require.Equal(t, job.ProxyID, actualProxy, "retry changed proxy for the same credential-submitted profile")
}

func TestReviewBatchCancelDuringManualSave(t *testing.T) {
	s, admin, _ := batchFixture(t)
	var starts atomic.Int32
	human := &batchFakeBrowser{}
	s.startBrowser = func(context.Context, string, string, bool, string) (batchBrowser, error) {
		if starts.Add(1) == 1 {
			return &batchFakeBrowser{manual: true}, nil
		}
		return human, nil
	}
	entered, release := make(chan struct{}), make(chan struct{})
	savingContext := make(chan context.Context, 1)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	s.createAccount = func(ctx context.Context, input *CreateAccountInput) (*Account, error) {
		savingContext <- ctx
		close(entered)
		<-release
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return admin.CreateAccount(ctx, input)
	}
	id, err := s.Create(context.Background(), 1, PlaygroundBatchRequest{Accounts: "owner@example.test----synthetic----", ProxyIDs: []int64{1}, Concurrency: 1})
	require.NoError(t, err)
	require.Eventually(t, func() bool { v := s.View(1, id); return v.Batches[0].Counts["manual"] == 1 && v.Running == 0 }, 5*time.Second, 10*time.Millisecond)
	job := s.View(1, id).Batches[0].Jobs[0]
	require.NoError(t, s.Action(1, id, job.ID, "open_manual"))
	require.Eventually(t, func() bool { return s.View(1, id).Batches[0].Jobs[0].ManualOpen }, 3*time.Second, 10*time.Millisecond)
	result := make(chan error, 1)
	go func() { result <- s.ManualFinish(context.Background(), 1, id, job.ID) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("save did not start")
	}
	s.mu.Lock()
	_, savingJob := s.jobLocked(id, job.ID)
	profile := savingJob.Profile
	s.mu.Unlock()
	lease, err := s.browser.Cache.TryAcquire(context.Background(), profile)
	if lease != nil {
		lease.Release(context.Background())
	}
	require.Error(t, err, "profile cleanup must stay excluded while account creation is pending")
	require.NoError(t, s.Action(1, id, job.ID, "cancel"))
	select {
	case <-((<-savingContext).Done()):
	case <-time.After(time.Second):
		t.Fatal("manual save did not observe job cancellation")
	}
	require.Equal(t, 1, s.View(1, id).Running, "manual save must remain tracked until it exits")
	require.Equal(t, "cancelled", s.View(1, id).Batches[0].Jobs[0].Status)
	close(release)
	require.Error(t, <-result)
	require.Equal(t, "cancelled", s.View(1, id).Batches[0].Jobs[0].Status, "save resurrected a cancelled job")
	admin.mu.Lock()
	defer admin.mu.Unlock()
	require.Empty(t, admin.accounts)
}

func TestReviewBatchRejectsChangedProxyOnManualResume(t *testing.T) {
	s, _, proxies := batchFixture(t)
	var starts atomic.Int32
	s.startBrowser = func(context.Context, string, string, bool, string) (batchBrowser, error) {
		starts.Add(1)
		return &batchFakeBrowser{manual: true}, nil
	}
	id, err := s.Create(context.Background(), 1, PlaygroundBatchRequest{Accounts: "owner@example.test----synthetic----", ProxyIDs: []int64{1}, Concurrency: 1})
	require.NoError(t, err)
	require.Eventually(t, func() bool { v := s.View(1, id); return v.Batches[0].Counts["manual"] == 1 && v.Running == 0 }, 5*time.Second, 10*time.Millisecond)
	job := s.View(1, id).Batches[0].Jobs[0]
	s.mu.Lock()
	loaded, err := s.store.Load()
	s.mu.Unlock()
	require.NoError(t, err)
	require.NotEmpty(t, loaded.Batches[0].Jobs[0].ProxyDigest, "proxy binding must survive restart")
	proxies.mu.Lock()
	proxies.proxies[0].Host = "127.0.0.2"
	proxies.mu.Unlock()
	require.NoError(t, s.Action(1, id, job.ID, "open_manual"))
	require.Eventually(t, func() bool { return s.View(1, id).Batches[0].Jobs[0].Reason == batchReason("proxy_unavailable") }, 3*time.Second, 10*time.Millisecond)
	require.EqualValues(t, 1, starts.Load(), "changed egress must not reopen the same profile")
}
