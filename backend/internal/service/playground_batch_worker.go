package service

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio/proxydial"
	"github.com/Wei-Shaw/sub2api/internal/pkg/camoufox"
	"github.com/google/uuid"
)

func checkPlaygroundBatchProxy(ctx context.Context, p *Proxy) error {
	if !eligibleBatchProxy(p) {
		return errors.New("proxy required")
	}
	u, err := url.Parse(p.URL())
	if err != nil {
		return errors.New("proxy invalid")
	}
	dialer, err := proxydial.Dialer(u)
	if err != nil {
		return errors.New("proxy invalid")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	connection, err := dialer.DialContext(ctx, "tcp", "accounts.google.com:443")
	if err != nil {
		return errors.New("proxy unavailable")
	}
	defer connection.Close()
	secure := tls.Client(connection, &tls.Config{ServerName: "accounts.google.com", MinVersion: tls.VersionTLS12})
	if secure.HandshakeContext(ctx) != nil {
		return errors.New("proxy TLS connection unavailable")
	}
	return nil
}

func batchProxyLabel(p Proxy) string {
	return p.Protocol + "://" + net.JoinHostPort(p.Host, strconv.Itoa(p.Port))
}

func (s *PlaygroundBatchService) checkpoint(batchID, jobID string, r *batchRun, step string, progress camoufox.GoogleLoginProgress) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, j := s.jobLocked(batchID, jobID)
	if j == nil || s.runs[jobID] != r || s.closed || j.Status == "cancelled" {
		return context.Canceled
	}
	j.Step = step
	j.Updated = time.Now()
	j.Progress.Submitted = map[string]time.Time{}
	for k, v := range progress.Submitted {
		j.Progress.Submitted[k] = v
	}
	return s.saveLocked()
}

func (s *PlaygroundBatchService) existingAccount(ctx context.Context, email, jobID string, backends ...string) (*Account, error) {
	backend := "aistudio"
	if len(backends) > 0 {
		backend, _ = normalizePlaygroundBackend(backends[0])
	}
	accounts, err := s.listAccounts(ctx)
	if err != nil {
		return nil, err
	}
	for i := range accounts {
		a := &accounts[i]
		accountBackend, _ := normalizePlaygroundBackend(a.GetCredential("playground_backend"))
		if accountBackend != backend {
			continue
		}
		marker, _ := a.Extra["playground_batch_job_id"].(string)
		if marker == jobID || (a.GetCredential("playground_email") != "" && camoufox.CanonicalGoogleEmail(a.GetCredential("playground_email")) == camoufox.CanonicalGoogleEmail(email)) {
			return a, nil
		}
	}
	return nil, nil
}

func (s *PlaygroundBatchService) recordAccount(batchID, jobID string, account *Account, skipped bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, j := s.jobLocked(batchID, jobID)
	if j == nil {
		return
	}
	j.Status = "succeeded"
	j.Reason = "account_saved"
	if skipped {
		j.Status = "skipped"
		j.Reason = "already_exists"
	}
	j.AccountID = account.ID
	j.Step = j.Status
	j.Secret = nil
	j.LastFrame = ""
	j.ResumeURL = ""
	j.ManualRequested = false
	j.Updated = time.Now()
	s.store.RemoveFrame(j.ID)
	_ = s.saveLocked()
}

func (s *PlaygroundBatchService) park(batchID, jobID string, r *batchRun, manual bool, reason string) {
	s.mu.Lock()
	_, initial := s.jobLocked(batchID, jobID)
	allowed := initial != nil && s.runs[jobID] == r && !batchTerminal(initial.Status) && initial.Reason != "expired" && !s.closed
	s.mu.Unlock()
	if !allowed {
		return
	}
	frame := ""
	resumeURL := ""
	if manual && r.browser != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		resumeURL, _ = r.browser.LoginURL(ctx)
		frame, _ = r.browser.Screenshot(ctx)
		cancel()
		if frame != "" && s.store.SaveFrame(jobID, frame) != nil {
			frame = ""
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, j := s.jobLocked(batchID, jobID)
	if j == nil || s.runs[jobID] != r || batchTerminal(j.Status) || j.Reason == "expired" || s.closed {
		if frame != "" {
			s.store.RemoveFrame(jobID)
		}
		return
	}
	j.Status = "failed"
	if manual {
		j.Status = "manual"
	}
	j.Step = j.Status
	j.Reason = reason
	if manual {
		j.ResumeURL = resumeURL
	}
	j.Updated = time.Now()
	if frame != "" {
		j.LastFrame = "available"
	}
	if reason == "proxy_unavailable" || reason == "network_error" {
		s.cooldown[r.proxy.ID] = time.Now().Add(2 * time.Minute)
		if !manual && j.Attempts < 2 && !batchSubmitted(j.Progress) && j.Secret != nil {
			j.Status = "queued"
			j.Step = "queued"
			j.NextTry = time.Now().Add(5 * time.Second)
		}
	}
	_ = s.saveLocked()
}

func (s *PlaygroundBatchService) runJob(batchID, jobID string, r *batchRun) {
	defer s.wg.Done()
	defer func() {
		r.cancel()
		// A manual save must finish (or observe cancellation) before the profile
		// can become eligible for cleanup and the browser can be closed.
		r.opMu.Lock()
		defer r.opMu.Unlock()
		if r.browser != nil {
			r.browser.Close()
		}
		s.mu.Lock()
		if s.runs[jobID] == r {
			_, job := s.jobLocked(batchID, jobID)
			if job != nil && !s.closed && job.Status == "queued" && !batchSubmitted(job.Progress) && (job.Reason == "proxy_unavailable" || job.Reason == "network_error") {
				s.queueCleanupLocked(job)
				job.Profile = uuid.NewString()
				job.ProxyID = 0
				job.Progress.Submitted = map[string]time.Time{}
				_ = s.saveLocked()
			}
			delete(s.runs, jobID)
			if s.proxyBusy[r.proxy.ID] == jobID {
				delete(s.proxyBusy, r.proxy.ID)
			}
		}
		s.mu.Unlock()
		s.notify()
	}()
	s.mu.Lock()
	b, j := s.jobLocked(batchID, jobID)
	if b == nil || j == nil {
		s.mu.Unlock()
		return
	}
	profile, email, owner, resumeURL := j.Profile, j.Email, b.Owner, j.ResumeURL
	backend, _ := normalizePlaygroundBackend(b.Backend)
	if backend == "gemini_web" && (!r.manual || resumeURL == "") {
		resumeURL = camoufox.GeminiOrigin + "/app"
	}
	manual := r.manual
	j.ProxyName = batchProxyLabel(r.proxy)
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.ctx, 15*time.Second)
	if s.ownerActive != nil && !s.ownerActive(ctx, owner) {
		cancel()
		s.park(batchID, jobID, r, false, "owner_inactive")
		return
	}
	existing, err := s.existingAccount(ctx, email, jobID, backend)
	cancel()
	if err != nil {
		s.park(batchID, jobID, r, false, "account_save_failed")
		return
	}
	if existing != nil {
		marker, _ := existing.Extra["playground_batch_job_id"].(string)
		s.recordAccount(batchID, jobID, existing, marker != jobID)
		return
	}
	if s.checkProxy(r.ctx, &r.proxy) != nil {
		s.park(batchID, jobID, r, false, "proxy_unavailable")
		return
	}
	if err = r.ctx.Err(); err != nil {
		return
	}
	s.mu.Lock()
	_, j = s.jobLocked(batchID, jobID)
	if j != nil {
		j.Step = "opening_browser"
		_ = s.saveLocked()
	}
	s.mu.Unlock()
	start := s.startBrowser
	if backend == "gemini_web" && s.startGeminiBrowser != nil {
		start = s.startGeminiBrowser
	}
	browser, err := start(r.ctx, profile, r.proxy.URL(), manual, resumeURL)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			s.park(batchID, jobID, r, true, "timeout")
		} else {
			s.park(batchID, jobID, r, false, "network_error")
		}
		return
	}
	s.mu.Lock()
	r.browser = browser
	_, j = s.jobLocked(batchID, jobID)
	if j != nil {
		j.Step = "browser_ready"
		_ = s.saveLocked()
	}
	s.mu.Unlock()
	for {
		if manual {
			select {
			case <-r.ctx.Done():
				s.park(batchID, jobID, r, true, "manual_closed")
				return
			case <-r.resume:
			}
		}
		result, err := s.automateRun(batchID, jobID, r, browser)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				s.park(batchID, jobID, r, true, "timeout")
			} else if !errors.Is(err, context.Canceled) {
				s.park(batchID, jobID, r, false, "storage_error")
			}
			return
		}
		if result.Email != "" {
			if s.provision(r.ctx, batchID, jobID, r, result.State, result.Email) != nil {
				s.park(batchID, jobID, r, false, "account_save_failed")
			}
			return
		}
		s.park(batchID, jobID, r, result.Manual, result.Reason)
		s.mu.Lock()
		_, current := s.jobLocked(batchID, jobID)
		keep := result.Manual && r.attended && current != nil && current.Status == "manual" && r.ctx.Err() == nil
		if keep {
			r.manual = true
			manual = true
		}
		s.mu.Unlock()
		if !keep {
			return
		}
	}
}

func (s *PlaygroundBatchService) automateRun(batchID, jobID string, r *batchRun, browser batchBrowser) (camoufox.GoogleLoginResult, error) {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	s.mu.Lock()
	b, j := s.jobLocked(batchID, jobID)
	if b == nil || j == nil || j.Secret == nil || batchTerminal(j.Status) {
		s.mu.Unlock()
		return camoufox.GoogleLoginResult{}, context.Canceled
	}
	credentials := camoufox.GoogleLoginCredentials{Email: j.Email, Password: j.Secret.Password, TOTP: j.Secret.TOTP}
	progress := camoufox.GoogleLoginProgress{Submitted: map[string]time.Time{}}
	for k, v := range j.Progress.Submitted {
		progress.Submitted[k] = v
	}
	autoOnboarding := b.AutoOnboarding
	s.mu.Unlock()
	result, err := camoufox.AutomateGoogleLogin(r.ctx, browser, credentials, progress, autoOnboarding, func(step string, p camoufox.GoogleLoginProgress) error {
		return s.checkpoint(batchID, jobID, r, step, p)
	})
	credentials.Password = ""
	credentials.TOTP = ""
	return result, err
}

func (s *PlaygroundBatchService) provision(ctx context.Context, batchID, jobID string, r *batchRun, state aistudio.StorageState, email string) error {
	s.provisionMu.Lock()
	defer s.provisionMu.Unlock()
	s.mu.Lock()
	b, j := s.jobLocked(batchID, jobID)
	if b == nil || j == nil || s.closed || j.Status == "cancelled" {
		s.mu.Unlock()
		return context.Canceled
	}
	profile, expected, owner := j.Profile, j.Email, b.Owner
	backend, _ := normalizePlaygroundBackend(b.Backend)
	groups := append([]int64(nil), b.GroupIDs...)
	j.Step = "saving_account"
	saveErr := s.saveLocked()
	s.mu.Unlock()
	if saveErr != nil {
		return saveErr
	}
	if s.ownerActive != nil && !s.ownerActive(ctx, owner) {
		return errors.New("administrator no longer active")
	}
	if camoufox.CanonicalGoogleEmail(email) != camoufox.CanonicalGoogleEmail(expected) {
		return errors.New("account identity mismatch")
	}
	currentProxy, proxyErr := s.browser.proxies.GetByID(ctx, r.proxy.ID)
	if proxyErr != nil || !eligibleBatchProxy(currentProxy) || currentProxy.URL() != r.proxy.URL() {
		return errors.New("proxy changed during login")
	}
	existing, err := s.existingAccount(ctx, email, jobID, backend)
	if err != nil {
		return err
	}
	if existing != nil {
		marker, _ := existing.Extra["playground_batch_job_id"].(string)
		s.recordAccount(batchID, jobID, existing, marker != jobID)
		return nil
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	lease, err := s.browser.Cache.Acquire(ctx, profile)
	if err != nil {
		return err
	}
	ctx, cancelLease := context.WithCancel(ctx)
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(8 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				renewCtx, stop := context.WithTimeout(ctx, 5*time.Second)
				err := lease.Renew(renewCtx)
				stop()
				if err != nil {
					cancelLease()
					return
				}
			}
		}
	}()
	releaseLease := func() {
		cancelLease()
		<-heartbeatDone
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		lease.Release(releaseCtx)
	}
	// Keep cleanup excluded until commit and lost-response reconciliation finish.
	defer releaseLease()
	err = lease.Put(ctx, playgroundCacheKey(profile, r.proxy.URL(), email), state)
	if err != nil {
		return err
	}
	token, err := s.browser.Cache.IssueGrant(ctx, aistudio.BrowserGrant{Profile: profile, Email: email, Proxy: aistudio.ComputePayloadDigest(r.proxy.URL()), Backend: backend})
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	proxyID := r.proxy.ID
	account, err := s.createAccount(ctx, &CreateAccountInput{Name: email, Platform: PlatformGemini, Type: AccountTypePlayground, Credentials: map[string]any{"playground_login_token": token, "playground_require_proxy": true, "playground_backend": backend}, Extra: map[string]any{"playground_batch_job_id": jobID}, ProxyID: &proxyID, Concurrency: 1, Priority: 0, GroupIDs: groups})
	if err != nil {
		// A committed account followed by a lost response/group-bind failure must
		// not be created a second time on retry or after process restart.
		lookup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if saved, e := s.existingAccount(lookup, email, jobID, backend); e == nil && saved != nil {
			marker, _ := saved.Extra["playground_batch_job_id"].(string)
			s.recordAccount(batchID, jobID, saved, marker != jobID)
			if marker == jobID && len(groups) > 0 {
				s.mu.Lock()
				_, job := s.jobLocked(batchID, jobID)
				if job != nil {
					job.Reason = "account_saved_check_groups"
					_ = s.saveLocked()
				}
				s.mu.Unlock()
			}
			return nil
		}
		return errors.New("account save failed")
	}
	s.recordAccount(batchID, jobID, account, false)
	return nil
}

func (s *PlaygroundBatchService) ManualFrame(ctx context.Context, owner int64, batchID, jobID string) (string, bool, error) {
	s.mu.Lock()
	_, j, err := s.findLocked(owner, batchID, jobID)
	if err != nil {
		s.mu.Unlock()
		return "", false, err
	}
	r := s.runs[jobID]
	frame := j.LastFrame
	var browser batchBrowser
	if r != nil && r.manual {
		browser = r.browser
	}
	s.mu.Unlock()
	if browser != nil {
		if !r.opMu.TryLock() {
			return "", false, errors.New("浏览器正忙，请稍后重试")
		}
		defer r.opMu.Unlock()
		if !s.manualRunCurrent(jobID, r) {
			return "", false, errors.New("人工窗口已结束")
		}
		image, err := browser.Screenshot(ctx)
		return image, true, err
	}
	if frame != "" {
		image, err := s.store.LoadFrame(jobID)
		return image, false, err
	}
	return "", false, errors.New("浏览器尚未就绪，请等待或打开人工窗口")
}
func (s *PlaygroundBatchService) ManualInput(ctx context.Context, owner int64, batchID, jobID string, input camoufox.Input) error {
	s.mu.Lock()
	_, _, err := s.findLocked(owner, batchID, jobID)
	r := s.runs[jobID]
	var browser batchBrowser
	if r != nil && r.manual {
		browser = r.browser
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if browser == nil {
		return errors.New("人工窗口尚未打开")
	}
	r.opMu.Lock()
	defer r.opMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.manualRunCurrent(jobID, r) {
		return errors.New("人工窗口已结束")
	}
	return browser.Input(ctx, input)
}
func (s *PlaygroundBatchService) ManualFinish(ctx context.Context, owner int64, batchID, jobID string) error {
	s.mu.Lock()
	_, j, err := s.findLocked(owner, batchID, jobID)
	r := s.runs[jobID]
	if err != nil {
		s.mu.Unlock()
		return err
	}
	email := j.Email
	var browser batchBrowser
	if r != nil && r.manual {
		browser = r.browser
	}
	s.mu.Unlock()
	if browser == nil {
		return errors.New("人工窗口尚未打开")
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.ctx, cancel)
	defer stop()
	defer cancel()
	r.opMu.Lock()
	defer r.opMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.manualRunCurrent(jobID, r) {
		return errors.New("人工窗口已结束")
	}
	page, pageErr := browser.LoginPage(ctx, email)
	if pageErr != nil || page.Origin != aistudio.AIStudioOrigin || page.Phase != "studio" {
		return errors.New("请完成当前验证或引导，并进入 AI Studio 后再保存")
	}
	state, actual, err := browser.Capture(ctx, "")
	if err != nil {
		return errors.New("请完成 Google 登录并进入 AI Studio 后再保存")
	}
	if camoufox.CanonicalGoogleEmail(actual) != camoufox.CanonicalGoogleEmail(email) {
		return errors.New(batchReason("identity_mismatch"))
	}
	if err = s.provision(ctx, batchID, jobID, r, state, actual); err != nil {
		return errors.New("账号保存失败，请稍后重试")
	}
	r.cancel()
	return nil
}

func (s *PlaygroundBatchService) manualRunCurrent(jobID string, r *batchRun) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs[jobID] == r && r.manual && r.ctx.Err() == nil
}
