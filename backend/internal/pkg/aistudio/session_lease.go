package aistudio

import (
	"context"
	"sync"
	"time"
)

type sessionScope struct {
	ctx     context.Context
	parent  context.Context
	cancel  context.CancelFunc
	pending sync.WaitGroup
}

// A cached VM may retain JS timers, but its network callbacks must never outlive
// the Redis lease or persist through a previous request's canceled context.
func (s *Session) bindLease(ctx context.Context, persist func(StorageState) (string, error)) {
	s.unbindLease()
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	s.setPersistence(persist)
	s.leaseMu.Lock()
	s.leaseScope = &sessionScope{ctx: ctx, parent: parent, cancel: cancel}
	s.leaseMu.Unlock()
}

func (s *Session) unbindLease() {
	s.leaseMu.Lock()
	scope := s.leaseScope
	s.leaseScope = nil
	if scope != nil {
		scope.cancel()
	}
	s.leaseMu.Unlock()
	if scope != nil {
		scope.pending.Wait()
	}
	s.setPersistence(nil)
}

func (s *Session) callbackContext() (context.Context, func(), bool) {
	s.leaseMu.Lock()
	scope := s.leaseScope
	if scope == nil || scope.ctx.Err() != nil {
		s.leaseMu.Unlock()
		return nil, nil, false
	}
	scope.pending.Add(1)
	s.leaseMu.Unlock()
	ctx, cancel := context.WithTimeout(scope.ctx, 30*time.Second)
	return ctx, func() { cancel(); scope.pending.Done() }, true
}

// ReplaceState drains old VM callbacks before changing headers/cookies, then
// reattaches the still-valid account lease for the replacement runtime.
func (s *Session) pauseCallbacks() func() {
	s.leaseMu.Lock()
	scope := s.leaseScope
	s.leaseScope = nil
	if scope != nil {
		scope.cancel()
	}
	s.leaseMu.Unlock()
	if scope == nil {
		return func() {}
	}
	scope.pending.Wait()
	return func() {
		ctx, cancel := context.WithCancel(scope.parent)
		s.leaseMu.Lock()
		s.leaseScope = &sessionScope{parent: scope.parent, ctx: ctx, cancel: cancel}
		s.leaseMu.Unlock()
	}
}
