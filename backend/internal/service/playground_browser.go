package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	"github.com/Wei-Shaw/sub2api/internal/pkg/camoufox"
	"github.com/google/uuid"
)

type PlaygroundBrowserService struct {
	Cache   *aistudio.SessionCache
	manager *camoufox.Manager
	proxies ProxyRepository
	mu      sync.Mutex
	jobs    map[string]*playgroundLoginJob
	// recover is replaceable for tests; production always uses the profile browser.
	recover       func(context.Context, string, string, string) (aistudio.StorageState, error)
	recoverWarm   func(context.Context, string, string, string) (aistudio.StorageState, error)
	warmup        *PlaygroundWarmupService
	cleanupCancel context.CancelFunc
	cleanupDone   chan struct{}
	batchClose    func()
}
type playgroundLoginJob struct {
	backend        string
	mu             sync.Mutex
	completed      bool
	browser        *camoufox.Browser
	profile, proxy string
	expires        time.Time
	cancel         context.CancelFunc
}

func NewPlaygroundBrowserService(cache *aistudio.SessionCache, proxies ProxyRepository) *PlaygroundBrowserService {
	root := os.Getenv("PLAYGROUND_PROFILE_DIR")
	if root == "" {
		base := os.Getenv("DATA_DIR")
		if base == "" {
			base = "data"
		}
		root = filepath.Join(base, "aistudio", "profiles")
	}
	manager := camoufox.NewManager(os.Getenv("CAMOUFOX_PATH"), root)
	return &PlaygroundBrowserService{Cache: cache, manager: manager, proxies: proxies, jobs: map[string]*playgroundLoginJob{}, recover: manager.Recover, recoverWarm: manager.RecoverWarm}
}
func (s *PlaygroundBrowserService) proxyURL(ctx context.Context, id *int64) (string, error) {
	if id == nil || *id == 0 {
		return "", nil
	}
	if s.proxies == nil {
		return "", errors.New("proxy repository unavailable")
	}
	proxy, err := s.proxies.GetByID(ctx, *id)
	if err != nil {
		return "", err
	}
	return proxy.URL(), nil
}
func (s *PlaygroundBrowserService) Begin(ctx context.Context, proxyID *int64) (string, error) {
	return s.BeginBackend(ctx, proxyID, "aistudio")
}
func (s *PlaygroundBrowserService) BeginBackend(ctx context.Context, proxyID *int64, backend string) (string, error) {
	backend, err := normalizePlaygroundBackend(backend)
	if err != nil {
		return "", err
	}
	proxy, err := s.proxyURL(ctx, proxyID)
	if err != nil {
		return "", err
	}
	profile := uuid.NewString()
	life, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	// Cancel startup when the HTTP caller disappears, then transfer lifetime to the job.
	stop := context.AfterFunc(ctx, cancel)
	start := s.manager.Start
	if backend == "gemini_web" {
		start = s.manager.StartGemini
	}
	browser, err := start(life, profile, proxy)
	stopped := stop()
	if err != nil || !stopped {
		cancel()
		if browser != nil {
			browser.Close()
		}
		if err != nil {
			return "", err
		}
		return "", ctx.Err()
	}
	job := &playgroundLoginJob{backend: backend, browser: browser, profile: profile, proxy: proxy, expires: time.Now().Add(10 * time.Minute), cancel: cancel}
	s.mu.Lock()
	s.jobs[profile] = job
	s.mu.Unlock()
	go func() { <-life.Done(); s.Cancel(profile) }()
	return profile, nil
}
func (s *PlaygroundBrowserService) job(id string) (*playgroundLoginJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[id]
	if job == nil || time.Now().After(job.expires) {
		return nil, errors.New("browser login expired; start a new login")
	}
	return job, nil
}
func (s *PlaygroundBrowserService) Frame(ctx context.Context, id string) (string, error) {
	job, err := s.job(id)
	if err != nil {
		return "", err
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	return job.browser.Screenshot(ctx)
}
func (s *PlaygroundBrowserService) Input(ctx context.Context, id string, input camoufox.Input) error {
	job, err := s.job(id)
	if err != nil {
		return err
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	return job.browser.Input(ctx, input)
}
func (s *PlaygroundBrowserService) Finish(ctx context.Context, id string) (string, string, error) {
	job, err := s.job(id)
	if err != nil {
		return "", "", err
	}
	job.mu.Lock()
	if job.completed {
		job.mu.Unlock()
		return "", "", errors.New("browser login already completed")
	}
	state, email, err := job.browser.Capture(ctx, "")
	if err != nil {
		job.mu.Unlock()
		return "", "", err
	}
	// Cache before closing. PostgreSQL receives only the one-time profile grant.
	lease, err := s.Cache.Acquire(ctx, job.profile)
	if err == nil {
		err = lease.Put(ctx, playgroundCacheKey(job.profile, job.proxy, email), state)
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		lease.Release(releaseCtx)
		cancel()
	}
	if err != nil {
		job.mu.Unlock()
		return "", "", err
	}
	token, err := s.Cache.IssueGrant(ctx, aistudio.BrowserGrant{Profile: job.profile, Email: email, Proxy: aistudio.ComputePayloadDigest(job.proxy), Backend: job.backend})
	if err == nil {
		job.completed = true
	}
	job.mu.Unlock()
	if err == nil {
		s.Cancel(id)
	}
	return token, email, err
}
func (s *PlaygroundBrowserService) Cancel(id string) {
	s.mu.Lock()
	job := s.jobs[id]
	delete(s.jobs, id)
	s.mu.Unlock()
	if job != nil {
		job.cancel()
		job.mu.Lock()
		job.browser.Close()
		job.mu.Unlock()
	}
}

func (s *PlaygroundBrowserService) Close() {
	if s.warmup != nil {
		s.warmup.Close()
	}
	if s.batchClose != nil {
		s.batchClose()
	}
	if s.cleanupCancel != nil {
		s.cleanupCancel()
		<-s.cleanupDone
	}
	s.manager.Close()
	s.mu.Lock()
	ids := make([]string, 0, len(s.jobs))
	for id := range s.jobs {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.Cancel(id)
	}
}

func (s *PlaygroundBrowserService) SetBatchCloser(close func()) { s.batchClose = close }
func playgroundCacheKey(profile, proxy, email string) string {
	return "playground:{" + profile + "}:session:" + aistudio.ComputePayloadDigest(proxy+"\n"+email)
}
