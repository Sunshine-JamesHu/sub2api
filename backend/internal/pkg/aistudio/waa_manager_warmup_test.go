package aistudio

import (
	"context"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func warmManagerSession(t *testing.T, m *WAAManager, key string) (*Session, func()) {
	t.Helper()
	s, release, err := m.AcquireFresh(context.Background(), key, func() (SessionOptions, error) { return SessionOptions{Version: "fixture", Client: &http.Client{}}, nil })
	require.NoError(t, err)
	return s, release
}
func TestManagerEvictsIdleWithoutInterruptingActiveAccount(t *testing.T) {
	m := &WAAManager{}
	a, releaseA := warmManagerSession(t, m, "active")
	a.resident.Store(true)
	defer releaseA()
	defer a.Close()
	b, releaseB := warmManagerSession(t, m, "idle")
	b.resident.Store(true)
	releaseB()
	m.Trim(1, 0)
	require.False(t, a.isClosed())
	require.False(t, b.isClosed())
	require.False(t, b.resident.Load())
	require.Len(t, m.Snapshot(), 2)
	c, releaseC := warmManagerSession(t, m, "parallel")
	releaseC()
	defer c.Close()
	require.False(t, a.isClosed())
}
func TestManagerIdleTTLAndEligibilityEviction(t *testing.T) {
	m := &WAAManager{}
	a, release := warmManagerSession(t, m, "old")
	release()
	entry, _ := m.entries.Load("old")
	entry.(*sessionEntry).lastUsed.Store(time.Now().Add(-31 * time.Minute).UnixNano())
	m.Trim(16, 30*time.Minute)
	require.True(t, a.isClosed())
	require.Empty(t, m.Snapshot())
	b, release := warmManagerSession(t, m, "cooling")
	release()
	m.EvictExcept(map[string]bool{})
	require.True(t, b.isClosed())
}
func TestManagerReleaseDetachesCanceledPersistenceAndNetworkScope(t *testing.T) {
	m := &WAAManager{}
	var oldWrites, newWrites atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	options := func(callback func(StorageState) (string, error)) SessionOptions {
		return SessionOptions{Version: "fixture", Client: &http.Client{}, OnChange: callback}
	}
	s, release, err := m.AcquireFresh(ctx, "account", func() (SessionOptions, error) {
		return options(func(StorageState) (string, error) { oldWrites.Add(1); return "fixture", ctx.Err() }), nil
	})
	require.NoError(t, err)
	callbackCtx, done, ok := s.callbackContext()
	require.True(t, ok)
	callbackExited := make(chan struct{})
	go func() { <-callbackCtx.Done(); done(); close(callbackExited) }()
	release()
	cancel()
	<-callbackExited
	_, _, ok = s.callbackContext()
	require.False(t, ok)
	require.NoError(t, s.pendingNetworkError())
	again, releaseAgain, err := m.AcquireFresh(context.Background(), "account", func() (SessionOptions, error) {
		return options(func(StorageState) (string, error) { newWrites.Add(1); return "fixture", nil }), nil
	})
	require.NoError(t, err)
	defer releaseAgain()
	defer again.Close()
	require.Same(t, s, again)
	require.NoError(t, again.Commit())
	require.Zero(t, oldWrites.Load())
	require.EqualValues(t, 1, newWrites.Load())
}
func TestCanceledBackgroundResponseCannotRotateCachedCookies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	state := StorageState{Cookies: []StateCookie{{Name: "SAPISID", Value: "old", Domain: ".google.com", Path: "/", Expires: -1}}}
	transport := playgroundCookieTransport(func(r *http.Request) (*http.Response, error) {
		cancel()
		return &http.Response{StatusCode: 200, Header: http.Header{"Set-Cookie": {"SAPISID=late; Domain=.google.com; Path=/"}}, Body: io.NopCloser(strings.NewReader("ok")), Request: r}, nil
	})
	s, err := NewSession(SessionOptions{State: state, Client: &http.Client{Transport: transport}})
	require.NoError(t, err)
	defer s.Close()
	_, err = s.Do(ctx, http.MethodGet, AIStudioReferer, nil, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, "old", s.StorageState().Cookies[0].Value)
	require.NoError(t, s.pendingNetworkError())
}
func TestSessionCacheTryAcquireDoesNotQueue(t *testing.T) {
	r := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: r.Addr()})
	defer c.Close()
	cache := &SessionCache{Client: c}
	lease, err := cache.TryAcquire(context.Background(), "busy")
	require.NoError(t, err)
	defer lease.Release(context.Background())
	_, err = cache.TryAcquire(context.Background(), "busy")
	require.ErrorIs(t, err, ErrSessionBusy)
	lease.Release(context.Background())
	next, err := cache.TryAcquire(context.Background(), "busy")
	require.NoError(t, err)
	next.Release(context.Background())
}

func TestSessionReplacementDrainsOldCallbacksAndRetainsCurrentLease(t *testing.T) {
	state := StorageState{Cookies: []StateCookie{{Name: "SAPISID", Value: "old", Domain: ".google.com", Path: "/", Expires: -1}}}
	var writes atomic.Int32
	s, err := NewSession(SessionOptions{State: state, Client: &http.Client{}, OnChange: func(StorageState) (string, error) { writes.Add(1); return "updated", nil }})
	require.NoError(t, err)
	defer s.Close()
	old, done, ok := s.callbackContext()
	require.True(t, ok)
	go func() { <-old.Done(); done() }()
	require.NoError(t, s.ReplaceState(state))
	require.EqualValues(t, 1, writes.Load())
	current, finish, ok := s.callbackContext()
	require.True(t, ok)
	defer finish()
	require.NoError(t, current.Err())
	require.ErrorIs(t, old.Err(), context.Canceled)
}
