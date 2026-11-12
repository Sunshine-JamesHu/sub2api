package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	"github.com/Wei-Shaw/sub2api/internal/pkg/camoufox"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

var errPlaygroundSessionBusy = errors.New("Playground browser profile is busy; retry another account")
var errPlaygroundAuthorizationMissing = fmt.Errorf("%w: this account must be authorized with Camoufox", aistudio.ErrAuthenticationRequired)

const playgroundSessionLeaseWaitTimeout = 2 * time.Second

var errPlaygroundSessionChanged = errors.New("Playground credentials or proxy changed during the request; retry with the updated account")

func playgroundCredentialVersion(credentials map[string]any) string {
	raw, _ := json.Marshal(credentials)
	return aistudio.ComputePayloadDigest(string(raw))
}

type playgroundActiveSession struct {
	acquisition                time.Duration
	firstProof                 bool
	background                 bool
	accountID                  int64
	account                    *Account
	lease                      *aistudio.SessionLease
	key, profile, proxy, email string
	ctx                        context.Context
}

func (s *PlaygroundGatewayService) acquireSession(ctx context.Context, account *Account) (*aistudio.Session, func(), error) {
	finish := func() {}
	if s.browser != nil && s.browser.warmup != nil {
		finish = s.browser.warmup.foreground(account.ID)
	}
	session, release, err := s.acquireSessionMode(ctx, account, false)
	if err != nil {
		finish()
		return nil, nil, err
	}
	var once sync.Once
	return session, func() { once.Do(func() { release(); finish() }) }, nil
}
func (s *PlaygroundGatewayService) acquireSessionMode(ctx context.Context, account *Account, background bool) (*aistudio.Session, func(), error) {
	acquireStarted := time.Now()
	if s.browser == nil || s.browser.Cache == nil {
		return nil, nil, errors.New("Playground Camoufox/Redis service is unavailable")
	}
	fresh, err := s.accountRepo.GetByID(ctx, account.ID)
	if err != nil {
		return nil, nil, err
	}
	if fresh.Platform != PlatformGemini || fresh.Type != AccountTypePlayground {
		return nil, nil, errors.New("account is not a Gemini Playground account")
	}
	if isGeminiWebPlayground(fresh) != isGeminiWebPlayground(account) {
		return nil, nil, errPlaygroundSessionChanged
	}
	profile := fresh.GetCredential("playground_profile_id")
	email := fresh.GetCredential("playground_email")
	if profile == "" || email == "" {
		s.markManualActionFailure(ctx, fresh, errPlaygroundAuthorizationMissing)
		return nil, nil, errPlaygroundAuthorizationMissing
	}
	proxy := ""
	if required, _ := fresh.Credentials["playground_require_proxy"].(bool); required && (fresh.ProxyID == nil || fresh.Proxy == nil || !fresh.Proxy.IsActive() || fresh.Proxy.IsExpired(time.Now())) {
		return nil, nil, errors.New("此 Playground 账号要求使用有效代理，已停止直连回退")
	}
	if fresh.ProxyID != nil {
		if fresh.Proxy == nil {
			return nil, nil, errors.New("Playground account proxy is missing")
		}
		proxy = fresh.Proxy.URL()
	}
	var lease *aistudio.SessionLease
	if background {
		lease, err = s.browser.Cache.TryAcquire(ctx, profile)
	} else {
		waitCtx, stopWaiting := context.WithTimeout(ctx, playgroundSessionLeaseWaitTimeout)
		lease, err = s.browser.Cache.Acquire(waitCtx, profile)
		stopWaiting()
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return nil, nil, errPlaygroundSessionBusy
		}
	}
	if err != nil {
		return nil, nil, err
	}
	leaseCtx, cancel := context.WithCancel(ctx)
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(8 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-leaseCtx.Done():
				return
			case <-ticker.C:
				renewCtx, done := context.WithTimeout(leaseCtx, 5*time.Second)
				err := lease.Renew(renewCtx)
				done()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	var once sync.Once
	releaseLease := func() {
		once.Do(func() {
			cancel()
			<-heartbeatDone
			releaseCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
			defer done()
			lease.Release(releaseCtx)
		})
	}
	latest, err := s.accountRepo.GetByID(leaseCtx, account.ID)
	if err != nil {
		releaseLease()
		return nil, nil, err
	}
	if background && !playgroundWarmupEligible(latest) {
		releaseLease()
		return nil, nil, aistudio.ErrSessionBusy
	}
	if playgroundCredentialVersion(latest.Credentials) != playgroundCredentialVersion(fresh.Credentials) || !samePlaygroundProxy(latest, fresh) {
		releaseLease()
		return nil, nil, errPlaygroundSessionChanged
	}
	key := playgroundCacheKey(profile, proxy, email)
	active := &playgroundActiveSession{accountID: fresh.ID, account: latest, lease: lease, key: key, profile: profile, proxy: proxy, email: email, ctx: leaseCtx, background: background}
	session, unlock, err := s.waaManager.AcquireFresh(leaseCtx, strconv.FormatInt(account.ID, 10), func() (aistudio.SessionOptions, error) {
		state, hit, err := lease.Get(leaseCtx, key)
		if err != nil {
			return aistudio.SessionOptions{}, err
		}
		if !hit {
			state, err = s.recoverPlaygroundSession(leaseCtx, latest, profile, proxy, email, "cache_miss", background)
			if err != nil {
				if errors.Is(err, camoufox.ErrInteractiveLoginRequired) {
					_ = lease.RequireLogin(leaseCtx, key)
				}
				return aistudio.SessionOptions{}, err
			}
			if err = lease.Put(leaseCtx, key, state); err != nil {
				return aistudio.SessionOptions{}, err
			}
		}
		raw, _ := json.Marshal(state)
		return aistudio.SessionOptions{State: state, Version: aistudio.ComputePayloadDigest(string(raw)), ProxyURL: proxy, AuthUser: 0, OnChange: func(state aistudio.StorageState) (string, error) {
			persistCtx, done := context.WithTimeout(leaseCtx, 10*time.Second)
			defer done()
			if err := lease.Put(persistCtx, key, state); err != nil {
				return "", err
			}
			raw, err := json.Marshal(state)
			return aistudio.ComputePayloadDigest(string(raw)), err
		}}, nil
	})
	if err != nil {
		s.markManualActionFailure(leaseCtx, latest, err)
		releaseLease()
		return nil, nil, err
	}
	active.acquisition = time.Since(acquireStarted)
	s.activeSessions.Store(session, active)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { s.activeSessions.Delete(session); unlock(); releaseLease() }) }
	return session, release, nil
}
func samePlaygroundProxy(a, b *Account) bool {
	if a.ProxyID == nil || b.ProxyID == nil {
		return a.ProxyID == nil && b.ProxyID == nil
	}
	if *a.ProxyID != *b.ProxyID {
		return false
	}
	if a.Proxy == nil || b.Proxy == nil {
		return a.Proxy == nil && b.Proxy == nil
	}
	return a.Proxy.URL() == b.Proxy.URL()
}
func (s *PlaygroundGatewayService) refreshSession(session *aistudio.Session) error {
	value, ok := s.activeSessions.Load(session)
	if !ok {
		return aistudio.ErrSessionLeaseLost
	}
	active := value.(*playgroundActiveSession)
	if err := active.lease.Delete(active.ctx, active.key); err != nil {
		return err
	}
	state, err := s.recoverPlaygroundSession(active.ctx, active.account, active.profile, active.proxy, active.email, "upstream_authentication", active.background)
	if err != nil {
		if errors.Is(err, camoufox.ErrInteractiveLoginRequired) {
			_ = active.lease.RequireLogin(active.ctx, active.key)
		}
		return err
	}
	// ReplaceState commits through the current lease and invalidates the old WAA VM.
	return session.ReplaceState(state)
}

func (s *PlaygroundGatewayService) recoverPlaygroundSession(ctx context.Context, account *Account, profile, proxy, email, trigger string, background bool) (aistudio.StorageState, error) {
	start := time.Now()
	accountID := account.ID
	slog.Info("playground.session_recovery_started", "account_id", accountID, "profile_id", profile, "trigger", trigger, "proxy_configured", proxy != "")
	recover := s.browser.recover
	if isGeminiWebPlayground(account) {
		recover = s.browser.manager.RecoverGemini
	}
	if background && !isGeminiWebPlayground(account) && s.browser.recoverWarm != nil {
		recover = s.browser.recoverWarm
	}
	finishRecovery := func() {}
	if !background && s.browser.warmup != nil {
		finishRecovery = s.browser.warmup.foregroundRecovery()
	}
	state, err := recover(ctx, profile, proxy, email)
	finishRecovery()
	if err != nil {
		slog.Warn("playground.session_recovery_failed", "account_id", accountID, "profile_id", profile, "trigger", trigger, "duration_ms", time.Since(start).Milliseconds(), "requires_login", errors.Is(err, camoufox.ErrInteractiveLoginRequired), "error", err)
		s.markManualActionFailure(ctx, account, err)
	} else {
		slog.Info("playground.session_recovery_completed", "account_id", accountID, "profile_id", profile, "trigger", trigger, "duration_ms", time.Since(start).Milliseconds())
	}
	return state, err
}

func (s *PlaygroundGatewayService) rawGenerate(ctx context.Context, session *aistudio.Session, model string, reqBody []byte) (*http.Response, error) {
	// Parse once here to keep the exact binding and wire encoder common to tests and API calls.
	request, err := decodePlaygroundRequest(reqBody)
	if err != nil {
		return nil, err
	}
	prepareStarted := time.Now()
	wasReady := session.Ready()
	proof, err := session.Proof(ctx, model, aistudio.ComputePayloadDigest(aistudio.ExtractPromptForBinding(request)), aistudio.PagePrompt(request))
	if err != nil {
		return nil, err
	}
	elapsed := time.Since(prepareStarted)
	if value, ok := s.activeSessions.Load(session); ok {
		active := value.(*playgroundActiveSession)
		if !active.firstProof {
			elapsed += active.acquisition
			active.firstProof = true
		}
	}
	session.RecordRequestPreparation(elapsed, wasReady)
	wire, err := aistudio.EncodeGenerateContentRequest(model, request, proof)
	if err != nil {
		return nil, err
	}
	return session.RPC(ctx, "GenerateContent", wire)
}

func (s *PlaygroundGatewayService) Models(ctx context.Context, account *Account) ([]string, []byte, error) {
	if isGeminiWebPlayground(account) {
		return s.geminiWebModels(ctx, account)
	}
	session, release, err := s.acquireSession(ctx, account)
	if err != nil {
		return nil, nil, err
	}
	defer release()
	ctx = s.sessionContext(session)
	account = s.sessionAccount(session)
	for attempt := 0; attempt < 2; attempt++ {
		response, err := session.RPC(ctx, "ListModels", []byte("[]"))
		var raw []byte
		var models []string
		if err == nil {
			raw, err = io.ReadAll(io.LimitReader(response.Body, 16<<20))
			response.Body.Close()
			if err == nil {
				err = aistudio.CheckRPCResponse("ListModels", response.StatusCode, raw)
			}
			if err == nil {
				models, err = aistudio.ParseModels(raw)
			}
		}
		if errors.Is(err, aistudio.ErrAuthenticationRequired) && attempt == 0 {
			if err = s.refreshSession(session); err != nil {
				return nil, nil, err
			}
			continue
		}
		if err != nil {
			s.markManualActionFailure(ctx, account, err)
			return nil, nil, err
		}
		for i := range models {
			models[i] = strings.TrimPrefix(models[i], "models/")
		}
		return dedupeAndSortModelIDs(models), raw, nil
	}
	return nil, nil, aistudio.ErrAuthenticationRequired
}

func (s *PlaygroundGatewayService) sessionContext(session *aistudio.Session) context.Context {
	value, _ := s.activeSessions.Load(session)
	return value.(*playgroundActiveSession).ctx
}

func (s *PlaygroundGatewayService) sessionAccount(session *aistudio.Session) *Account {
	value, _ := s.activeSessions.Load(session)
	return value.(*playgroundActiveSession).account
}
