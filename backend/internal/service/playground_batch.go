package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	"github.com/Wei-Shaw/sub2api/internal/pkg/camoufox"
	"github.com/google/uuid"
)

type batchBrowser interface {
	camoufox.GoogleLoginBrowser
	Screenshot(context.Context) (string, error)
	Input(context.Context, camoufox.Input) error
	LoginURL(context.Context) (string, error)
	Close()
}
type batchRun struct {
	ctx      context.Context
	cancel   context.CancelFunc
	manual   bool
	attended bool
	browser  batchBrowser
	proxy    Proxy
	resume   chan struct{}
	opMu     sync.Mutex
}

type PlaygroundBatchService struct {
	mu                 sync.Mutex
	provisionMu        sync.Mutex
	state              *playgroundBatchState
	store              *playgroundBatchStore
	browser            *PlaygroundBrowserService
	admin              AdminService
	runs               map[string]*batchRun
	proxyBusy          map[int64]string
	cooldown           map[int64]time.Time
	cursor             int
	ctx                context.Context
	cancel             context.CancelFunc
	wake               chan struct{}
	wg                 sync.WaitGroup
	closed             bool
	closeOnce          sync.Once
	storageError       bool
	resourceError      string
	startBrowser       func(context.Context, string, string, bool, string) (batchBrowser, error)
	startGeminiBrowser func(context.Context, string, string, bool, string) (batchBrowser, error)
	checkProxy         func(context.Context, *Proxy) error
	createAccount      func(context.Context, *CreateAccountInput) (*Account, error)
	listAccounts       func(context.Context) ([]Account, error)
	ownerActive        func(context.Context, int64) bool
	capacity           func() camoufox.BrowserCapacity
}

func NewPlaygroundBatchService(browser *PlaygroundBrowserService, admin AdminService, cfg *config.Config) (*PlaygroundBatchService, error) {
	if browser == nil || browser.proxies == nil || admin == nil || cfg == nil || !cfg.Totp.EncryptionKeyConfigured {
		return nil, errors.New("批量上号服务需要固定的加密密钥与代理配置")
	}
	base := os.Getenv("DATA_DIR")
	if base == "" {
		base = "data"
	}
	store, err := openPlaygroundBatchStore(filepath.Join(base, "aistudio", "batches"), cfg.Totp.EncryptionKey)
	if err != nil {
		return nil, err
	}
	state, err := store.Load()
	if err != nil {
		store.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &PlaygroundBatchService{state: state, store: store, browser: browser, admin: admin, runs: map[string]*batchRun{}, proxyBusy: map[int64]string{}, cooldown: map[int64]time.Time{}, ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1)}
	s.startBrowser = func(ctx context.Context, profile, proxy string, manual bool, resume string) (batchBrowser, error) {
		if manual {
			return browser.manager.StartManualResume(ctx, profile, proxy, resume)
		}
		return browser.manager.StartBatch(ctx, profile, proxy)
	}
	s.checkProxy = checkPlaygroundBatchProxy
	s.startGeminiBrowser = func(ctx context.Context, profile, proxy string, manual bool, resume string) (batchBrowser, error) {
		if manual {
			return browser.manager.StartGeminiManualResume(ctx, profile, proxy, resume)
		}
		return browser.manager.StartGeminiBatch(ctx, profile, proxy)
	}
	s.capacity = browser.manager.Capacity
	s.createAccount = admin.CreateAccount
	s.ownerActive = func(ctx context.Context, id int64) bool {
		user, err := admin.GetUser(ctx, id)
		return err == nil && user != nil && user.IsActive() && user.IsAdmin()
	}
	s.listAccounts = func(ctx context.Context) ([]Account, error) {
		return admin.ListAccountsForSchedulerScoreFilter(ctx, PlatformGemini, AccountTypePlayground, "", "", 0, "")
	}
	if state.Concurrency < 1 || state.Concurrency > 128 {
		state.Concurrency = 2
	}
	for _, batch := range state.Batches {
		for _, job := range batch.Jobs {
			job.ManualRequested = false
			if job.Secret != nil && time.Now().After(job.SecretExpires) {
				job.Secret = nil
				if !batchTerminal(job.Status) {
					job.Status = "failed"
					job.Reason = "expired"
				}
			}
			if job.Status == "running" {
				job.Status = "queued"
				job.Reason = "interrupted"
			}
			if batchTerminal(job.Status) {
				job.Secret = nil
				job.LastFrame = ""
			}
			if job.Progress.Submitted == nil {
				job.Progress.Submitted = map[string]time.Time{}
			}
			if job.Status == "queued" && !batchSubmitted(job.Progress) && (job.Reason == "proxy_unavailable" || job.Reason == "network_error") {
				s.queueCleanupLocked(job)
				job.Profile = uuid.NewString()
				job.ProxyID = 0
				job.Progress.Submitted = map[string]time.Time{}
			}
		}
	}
	if err = s.saveLocked(); err != nil {
		store.Close()
		cancel()
		return nil, err
	}
	browser.manager.SetConcurrency(state.Concurrency)
	s.wg.Add(1)
	go s.loop()
	return s, nil
}

func (s *PlaygroundBatchService) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *PlaygroundBatchService) saveLocked() error {
	if err := s.store.Save(s.state); err != nil {
		s.storageError = true
		return errors.New("任务保存失败，自动执行已暂停")
	}
	s.storageError = false
	return nil
}

func (s *PlaygroundBatchService) queueCleanupLocked(job *playgroundBatchJob) {
	if job.AccountID != 0 || job.Profile == "" {
		return
	}
	job.ProxyDigest = ""
	for _, id := range s.state.Cleanup {
		if id == job.Profile {
			job.Profile = ""
			return
		}
	}
	s.state.Cleanup = append(s.state.Cleanup, job.Profile)
	job.Profile = ""
	job.ResumeURL = ""
}

func (s *PlaygroundBatchService) flushCleanupLocked() {
	if s.storageError || len(s.state.Cleanup) == 0 {
		return
	}
	left := []string{}
	for _, profile := range s.state.Cleanup {
		if s.browser.prepareProfileCleanup(profile, 0) != nil {
			left = append(left, profile)
		}
	}
	if len(left) != len(s.state.Cleanup) {
		s.state.Cleanup = left
		_ = s.saveLocked()
	}
}
func (s *PlaygroundBatchService) findLocked(owner int64, batchID, jobID string) (*playgroundBatch, *playgroundBatchJob, error) {
	for _, b := range s.state.Batches {
		if b.ID == batchID && b.Owner == owner {
			if jobID == "" {
				return b, nil, nil
			}
			for _, j := range b.Jobs {
				if j.ID == jobID {
					return b, j, nil
				}
			}
		}
	}
	return nil, nil, errors.New("任务不存在或不属于当前管理员")
}
func (s *PlaygroundBatchService) jobLocked(batchID, jobID string) (*playgroundBatch, *playgroundBatchJob) {
	for _, b := range s.state.Batches {
		if b.ID == batchID {
			for _, j := range b.Jobs {
				if j.ID == jobID {
					return b, j
				}
			}
		}
	}
	return nil, nil
}

func (s *PlaygroundBatchService) Create(ctx context.Context, owner int64, request PlaygroundBatchRequest) (string, error) {
	backend, err := normalizePlaygroundBackend(request.Backend)
	if err != nil {
		return "", err
	}
	if owner <= 0 {
		return "", errors.New("需要管理员登录")
	}
	if request.Concurrency < 1 || request.Concurrency > 128 {
		return "", errors.New("并发须在 1 到 128 之间")
	}
	parsed, err := parsePlaygroundBatch(request.Accounts)
	if err != nil {
		return "", err
	}
	if len(request.ProxyIDs) == 0 || len(request.ProxyIDs) > 1000 {
		return "", errors.New("请选择代理池；批量上号不允许直连")
	}
	seenProxy := map[int64]bool{}
	ids := []int64{}
	for _, id := range request.ProxyIDs {
		if seenProxy[id] {
			continue
		}
		seenProxy[id] = true
		p, e := s.browser.proxies.GetByID(ctx, id)
		if e != nil || !eligibleBatchProxy(p) {
			return "", errors.New("所选代理已停用、过期或不可用")
		}
		ids = append(ids, id)
	}
	if err = s.admin.ValidateAccountGroupBindings(ctx, request.GroupIDs); err != nil {
		return "", errors.New("所选分组不可用")
	}
	accounts, err := s.listAccounts(ctx)
	if err != nil {
		return "", errors.New("无法核对现有账号，请稍后再试")
	}
	existing := map[string]int64{}
	for _, a := range accounts {
		accountBackend, _ := normalizePlaygroundBackend(a.GetCredential("playground_backend"))
		if accountBackend != backend {
			continue
		}
		email := a.GetCredential("playground_email")
		if email != "" {
			existing[camoufox.CanonicalGoogleEmail(email)] = a.ID
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", errors.New("服务正在停止")
	}
	active := map[string]bool{}
	pending := 0
	stored := 0
	for _, b := range s.state.Batches {
		stored += len(b.Jobs)
		for _, j := range b.Jobs {
			if j.Status == "queued" || j.Status == "running" || j.Status == "manual" {
				batchBackend, _ := normalizePlaygroundBackend(b.Backend)
				if batchBackend == backend {
					active[camoufox.CanonicalGoogleEmail(j.Email)] = true
				}
				pending++
			}
		}
	}
	if pending+len(parsed) > 5000 || stored+len(parsed) > 20000 || len(s.state.Batches) >= 100 {
		return "", errors.New("待处理任务较多，请先完成或删除历史批次")
	}
	batch := &playgroundBatch{Backend: backend, ID: uuid.NewString(), Owner: owner, ProxyIDs: ids, GroupIDs: append([]int64(nil), request.GroupIDs...), AutoOnboarding: request.AutoOnboarding, Created: time.Now(), Jobs: []*playgroundBatchJob{}}
	for _, row := range parsed {
		secret := row.Secret
		job := &playgroundBatchJob{ID: uuid.NewString(), Email: row.Email, Status: "queued", Step: "queued", Secret: &secret, SecretExpires: time.Now().Add(24 * time.Hour), Profile: uuid.NewString(), Updated: time.Now(), Progress: camoufox.GoogleLoginProgress{Submitted: map[string]time.Time{}}}
		key := camoufox.CanonicalGoogleEmail(row.Email)
		if id := existing[key]; id > 0 {
			job.Status = "skipped"
			job.Reason = "already_exists"
			job.AccountID = id
			job.Secret = nil
		} else if active[key] {
			job.Status = "skipped"
			job.Reason = "active_duplicate"
			job.Secret = nil
		}
		batch.Jobs = append(batch.Jobs, job)
	}
	oldConcurrency := s.state.Concurrency
	s.state.Batches = append(s.state.Batches, batch)
	s.state.Concurrency = request.Concurrency
	if err = s.saveLocked(); err != nil {
		s.state.Batches = s.state.Batches[:len(s.state.Batches)-1]
		s.state.Concurrency = oldConcurrency
		return "", err
	}
	s.browser.manager.SetConcurrency(request.Concurrency)
	s.notify()
	return batch.ID, nil
}

func (s *PlaygroundBatchService) View(owner int64, selected string) PlaygroundBatchViewState {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := PlaygroundBatchViewState{Concurrency: s.state.Concurrency, Capacity: s.capacity(), Batches: []PlaygroundBatchView{}}
	if s.storageError {
		result.Error = batchReason("storage_error")
	} else {
		result.Error = s.resourceError
	}
	if selected == "" {
		for i := len(s.state.Batches) - 1; i >= 0; i-- {
			if s.state.Batches[i].Owner == owner {
				selected = s.state.Batches[i].ID
				break
			}
		}
	}
	for _, b := range s.state.Batches {
		if b.Owner != owner {
			continue
		}
		view := PlaygroundBatchView{Backend: b.Backend, ID: b.ID, Paused: b.Paused, Created: b.Created, ProxyIDs: append([]int64(nil), b.ProxyIDs...), GroupIDs: append([]int64(nil), b.GroupIDs...), AutoOnboarding: b.AutoOnboarding, Counts: map[string]int{}, Jobs: []PlaygroundBatchJobView{}}
		for _, j := range b.Jobs {
			r := s.runs[j.ID]
			open := r != nil && r.manual && r.browser != nil
			opening := j.ManualRequested || (r != nil && r.manual && r.browser == nil)
			view.Counts[j.Status]++
			if r != nil && r.browser != nil {
				result.Running++
			}
			if b.ID == selected {
				view.Jobs = append(view.Jobs, PlaygroundBatchJobView{ID: j.ID, Email: j.Email, Status: j.Status, Step: j.Step, Reason: batchReason(j.Reason), ProxyID: j.ProxyID, ProxyName: j.ProxyName, Attempts: j.Attempts, AccountID: j.AccountID, HasCredentials: j.Secret != nil, ManualOpen: open, Opening: opening, Updated: j.Updated})
			}
		}
		result.Batches = append(result.Batches, view)
	}
	sort.Slice(result.Batches, func(i, j int) bool { return result.Batches[i].Created.After(result.Batches[j].Created) })
	return result
}

func (s *PlaygroundBatchService) Configure(owner int64, concurrency int) error {
	if owner <= 0 || concurrency < 1 || concurrency > 128 {
		return errors.New("并发须在 1 到 128 之间")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("服务正在停止")
	}
	previous := s.state.Concurrency
	s.state.Concurrency = concurrency
	if err := s.saveLocked(); err != nil {
		s.state.Concurrency = previous
		return err
	}
	s.browser.manager.SetConcurrency(concurrency)
	s.notify()
	return nil
}

func (s *PlaygroundBatchService) Action(owner int64, batchID, jobID, action string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, j, err := s.findLocked(owner, batchID, jobID)
	if err != nil {
		return err
	}
	if s.closed {
		return errors.New("服务正在停止")
	}
	if j == nil {
		switch action {
		case "pause":
			b.Paused = true
		case "resume":
			b.Paused = false
		case "cancel":
			for _, job := range b.Jobs {
				if !batchTerminal(job.Status) {
					job.Status = "cancelled"
					job.Step = "cancelled"
					job.Reason = "cancelled"
					job.Secret = nil
					job.LastFrame = ""
					s.store.RemoveFrame(job.ID)
					job.ManualRequested = false
					if r := s.runs[job.ID]; r != nil {
						r.cancel()
					}
				}
			}
		case "delete":
			for _, job := range b.Jobs {
				if s.runs[job.ID] != nil {
					return errors.New("请先取消并等待运行中的任务结束")
				}
			}
			for _, job := range b.Jobs {
				s.queueCleanupLocked(job)
				s.store.RemoveFrame(job.ID)
			}
			for i, item := range s.state.Batches {
				if item.ID == b.ID {
					s.state.Batches = append(s.state.Batches[:i], s.state.Batches[i+1:]...)
					break
				}
			}
		default:
			return errors.New("不支持的批次操作")
		}
	} else {
		switch action {
		case "cancel":
			if !batchTerminal(j.Status) {
				j.Status = "cancelled"
				j.Reason = "cancelled"
				j.Secret = nil
				j.LastFrame = ""
				s.store.RemoveFrame(j.ID)
				j.ManualRequested = false
				if r := s.runs[j.ID]; r != nil {
					r.cancel()
				}
			}
		case "retry":
			if s.runs[j.ID] != nil || batchTerminal(j.Status) {
				return errors.New("该任务当前不能重试")
			}
			if j.Secret == nil || time.Now().After(j.SecretExpires) {
				return errors.New(batchReason("credentials_required"))
			}
			j.Status = "queued"
			if j.Profile == "" || j.Reason == "expired" {
				j.Profile = uuid.NewString()
			}
			j.Reason = ""
			j.Step = "queued"
			j.Attempts = 0
			j.NextTry = time.Time{}
			j.Progress.Submitted = map[string]time.Time{}
			j.LastFrame = ""
		case "open_manual":
			if batchTerminal(j.Status) || s.runs[j.ID] != nil {
				return errors.New("该任务当前不能打开人工窗口")
			}
			if j.ProxyID == 0 {
				return errors.New("请先重试并分配一个有效代理")
			}
			if j.Profile == "" || j.Reason == "expired" {
				return errors.New("任务资料已清理，请重新导入账号")
			}
			j.Status = "manual"
			j.ManualRequested = true
			j.Step = "manual_opening"
		case "close_manual":
			j.ManualRequested = false
			if r := s.runs[j.ID]; r != nil {
				r.attended = false
				if r.manual {
					r.cancel()
				}
			}
		case "continue_auto":
			r := s.runs[j.ID]
			if r == nil || !r.manual || r.browser == nil {
				return errors.New("请先打开人工处理窗口")
			}
			if j.Secret == nil {
				return errors.New(batchReason("credentials_required"))
			}
			r.manual = false
			j.Progress.Submitted = map[string]time.Time{}
			j.Attempts++
			j.Status = "running"
			j.Step = "resuming"
			select {
			case r.resume <- struct{}{}:
			default:
			}
		default:
			return errors.New("不支持的任务操作")
		}
		j.Updated = time.Now()
	}
	if err = s.saveLocked(); err != nil {
		return err
	}
	s.flushCleanupLocked()
	s.notify()
	return nil
}

func eligibleBatchProxy(p *Proxy) bool {
	return p != nil && p.IsActive() && !p.IsExpired(time.Now()) && (p.Protocol == "http" || p.Protocol == "https" || p.Protocol == "socks5" || p.Protocol == "socks5h")
}

func (s *PlaygroundBatchService) loop() {
	defer s.wg.Done()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.wake:
		case <-tick.C:
		}
		ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
		proxies, err := s.browser.proxies.ListActive(ctx)
		cancel()
		available := map[int64]Proxy{}
		for _, p := range proxies {
			if eligibleBatchProxy(&p) {
				available[p.ID] = p
			}
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			continue
		}
		if s.storageError {
			if s.saveLocked() != nil {
				s.mu.Unlock()
				continue
			}
		}
		changed := false
		for _, b := range s.state.Batches {
			for _, j := range b.Jobs {
				if j.Secret != nil && time.Now().After(j.SecretExpires) {
					j.Secret = nil
					if run := s.runs[j.ID]; run != nil && !run.manual {
						j.Status = "failed"
						j.Step = "failed"
						j.Reason = "expired"
						run.cancel()
					}
					if !batchTerminal(j.Status) && s.runs[j.ID] == nil {
						j.Status = "failed"
						j.Reason = "expired"
					}
					changed = true
				}
				if s.runs[j.ID] == nil && j.AccountID == 0 && j.Profile != "" && (j.Status == "cancelled" || j.Reason == "expired") {
					s.queueCleanupLocked(j)
					j.LastFrame = ""
					s.store.RemoveFrame(j.ID)
					changed = true
				}
			}
		}
		if changed {
			if s.saveLocked() != nil {
				s.mu.Unlock()
				continue
			}
		}
		s.flushCleanupLocked()
		if s.storageError {
			s.mu.Unlock()
			continue
		}
		if err != nil {
			s.resourceError = "暂时无法读取代理池，等待恢复"
			if changed {
				_ = s.saveLocked()
			}
			s.mu.Unlock()
			continue
		}
		if free := playgroundBatchDiskFree(s.store.directory); free > 0 && free < 512<<20 {
			s.resourceError = "磁盘空间不足，已暂停新的上号任务"
			if changed {
				_ = s.saveLocked()
			}
			s.mu.Unlock()
			continue
		}
		s.resourceError = ""
		cap := s.capacity()
		live := 0
		for _, r := range s.runs {
			if r.browser != nil {
				live++
			}
		}
		core := max(0, cap.Active-live)
		limit := min(s.state.Concurrency, max(0, cap.Effective-core))
		// A human request is dispatched before the next background account.
		for _, b := range s.state.Batches {
			for _, j := range b.Jobs {
				if len(s.runs) >= max(1, limit) {
					break
				}
				if !j.ManualRequested || s.runs[j.ID] != nil {
					continue
				}
				p, ok := available[j.ProxyID]
				if !ok || (j.ProxyDigest != "" && j.ProxyDigest != aistudio.ComputePayloadDigest(p.URL())) {
					j.ManualRequested = false
					j.Reason = "proxy_unavailable"
					changed = true
					continue
				}
				if s.proxyBusy[p.ID] != "" {
					continue
				}
				s.dispatchLocked(b, j, p, true)
				changed = true
			}
		}
		for _, b := range s.state.Batches {
			if b.Paused {
				continue
			}
			for _, j := range b.Jobs {
				if len(s.runs) >= limit {
					break
				}
				if j.Status != "queued" || s.runs[j.ID] != nil || j.Secret == nil || time.Now().Before(j.NextTry) {
					continue
				}
				var selected *Proxy
				for n := 0; n < len(b.ProxyIDs); n++ {
					index := (s.cursor + n) % len(b.ProxyIDs)
					id := b.ProxyIDs[index]
					p, ok := available[id]
					if !ok || s.proxyBusy[id] != "" || time.Now().Before(s.cooldown[id]) {
						continue
					}
					if (j.ProxyDigest != "" || batchSubmitted(j.Progress)) && j.ProxyID != 0 && id != j.ProxyID {
						continue
					}
					if j.ProxyDigest != "" && j.ProxyDigest != aistudio.ComputePayloadDigest(p.URL()) {
						continue
					}
					selected = &p
					s.cursor = (index + 1) % len(b.ProxyIDs)
					break
				}
				if selected == nil {
					if j.Reason != "waiting_proxy" {
						j.Reason = "waiting_proxy"
						changed = true
					}
					continue
				}
				s.dispatchLocked(b, j, *selected, false)
				changed = true
			}
		}
		if changed {
			_ = s.saveLocked()
		}
		s.mu.Unlock()
	}
}

func (s *PlaygroundBatchService) dispatchLocked(batch *playgroundBatch, job *playgroundBatchJob, proxy Proxy, manual bool) {
	lifetime := 6 * time.Minute
	if manual {
		lifetime = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(s.ctx, lifetime)
	if !manual && !job.SecretExpires.IsZero() && time.Until(job.SecretExpires) < lifetime {
		cancel()
		ctx, cancel = context.WithDeadline(s.ctx, job.SecretExpires)
	}
	r := &batchRun{ctx: ctx, cancel: cancel, manual: manual, attended: manual, proxy: proxy, resume: make(chan struct{}, 1)}
	s.runs[job.ID] = r
	s.proxyBusy[proxy.ID] = job.ID
	job.ProxyID = proxy.ID
	job.ProxyDigest = aistudio.ComputePayloadDigest(proxy.URL())
	job.ProxyName = proxy.Name
	job.ManualRequested = false
	job.Updated = time.Now()
	if manual {
		job.Step = "manual_opening"
	} else {
		job.Status = "running"
		job.Step = "checking_proxy"
		job.Reason = ""
		job.Attempts++
	}
	// Persist ownership before the worker may send a password.
	if s.saveLocked() != nil {
		delete(s.runs, job.ID)
		delete(s.proxyBusy, proxy.ID)
		cancel()
		job.Status = "queued"
		return
	}
	s.wg.Add(1)
	go s.runJob(batch.ID, job.ID, r)
}

func batchSubmitted(progress camoufox.GoogleLoginProgress) bool {
	for _, k := range []string{"email", "password", "totp"} {
		if _, ok := progress.Submitted[k]; ok {
			return true
		}
	}
	return false
}

func (s *PlaygroundBatchService) Close() {
	s.closeOnce.Do(func() {
		s.cancel()
		s.mu.Lock()
		s.closed = true
		for _, b := range s.state.Batches {
			for _, j := range b.Jobs {
				j.ManualRequested = false
				if j.Status == "running" {
					j.Status = "queued"
					j.Reason = "interrupted"
				}
			}
		}
		for _, r := range s.runs {
			r.cancel()
		}
		_ = s.saveLocked()
		s.mu.Unlock()
		done := make(chan struct{})
		go func() { s.wg.Wait(); s.store.Close(); close(done) }()
		select {
		case <-done:
		case <-time.After(8 * time.Second):
		}
	})
}
