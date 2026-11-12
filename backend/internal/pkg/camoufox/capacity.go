package camoufox

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const browserMemoryEstimate = int64(384 << 20)
const browserMemoryReserve = int64(192 << 20)
const hostMemoryReserve = int64(256 << 20)

type memoryBudget struct{ Limit, Used, Available int64 }

type BrowserCapacity struct {
	Configured int    `json:"configured"`
	Effective  int    `json:"effective"`
	Active     int    `json:"active"`
	Waiting    int    `json:"waiting"`
	Reason     string `json:"reason,omitempty"`
}

type browserGate struct {
	mu       sync.Mutex
	target   int
	active   int
	waiting  [4]int
	baseUsed int64
	changed  chan struct{}
	budget   func() memoryBudget
}

func newBrowserGate() *browserGate {
	return &browserGate{target: 2, changed: make(chan struct{}), budget: readBrowserBudget}
}

func readBrowserBudget() memoryBudget {
	var result memoryBudget
	read := func(path string) int64 {
		b, err := os.ReadFile(path)
		if err != nil {
			return 0
		}
		n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		if n > 1<<60 {
			return 0
		}
		return n
	}
	result.Limit = read("/sys/fs/cgroup/memory.max")
	result.Used = read("/sys/fs/cgroup/memory.current")
	statPath := "/sys/fs/cgroup/memory.stat"
	inactiveKey := "inactive_file"
	if result.Limit == 0 {
		result.Limit = read("/sys/fs/cgroup/memory/memory.limit_in_bytes")
		result.Used = read("/sys/fs/cgroup/memory/memory.usage_in_bytes")
		statPath = "/sys/fs/cgroup/memory/memory.stat"
		inactiveKey = "total_inactive_file"
	}
	if raw, err := os.ReadFile(statPath); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[0] == inactiveKey {
				n, _ := strconv.ParseInt(fields[1], 10, 64)
				result.Used = max(int64(0), result.Used-n)
			}
		}
	}
	if raw, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] == "MemAvailable:" {
				n, _ := strconv.ParseInt(fields[1], 10, 64)
				result.Available = n * 1024
			}
		}
	}
	return result
}

func (g *browserGate) signal() { close(g.changed); g.changed = make(chan struct{}) }

func (g *browserGate) setTarget(n int) {
	if n < 2 {
		n = 2
	}
	if n > 128 {
		n = 128
	}
	g.mu.Lock()
	g.target = n
	g.signal()
	g.mu.Unlock()
}

func (g *browserGate) capacity() BrowserCapacity {
	b := g.budget()
	base := g.baseUsed
	if g.active == 0 {
		base = b.Used
	}
	limit := g.target
	if b.Limit > 0 {
		limit = min(limit, max(0, int((b.Limit-base-browserMemoryReserve)/browserMemoryEstimate)))
		if b.Limit-b.Used < browserMemoryEstimate+browserMemoryReserve {
			limit = min(limit, g.active)
		}
	}
	if b.Available > 0 {
		// Add back observed browser memory, so startup reservations cannot all
		// observe the same free RAM before their child processes have allocated it.
		budget := b.Available + max(int64(0), b.Used-base) - hostMemoryReserve
		limit = min(limit, max(0, int(budget/browserMemoryEstimate)))
	}
	result := BrowserCapacity{Configured: g.target, Effective: limit, Active: g.active, Waiting: g.waiting[0] + g.waiting[1] + g.waiting[2]}
	if limit < g.target {
		result.Reason = "memory"
	}
	return result
}

func (g *browserGate) stats() BrowserCapacity { g.mu.Lock(); defer g.mu.Unlock(); return g.capacity() }

// Recovery=0, human login=1, batch automation=2, speculative prewarm=3. Existing callers keep their
// reservations; lowering the target only stops new admissions.
func (g *browserGate) acquire(ctx context.Context, priority int) (func(), error) {
	priority = max(0, min(priority, 3))
	g.mu.Lock()
	g.waiting[priority]++
	g.mu.Unlock()
	defer func() { g.mu.Lock(); g.waiting[priority]--; g.signal(); g.mu.Unlock() }()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		g.mu.Lock()
		higher := 0
		for i := 0; i < priority; i++ {
			higher += g.waiting[i]
		}
		capacity := g.capacity()
		if higher == 0 && g.active < capacity.Effective {
			if g.active == 0 {
				g.baseUsed = g.budget().Used
			}
			g.active++
			g.mu.Unlock()
			var once sync.Once
			return func() { once.Do(func() { g.mu.Lock(); g.active--; g.signal(); g.mu.Unlock() }) }, nil
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		case <-tick.C:
		}
	}
}

func (m *Manager) SetConcurrency(n int)      { m.gate.setTarget(n) }
func (m *Manager) Capacity() BrowserCapacity { return m.gate.stats() }
func (m *Manager) StartBatch(ctx context.Context, profile, proxy string) (*Browser, error) {
	return m.start(ctx, profile, proxy, 2)
}

// MemoryHeadroom includes native VMs and browser children, unlike Go heap stats.
// A negative value means the platform does not expose a usable memory limit.
func MemoryHeadroom() int64 {
	b := readBrowserBudget()
	free := int64(-1)
	if b.Limit > 0 {
		free = max(int64(0), b.Limit-b.Used)
	}
	if b.Available > 0 && (free < 0 || b.Available < free) {
		free = b.Available
	}
	return free
}
