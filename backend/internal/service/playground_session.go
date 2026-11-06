package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

var errPlaygroundSessionChanged = errors.New("Playground credentials or proxy changed during the request; retry with the updated account")

func playgroundCredentialVersion(credentials map[string]any) string {
	raw, _ := json.Marshal(credentials)
	return aistudio.ComputePayloadDigest(string(raw))
}

type playgroundActiveSession struct {
	lease                      *aistudio.SessionLease
	key, profile, proxy, email string
	ctx                        context.Context
}

func (s *PlaygroundGatewayService) acquireSession(ctx context.Context, account *Account) (*aistudio.Session, func(), error) {
	if s.browser == nil || s.browser.Cache == nil {
		return nil, nil, errors.New("Playground Camoufox/Redis service is unavailable")
	}
	fresh, err := s.accountRepo.GetByID(ctx, account.ID)
	if err != nil {
		return nil, nil, err
	}
	profile := fresh.GetCredential("playground_profile_id")
	email := fresh.GetCredential("playground_email")
	if profile == "" || email == "" {
		return nil, nil, fmt.Errorf("%w: this account must be authorized with Camoufox", aistudio.ErrAuthenticationRequired)
	}
	if fresh.Platform != PlatformGemini || fresh.Type != AccountTypePlayground {
		return nil, nil, errors.New("account is not a Gemini Playground account")
	}
	proxy := ""
	if fresh.ProxyID != nil {
		if fresh.Proxy == nil {
			return nil, nil, errors.New("Playground account proxy is missing")
		}
		proxy = fresh.Proxy.URL()
	}
	lease, err := s.browser.Cache.Acquire(ctx, profile)
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
	if playgroundCredentialVersion(latest.Credentials) != playgroundCredentialVersion(fresh.Credentials) || !samePlaygroundProxy(latest, fresh) {
		releaseLease()
		return nil, nil, errPlaygroundSessionChanged
	}
	key := playgroundCacheKey(profile, proxy, email)
	active := &playgroundActiveSession{lease: lease, key: key, profile: profile, proxy: proxy, email: email, ctx: leaseCtx}
	session, unlock, err := s.waaManager.AcquireFresh(leaseCtx, strconv.FormatInt(account.ID, 10), func() (aistudio.SessionOptions, error) {
		state, hit, err := lease.Get(leaseCtx, key)
		if err != nil {
			return aistudio.SessionOptions{}, err
		}
		if !hit {
			state, err = s.browser.recover(leaseCtx, profile, proxy, email)
			if err != nil {
				if errors.Is(err, aistudio.ErrAuthenticationRequired) {
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
		releaseLease()
		return nil, nil, err
	}
	s.activeSessions.Store(session, active)
	release := func() { s.activeSessions.Delete(session); unlock(); releaseLease() }
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
	state, err := s.browser.recover(active.ctx, active.profile, active.proxy, active.email)
	if err != nil {
		if errors.Is(err, aistudio.ErrAuthenticationRequired) {
			_ = active.lease.RequireLogin(active.ctx, active.key)
		}
		return err
	}
	// ReplaceState commits through the current lease and invalidates the old WAA VM.
	return session.ReplaceState(state)
}

func (s *PlaygroundGatewayService) rawGenerate(ctx context.Context, session *aistudio.Session, model string, reqBody []byte) (*http.Response, error) {
	// Parse once here to keep the exact binding and wire encoder common to tests and API calls.
	request, err := decodePlaygroundRequest(reqBody)
	if err != nil {
		return nil, err
	}
	proof, err := session.Proof(ctx, model, aistudio.ComputePayloadDigest(aistudio.ExtractPromptForBinding(request)), aistudio.PagePrompt(request))
	if err != nil {
		return nil, err
	}
	wire, err := aistudio.EncodeGenerateContentRequest(model, request, proof)
	if err != nil {
		return nil, err
	}
	return session.RPC(ctx, "GenerateContent", wire)
}

func (s *PlaygroundGatewayService) Models(ctx context.Context, account *Account) ([]string, []byte, error) {
	session, release, err := s.acquireSession(ctx, account)
	if err != nil {
		return nil, nil, err
	}
	defer release()
	ctx = s.sessionContext(session)
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
