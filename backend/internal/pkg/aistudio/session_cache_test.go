package aistudio

import (
	"context"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type playgroundCookieTransport func(*http.Request) (*http.Response, error)

func (f playgroundCookieTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPlaygroundSetCookieWritesRedisWithoutSQL(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	cache := &SessionCache{Client: client}
	ctx := context.Background()
	lease, err := cache.Acquire(ctx, "profile")
	require.NoError(t, err)
	defer lease.Release(ctx)
	key := "playground:{profile}:session:test"
	state := StorageState{Cookies: []StateCookie{{Name: "SAPISID", Value: "old", Domain: ".google.com", Path: "/", Expires: -1}}}
	transport := playgroundCookieTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Set-Cookie": []string{"SAPISID=rotated; Domain=.google.com; Path=/; Secure; HttpOnly"}}, Body: io.NopCloser(strings.NewReader("ok")), Request: r}, nil
	})
	session, err := NewSession(SessionOptions{State: state, Client: &http.Client{Transport: transport}, OnChange: func(s StorageState) (string, error) { return "new", lease.Put(ctx, key, s) }})
	require.NoError(t, err)
	defer session.Close()
	response, err := session.Do(ctx, "GET", AIStudioReferer, nil, nil)
	require.NoError(t, err)
	response.Body.Close()
	cached, hit, err := lease.Get(ctx, key)
	require.NoError(t, err)
	require.True(t, hit)
	value, ok := cached.CookieValue("SAPISID", AIStudioReferer, time.Now())
	require.True(t, ok)
	require.Equal(t, "rotated", value)
	require.NoError(t, lease.RequireLogin(ctx, key))
	_, _, err = lease.Get(ctx, key)
	require.ErrorIs(t, err, ErrAuthenticationRequired)
}

func TestSessionCacheSlidingExpiryAndLeaseFencing(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	cache := &SessionCache{Client: client}
	ctx := context.Background()
	key := "playground:{profile}:session:test"
	lease, err := cache.Acquire(ctx, "profile")
	require.NoError(t, err)
	state := StorageState{Cookies: []StateCookie{{Name: "SAPISID", Value: "one", Domain: ".google.com", Path: "/", Expires: -1}}}
	require.NoError(t, lease.Put(ctx, key, state))
	lease.Release(ctx)
	server.FastForward(4 * time.Minute)
	lease, err = cache.Acquire(ctx, "profile")
	require.NoError(t, err)
	got, hit, err := lease.Get(ctx, key)
	require.NoError(t, err)
	require.True(t, hit)
	require.Equal(t, "one", got.Cookies[0].Value)
	require.Equal(t, 5*time.Minute, server.TTL(key))
	lease.Release(ctx)
	server.FastForward(5*time.Minute + time.Second)
	lease, err = cache.Acquire(ctx, "profile")
	require.NoError(t, err)
	_, hit, err = lease.Get(ctx, key)
	require.NoError(t, err)
	require.False(t, hit)
	server.FastForward(31 * time.Second)
	replacement, err := cache.Acquire(ctx, "profile")
	require.NoError(t, err)
	state.Cookies[0].Value = "new"
	require.NoError(t, replacement.Put(ctx, key, state))
	require.Error(t, lease.Put(ctx, key, state))
	require.Error(t, lease.Delete(ctx, key))
	require.Error(t, lease.DeleteProfileSessions(ctx))
	lease.Release(ctx)
	require.NoError(t, replacement.Renew(ctx))
	got, hit, err = replacement.Get(ctx, key)
	require.NoError(t, err)
	require.True(t, hit)
	require.Equal(t, "new", got.Cookies[0].Value)
}
func TestSessionCacheGrantIsSingleUse(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	cache := &SessionCache{Client: client}
	grant := BrowserGrant{Profile: "profile", Email: "test@example.test", Proxy: "digest"}
	token, err := cache.IssueGrant(context.Background(), grant)
	require.NoError(t, err)
	got, err := cache.ConsumeGrant(context.Background(), token)
	require.NoError(t, err)
	require.Equal(t, grant, got)
	_, err = cache.ConsumeGrant(context.Background(), token)
	require.Error(t, err)
}
