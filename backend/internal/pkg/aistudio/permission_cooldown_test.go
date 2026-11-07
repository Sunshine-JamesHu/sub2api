package aistudio

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestPermissionCooldownSharedBackoffAndRecovery(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	first, second := &SessionCache{Client: client}, &SessionCache{Client: client}
	ctx := context.Background()
	for i, want := range []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute, 30 * time.Minute} {
		cache := first
		if i%2 == 1 {
			cache = second
		}
		delay, streak, err := cache.PermissionFailure(ctx, "profile", "flash")
		require.NoError(t, err)
		require.Equal(t, want, delay)
		require.Equal(t, int64(min(i+1, 4)), streak)
	}
	delay, _, err := second.PermissionFailure(ctx, "profile", "argon")
	require.NoError(t, err)
	require.Equal(t, time.Minute, delay)
	delay, _, err = second.PermissionFailure(ctx, "other", "flash")
	require.NoError(t, err)
	require.Equal(t, time.Minute, delay)
	hadFailures, err := first.PermissionSuccess(ctx, "profile", "flash")
	require.NoError(t, err)
	require.True(t, hadFailures)
	delay, _, err = second.PermissionFailure(ctx, "profile", "flash")
	require.NoError(t, err)
	require.Equal(t, time.Minute, delay)
	delay, _, err = first.PermissionFailure(ctx, "profile", "argon")
	require.NoError(t, err)
	require.Equal(t, 5*time.Minute, delay, "success must not reset other models")
	server.FastForward(24*time.Hour + time.Second)
	delay, _, err = second.PermissionFailure(ctx, "profile", "argon")
	require.NoError(t, err)
	require.Equal(t, time.Minute, delay)
}

func TestSessionCacheConfiguredSlidingExpiry(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cache := &SessionCache{Client: client, TTL: 7 * time.Minute}
	ctx := context.Background()
	key := "playground:{profile}:session:override"
	lease, err := cache.Acquire(ctx, "profile")
	require.NoError(t, err)
	state := StorageState{Cookies: []StateCookie{{Name: "SAPISID", Value: "one", Domain: ".google.com", Path: "/", Expires: -1}}}
	require.NoError(t, lease.Put(ctx, key, state))
	lease.Release(ctx)
	server.FastForward(6 * time.Minute)
	lease, err = cache.Acquire(ctx, "profile")
	require.NoError(t, err)
	_, hit, err := lease.Get(ctx, key)
	require.NoError(t, err)
	require.True(t, hit)
	require.Equal(t, 7*time.Minute, server.TTL(key))
	lease.Release(ctx)
	server.FastForward(7*time.Minute + time.Second)
	lease, err = cache.Acquire(ctx, "profile")
	require.NoError(t, err)
	defer lease.Release(ctx)
	_, hit, err = lease.Get(ctx, key)
	require.NoError(t, err)
	require.False(t, hit)
}
