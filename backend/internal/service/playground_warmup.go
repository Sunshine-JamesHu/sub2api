package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	"github.com/Wei-Shaw/sub2api/internal/pkg/camoufox"
	"github.com/redis/go-redis/v9"
)

const playgroundWarmupSettingsKey = "playground:warmup:settings"
const warmupMemoryReserve = int64(512 << 20)

type PlaygroundWarmupConfig struct {
	Enabled     bool `json:"enabled"`
	Target      int  `json:"target"`
	MaxCached   int  `json:"max_cached"`
	IdleMinutes int  `json:"idle_minutes"`
}

func defaultPlaygroundWarmupConfig() PlaygroundWarmupConfig {
	return PlaygroundWarmupConfig{Enabled: true, Target: 8, MaxCached: 16, IdleMinutes: 30}
}
func (c PlaygroundWarmupConfig) Validate() error {
	if c.Target < 0 || c.Target > c.MaxCached || c.MaxCached < 1 || c.MaxCached > 1000 || c.IdleMinutes < 5 || c.IdleMinutes > 240 {
		return errors.New("预热数须在 0 到缓存上限之间；缓存上限 1–1000；闲置回收 5–240 分钟")
	}
	return nil
}

type PlaygroundWarmupAccount struct {
	RequestPreparationMS *int64     `json:"request_preparation_ms,omitempty"`
	LastRequestWarm      bool       `json:"last_request_warm"`
	AccountID            int64      `json:"account_id"`
	State                string     `json:"state"`
	DurationMS           int64      `json:"duration_ms"`
	PreparedAt           *time.Time `json:"prepared_at,omitempty"`
	RetryAt              *time.Time `json:"retry_at,omitempty"`
	Error                string     `json:"error,omitempty"`
	failures             int
}
type PlaygroundWarmupView struct {
	LightCached      int                       `json:"light_cached"`
	Config           PlaygroundWarmupConfig    `json:"config"`
	Eligible         int                       `json:"eligible"`
	Cached           int                       `json:"cached"`
	Ready            int                       `json:"ready"`
	ActiveRequests   int                       `json:"active_requests"`
	Warming          int64                     `json:"warming_account_id,omitempty"`
	MemoryHeadroomMB int64                     `json:"memory_headroom_mb"`
	PausedReason     string                    `json:"paused_reason,omitempty"`
	Accounts         []PlaygroundWarmupAccount `json:"accounts"`
}

// One low-priority producer per process, not one browser/timer per account.
// Account leases coordinate with other replicas; foreground traffic preempts it.
type PlaygroundWarmupService struct {
	recoveries     int
	browser        *PlaygroundBrowserService
	gateway        *PlaygroundGatewayService
	repo           AccountRepository
	manager        *aistudio.WAAManager
	mu             sync.Mutex
	config         PlaygroundWarmupConfig
	records        map[int64]PlaygroundWarmupAccount
	eligible       int
	active         int
	warming        int64
	cancelWarm     context.CancelFunc
	cancel         context.CancelFunc
	done           chan struct{}
	wake           chan struct{}
	headroom       func() int64
	prepare        func(context.Context, *Account) error
	settingsError  bool
	configRevision uint64
	configWriteMu  sync.Mutex
}

func (b *PlaygroundBrowserService) startWarmup(repo AccountRepository) {
	gateway := NewPlaygroundGatewayService(repo)
	gateway.browser = b
	w := &PlaygroundWarmupService{browser: b, gateway: gateway, repo: repo, manager: gateway.waaManager, config: defaultPlaygroundWarmupConfig(), records: map[int64]PlaygroundWarmupAccount{}, done: make(chan struct{}), wake: make(chan struct{}, 1), headroom: camoufox.MemoryHeadroom}
	w.prepare = w.prepareAccount
	b.warmup = w
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	go w.run(ctx)
}
func (w *PlaygroundWarmupService) run(ctx context.Context) {
	defer close(w.done)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		w.pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-w.wake:
		}
	}
}
func (w *PlaygroundWarmupService) Close() {
	w.cancel()
	<-w.done
	w.manager.EvictExcept(map[string]bool{})
}
func (w *PlaygroundWarmupService) foreground(accountID int64) func() {
	w.mu.Lock()
	w.active++
	if w.cancelWarm != nil && (w.warming == accountID || w.active >= w.config.MaxCached) {
		w.cancelWarm()
	}
	w.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { w.mu.Lock(); w.active--; w.mu.Unlock() }) }
}

// Browser recovery is the expensive part of cold admission. Give those real
// requests the browser/CPU budget; unrelated warm requests may run concurrently.
func (w *PlaygroundWarmupService) foregroundRecovery() func() {
	w.mu.Lock()
	w.recoveries++
	if w.cancelWarm != nil {
		w.cancelWarm()
	}
	w.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { w.mu.Lock(); w.recoveries--; w.mu.Unlock() }) }
}
func (w *PlaygroundWarmupService) Configure(ctx context.Context, c PlaygroundWarmupConfig) error {
	w.configWriteMu.Lock()
	defer w.configWriteMu.Unlock()
	if err := c.Validate(); err != nil {
		return err
	}
	raw, _ := json.Marshal(c)
	if err := w.browser.Cache.Client.Set(ctx, playgroundWarmupSettingsKey, raw, 0).Err(); err != nil {
		return err
	}
	w.mu.Lock()
	w.config = c
	w.configRevision++
	if w.cancelWarm != nil {
		w.cancelWarm()
	}
	w.mu.Unlock()
	w.manager.Configure(c.MaxCached, time.Duration(c.IdleMinutes)*time.Minute)
	select {
	case w.wake <- struct{}{}:
	default:
	}
	return nil
}
func (w *PlaygroundWarmupService) loadConfig(ctx context.Context) (PlaygroundWarmupConfig, error) {
	w.mu.Lock()
	revision := w.configRevision
	w.mu.Unlock()
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	raw, err := w.browser.Cache.Client.Get(readCtx, playgroundWarmupSettingsKey).Bytes()
	w.mu.Lock()
	defer w.mu.Unlock()
	if revision != w.configRevision {
		return w.config, nil
	}
	if err == nil {
		var c PlaygroundWarmupConfig
		if json.Unmarshal(raw, &c) != nil || c.Validate() != nil {
			w.settingsError = true
			return w.config, errors.New("invalid prewarm settings")
		}
		w.config = c
	} else if !errors.Is(err, redis.Nil) {
		w.settingsError = true
		return w.config, err
	}
	w.settingsError = false
	return w.config, nil
}
func playgroundWarmupEligible(a *Account) bool {
	if isGeminiWebPlayground(a) {
		return false
	}
	if a.Platform != PlatformGemini || a.Type != AccountTypePlayground || !a.IsSchedulable() || a.GetCredential("playground_profile_id") == "" || a.GetCredential("playground_email") == "" {
		return false
	}
	if a.ProxyID != nil && (a.Proxy == nil || !a.Proxy.IsActive() || a.Proxy.IsExpired(time.Now())) {
		return false
	}
	required, _ := a.Credentials["playground_require_proxy"].(bool)
	return !required || a.ProxyID != nil
}
func (w *PlaygroundWarmupService) pass(ctx context.Context) {
	config, err := w.loadConfig(ctx)
	w.manager.Configure(config.MaxCached, time.Duration(config.IdleMinutes)*time.Minute)
	keep := config.MaxCached
	headroom := w.headroom()
	if headroom >= 0 && headroom < 256<<20 {
		keep = 0
	}
	w.manager.Trim(keep, time.Duration(config.IdleMinutes)*time.Minute)
	if err != nil {
		return
	}
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	accounts, err := w.repo.ListSchedulableByPlatform(queryCtx, PlatformGemini)
	cancel()
	if err != nil {
		return
	}
	eligible := make([]Account, 0, len(accounts))
	ids := map[string]bool{}
	for _, a := range accounts {
		if playgroundWarmupEligible(&a) {
			eligible = append(eligible, a)
			ids[strconv.FormatInt(a.ID, 10)] = true
		}
	}
	w.manager.EvictExcept(ids)
	cached := map[int64]aistudio.CachedSessionInfo{}
	for _, entry := range w.manager.Snapshot() {
		id, _ := strconv.ParseInt(entry.AccountID, 10, 64)
		cached[id] = entry
	}
	// Prepare the next accounts in the existing priority/LRU rotation. Do not
	// change routing or concentrate requests on the accounts we have warmed.
	sort.SliceStable(eligible, func(i, j int) bool {
		a, b := eligible[i], eligible[j]
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if a.LastUsedAt != nil || b.LastUsedAt != nil {
			if a.LastUsedAt == nil {
				return true
			}
			if b.LastUsedAt == nil {
				return false
			}
			if !a.LastUsedAt.Equal(*b.LastUsedAt) {
				return a.LastUsedAt.Before(*b.LastUsedAt)
			}
		}
		if cached[a.ID].Ready != cached[b.ID].Ready {
			return cached[a.ID].Ready
		}
		return a.ID < b.ID
	})
	w.mu.Lock()
	w.eligible = len(eligible)
	for id := range w.records {
		if !ids[strconv.FormatInt(id, 10)] {
			delete(w.records, id)
		}
	}
	w.mu.Unlock()
	if !config.Enabled || config.Target == 0 {
		return
	}
	selected, attempts := 0, 0
	for i := 0; i < len(eligible) && selected < config.Target; i++ {
		if ctx.Err() != nil {
			return
		}
		if free := w.headroom(); free >= 0 && free < warmupMemoryReserve {
			return
		}
		a := eligible[i]
		w.mu.Lock()
		record := w.records[a.ID]
		now := time.Now()
		if w.recoveries > 0 || w.active >= config.MaxCached || w.config != config {
			w.mu.Unlock()
			return
		}
		if cached[a.ID].Ready && record.Error != "" {
			record.Error = ""
			record.RetryAt = nil
			record.State = "ready"
			record.failures = 0
			w.records[a.ID] = record
		}
		if record.RetryAt != nil && now.Before(*record.RetryAt) {
			w.mu.Unlock()
			continue
		}
		if cached[a.ID].Ready && record.PreparedAt != nil && now.Sub(*record.PreparedAt) < 3*time.Minute {
			selected++
			w.mu.Unlock()
			continue
		}
		if attempts >= config.Target {
			w.mu.Unlock()
			return
		}
		attempts++
		warmCtx, stop := context.WithTimeout(ctx, 2*time.Minute)
		w.cancelWarm = stop
		w.warming = a.ID
		w.mu.Unlock()
		started := time.Now()
		err := w.prepare(warmCtx, &a)
		canceled := warmCtx.Err() != nil
		stop()
		ended := time.Now()
		w.mu.Lock()
		w.cancelWarm = nil
		w.warming = 0
		record.AccountID = a.ID
		record.DurationMS = time.Since(started).Milliseconds()
		if err == nil {
			selected++
			record.State = "ready"
			record.PreparedAt = &ended
			record.RetryAt = nil
			record.Error = ""
			record.failures = 0
		} else if canceled || errors.Is(err, aistudio.ErrSessionBusy) {
			// Preemption is neither an account failure nor proof of an expired login.
			record.State = "deferred"
			record.Error = ""
			retry := ended.Add(15 * time.Second)
			record.RetryAt = &retry
		} else {
			record.State = "failed"
			record.failures++
			retry := ended.Add(time.Duration(min(record.failures*2, 15)) * time.Minute)
			record.RetryAt = &retry
			record.Error = playgroundWarmupFailure(err)
			slog.Warn("playground.warmup_failed", "account_id", a.ID, "reason", record.Error, "retry_at", retry)
		}
		w.records[a.ID] = record
		w.mu.Unlock()
	}
}
func (w *PlaygroundWarmupService) prepareAccount(ctx context.Context, a *Account) error {
	session, release, err := w.gateway.acquireSessionMode(ctx, a, true)
	if err != nil {
		return err
	}
	defer release()
	ctx = w.gateway.sessionContext(session)
	err = session.Warmup(ctx)
	if errors.Is(err, aistudio.ErrAuthenticationRequired) {
		if err = w.gateway.refreshSession(session); err == nil {
			err = session.Warmup(ctx)
		}
	}
	return err
}
func (w *PlaygroundWarmupService) View() PlaygroundWarmupView {
	w.mu.Lock()
	v := PlaygroundWarmupView{Config: w.config, Eligible: w.eligible, ActiveRequests: w.active, Warming: w.warming, Accounts: []PlaygroundWarmupAccount{}}
	recoveries := w.recoveries
	records := make(map[int64]PlaygroundWarmupAccount, len(w.records))
	for id, r := range w.records {
		records[id] = r
	}
	if w.settingsError {
		v.PausedReason = "settings_unavailable"
	}
	w.mu.Unlock()
	free := w.headroom()
	v.MemoryHeadroomMB = -1
	if free >= 0 {
		v.MemoryHeadroomMB = free >> 20
	}
	if !v.Config.Enabled || v.Config.Target == 0 {
		v.PausedReason = "disabled"
	} else if recoveries > 0 || v.ActiveRequests >= v.Config.MaxCached {
		v.PausedReason = "foreground"
	} else if free >= 0 && free < warmupMemoryReserve {
		v.PausedReason = "memory"
	}
	snapshot := w.manager.Snapshot()
	for _, item := range snapshot {
		if item.Resident {
			v.Cached++
		} else {
			v.LightCached++
		}
	}
	for _, item := range snapshot {
		id, _ := strconv.ParseInt(item.AccountID, 10, 64)
		r := records[id]
		r.AccountID = id
		r.RequestPreparationMS = item.RequestPreparationMS
		r.LastRequestWarm = item.LastRequestWarm
		if item.Ready {
			v.Ready++
			r.State = "ready"
			r.Error = ""
			r.RetryAt = nil
		} else if r.State != "failed" && r.State != "deferred" {
			r.State = "cold"
			if item.PageReady {
				r.State = "standby"
			}
		}
		if item.Busy {
			r.State = "busy"
		}
		if item.PreparationMS > 0 {
			r.DurationMS = item.PreparationMS
		}
		records[id] = r
	}
	for id, r := range records {
		if id == v.Warming {
			r.State = "warming"
		} else if r.State == "ready" {
			found := false
			for _, s := range snapshot {
				if s.AccountID == strconv.FormatInt(id, 10) {
					found = true
					break
				}
			}
			if !found {
				r.State = "evicted"
			}
		}
		v.Accounts = append(v.Accounts, r)
	}
	sort.Slice(v.Accounts, func(i, j int) bool { return v.Accounts[i].AccountID < v.Accounts[j].AccountID })
	return v
}
func (s *PlaygroundBrowserService) WarmupService() (*PlaygroundWarmupService, error) {
	if s == nil || s.warmup == nil {
		return nil, fmt.Errorf("Playground 预热服务不可用")
	}
	return s.warmup, nil
}

// Never return raw browser/transport errors, which may contain proxy credentials.
func playgroundWarmupFailure(err error) string {
	if errors.Is(err, aistudio.ErrAuthenticationRequired) {
		return "需要重新登录或人工验证"
	}
	var rpc *aistudio.RPCError
	if errors.As(err, &rpc) {
		return fmt.Sprintf("Google 准备接口返回 HTTP %d / RPC %d，将稍后重试", rpc.StatusCode, rpc.Code)
	}
	var network net.Error
	if errors.As(err, &network) {
		return "代理连接或 Google 请求超时，将稍后重试"
	}
	message := err.Error()
	if strings.Contains(message, "profile-missing") || strings.Contains(message, "profile-empty") {
		return "浏览器登录资料缺失，请重新导入或登录账号"
	}
	if strings.Contains(message, "recovery-timeout") {
		return "浏览器恢复登录态超时，请检查代理连接"
	}
	return "准备失败，将稍后重试；可使用账号测试查看上游错误"
}
