package camoufox

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBrowserCapacityAllowsTwentyWhenMemoryPermits(t *testing.T) {
	g := newBrowserGate()
	g.budget = func() memoryBudget { return memoryBudget{Limit: 32 << 30, Used: 128 << 20, Available: 30 << 30} }
	g.setTarget(20)
	releases := []func(){}
	for i := 0; i < 20; i++ {
		release, err := g.acquire(context.Background(), 2)
		require.NoError(t, err)
		releases = append(releases, release)
	}
	require.Equal(t, 20, g.stats().Active)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := g.acquire(ctx, 2)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	for _, release := range releases {
		release()
		release()
	}
	require.Zero(t, g.stats().Active)
}

func TestBrowserCapacityReservesMemoryBeforeProcessesAllocate(t *testing.T) {
	g := newBrowserGate()
	g.budget = func() memoryBudget { return memoryBudget{Limit: 1 << 30, Used: 128 << 20, Available: 2 << 30} }
	g.setTarget(20)
	require.Equal(t, 1, g.stats().Effective)
	release, err := g.acquire(context.Background(), 2)
	require.NoError(t, err)
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = g.acquire(ctx, 2)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, 1, g.stats().Active)
}

func TestBrowserCapacityPrioritizesRecoveryAndHonorsCancellation(t *testing.T) {
	g := newBrowserGate()
	g.target = 1
	g.budget = func() memoryBudget { return memoryBudget{} }
	release, err := g.acquire(context.Background(), 2)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	order := make(chan int, 2)
	humanRelease := make(chan struct{})
	var finished atomic.Int32
	go func() {
		r, e := g.acquire(ctx, 2)
		if e == nil {
			order <- 2
			r()
		}
		finished.Add(1)
	}()
	go func() {
		r, e := g.acquire(ctx, 0)
		if e == nil {
			order <- 0
			<-humanRelease
			r()
		}
		finished.Add(1)
	}()
	require.Eventually(t, func() bool { return g.stats().Waiting == 2 }, time.Second, time.Millisecond)
	release()
	require.Equal(t, 0, <-order)
	close(humanRelease)
	require.Equal(t, 2, <-order)
	require.Eventually(t, func() bool { return finished.Load() == 2 }, time.Second, time.Millisecond)
}
