package aistudio

import (
	"context"
	"sync"
)

// WAAManager owns one serialized browser session per account. The lease spans
// the response body so changing credentials cannot close an active VM.
type WAAManager struct{ entries sync.Map }
type sessionEntry struct {
	gate    chan struct{}
	session *Session
}

var defaultWAAManager = &WAAManager{}

func DefaultWAAManager() *WAAManager { return defaultWAAManager }

// CloseCached releases login material and VM timers. Keep the empty gate so
// callers already waiting on it cannot race a newly allocated entry.
func (m *WAAManager) CloseCached(ctx context.Context, key string) error {
	loaded, ok := m.entries.Load(key)
	if !ok {
		return nil
	}
	entry := loaded.(*sessionEntry)
	select {
	case entry.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-entry.gate }()
	if entry.session != nil {
		entry.session.Close()
		entry.session = nil
	}
	return nil
}

// AcquireFresh reads credentials after waiting for the preceding response.
func (m *WAAManager) AcquireFresh(ctx context.Context, key string, load func() (SessionOptions, error)) (*Session, func(), error) {
	loaded, _ := m.entries.LoadOrStore(key, &sessionEntry{gate: make(chan struct{}, 1)})
	entry := loaded.(*sessionEntry)
	select {
	case entry.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	var once sync.Once
	release := func() { once.Do(func() { <-entry.gate }) }
	options, err := load()
	if err != nil {
		release()
		return nil, nil, err
	}
	if entry.session != nil && (entry.session.version() != options.Version || entry.session.proxyURL != options.ProxyURL || entry.session.authUser != options.AuthUser || entry.session.pendingNetworkError() != nil) {
		entry.session.Close()
		entry.session = nil
	}
	if entry.session == nil {
		session, err := NewSession(options)
		if err != nil {
			release()
			return nil, nil, err
		}
		entry.session = session
	}
	entry.session.setPersistence(options.OnChange)
	return entry.session, release, nil
}
