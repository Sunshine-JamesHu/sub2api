package aistudio

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// WAAManager serializes only requests for the same account. Idle sessions have
// bounded retention; eviction never closes an in-flight response or its VM.
type WAAManager struct {
	entries   sync.Map
	trimMu    sync.Mutex
	maxCached atomic.Int64
	idleNanos atomic.Int64
}
type sessionEntry struct {
	gate     chan struct{}
	session  atomic.Pointer[Session]
	lastUsed atomic.Int64
}
type CachedSessionInfo struct {
	RequestPreparationMS *int64    `json:"request_preparation_ms,omitempty"`
	LastRequestWarm      bool      `json:"last_request_warm"`
	AccountID            string    `json:"account_id"`
	Ready                bool      `json:"ready"`
	Resident             bool      `json:"resident"`
	PageReady            bool      `json:"page_ready"`
	Busy                 bool      `json:"busy"`
	LastUsed             time.Time `json:"last_used"`
	PreparationMS        int64     `json:"preparation_ms"`
}

var defaultWAAManager = &WAAManager{}

func DefaultWAAManager() *WAAManager { return defaultWAAManager }

func (m *WAAManager) Configure(maxCached int, idleTTL time.Duration) {
	m.maxCached.Store(int64(maxCached))
	m.idleNanos.Store(int64(idleTTL))
}

// Keep the empty gate so callers already waiting on it cannot race a new entry.
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
	if session := entry.session.Swap(nil); session != nil {
		session.Close()
	}
	return nil
}

func (m *WAAManager) AcquireFresh(ctx context.Context, key string, load func() (SessionOptions, error)) (*Session, func(), error) {
	loaded, _ := m.entries.LoadOrStore(key, &sessionEntry{gate: make(chan struct{}, 1)})
	entry := loaded.(*sessionEntry)
	select {
	case entry.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			if session := entry.session.Load(); session != nil {
				session.unbindLease()
			}
			entry.lastUsed.Store(time.Now().UnixNano())
			<-entry.gate
			if limit := m.maxCached.Load(); limit > 0 {
				m.Trim(int(limit), time.Duration(m.idleNanos.Load()))
			}
		})
	}
	options, err := load()
	if err != nil {
		release()
		return nil, nil, err
	}
	session := entry.session.Load()
	if session != nil && (session.version() != options.Version || session.proxyURL != options.ProxyURL || session.authUser != options.AuthUser || session.pendingNetworkError() != nil || session.isClosed()) {
		entry.session.Store(nil)
		session.Close()
		session = nil
	}
	if session == nil {
		session, err = NewSession(options)
		if err != nil {
			release()
			return nil, nil, err
		}
		entry.session.Store(session)
	}
	session.bindLease(ctx, options.OnChange)
	return session, release, nil
}

func (m *WAAManager) Snapshot() []CachedSessionInfo {
	result := []CachedSessionInfo{}
	m.entries.Range(func(key, value any) bool {
		entry := value.(*sessionEntry)
		if session := entry.session.Load(); session != nil {
			var requestMS *int64
			if session.hasRequest.Load() {
				value := session.requestPrepareMillis.Load()
				requestMS = &value
			}
			result = append(result, CachedSessionInfo{RequestPreparationMS: requestMS, LastRequestWarm: session.requestWarm.Load(), AccountID: key.(string), Ready: session.Ready(), Resident: session.resident.Load(), PageReady: session.pageLoaded.Load(), Busy: len(entry.gate) > 0, LastUsed: time.Unix(0, entry.lastUsed.Load()), PreparationMS: session.PreparationMillis()})
		}
		return true
	})
	sort.Slice(result, func(i, j int) bool { return result[i].AccountID < result[j].AccountID })
	return result
}

// Trim takes account gates without waiting. A response can exceed the idle
// budget while active; its session becomes eligible for eviction after release.
func (m *WAAManager) Trim(maxCached int, idleTTL time.Duration) {
	if !m.trimMu.TryLock() {
		return
	}
	defer m.trimMu.Unlock()
	type candidate struct {
		entry *sessionEntry
		used  int64
	}
	var candidates []candidate
	count := 0
	m.entries.Range(func(_, value any) bool {
		e := value.(*sessionEntry)
		if session := e.session.Load(); session != nil {
			if session.resident.Load() {
				count++
			}
			candidates = append(candidates, candidate{e, e.lastUsed.Load()})
		}
		return true
	})
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].used < candidates[j].used })
	now := time.Now()
	for _, c := range candidates {
		expired := idleTTL > 0 && now.Sub(time.Unix(0, c.used)) >= idleTTL
		if count <= maxCached && !expired {
			continue
		}
		e := c.entry
		select {
		case e.gate <- struct{}{}:
		default:
			continue
		}
		// Gates also protect hibernation: the HTTP response and its proof own
		// their runtime until release. Retain the page when only RAM is evicted.
		if e.lastUsed.Load() != c.used {
			<-e.gate
			continue
		}
		session := e.session.Load()
		if session == nil {
			<-e.gate
			continue
		}
		resident := session.resident.Load()
		if expired {
			e.session.Store(nil)
			<-e.gate
			session.Close()
			if resident {
				count--
			}
		} else if count > maxCached && resident {
			session.Hibernate()
			<-e.gate
			count--
		} else {
			<-e.gate
		}

	}
}

// Drop disabled, expired and cooling accounts without waiting on live traffic.
func (m *WAAManager) EvictExcept(keep map[string]bool) {
	m.entries.Range(func(key, value any) bool {
		if keep[key.(string)] {
			return true
		}
		e := value.(*sessionEntry)
		select {
		case e.gate <- struct{}{}:
		default:
			return true
		}
		session := e.session.Swap(nil)
		<-e.gate
		if session != nil {
			session.Close()
		}
		return true
	})
}
