package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	"github.com/Wei-Shaw/sub2api/internal/pkg/camoufox"
	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/pquerna/otp/totp"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestPlaygroundBatchParserNormalizesTOTPWithoutEchoingSecrets(t *testing.T) {
	rows, err := parsePlaygroundBatch("Owner@Example.test----secret-password-marker----gezd gnbv gy3t qojq gezd gnbv gy3t qojq\nsecond@example.test----another-password----")
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, "owner@example.test", rows[0].Email)
	code, err := totp.GenerateCode(rows[0].Secret.TOTP, time.Unix(59, 0))
	require.NoError(t, err)
	require.Equal(t, "287082", code)
	_, err = parsePlaygroundBatch("a@example.test----secret-password-marker----not-a-secret")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret-password-marker")
	require.NotContains(t, err.Error(), "not-a-secret")
	_, err = parsePlaygroundBatch("u.ser@gmail.com----p----\nuser+alias@googlemail.com----p----")
	require.ErrorContains(t, err, "重复")
}

func TestPlaygroundBatchStoreEncryptsRejectsWrongKeyAndLocks(t *testing.T) {
	dir := t.TempDir()
	key := strings.Repeat("19", 32)
	store, err := openPlaygroundBatchStore(dir, key)
	require.NoError(t, err)
	_, err = openPlaygroundBatchStore(dir, key)
	require.Error(t, err)
	state := &playgroundBatchState{Version: 1, Concurrency: 20, Batches: []*playgroundBatch{{ID: uuid.NewString(), Jobs: []*playgroundBatchJob{{ID: uuid.NewString(), Email: "owner@example.test", Secret: &batchSecret{Password: "private-password-marker", TOTP: "JBSWY3DPEHPK3PXP"}}}}}}
	require.NoError(t, store.Save(state))
	raw, err := os.ReadFile(filepath.Join(dir, "state.enc"))
	require.NoError(t, err)
	for _, value := range []string{"private-password-marker", "JBSWY3DPEHPK3PXP", "owner@example.test"} {
		require.NotContains(t, string(raw), value)
	}
	loaded, err := store.Load()
	require.NoError(t, err)
	require.Equal(t, state, loaded)
	frameID := state.Batches[0].Jobs[0].ID
	require.NoError(t, store.SaveFrame(frameID, "private-frame-marker"))
	frame, err := store.LoadFrame(frameID)
	require.NoError(t, err)
	require.Equal(t, "private-frame-marker", frame)
	store.Close()
	wrong, err := openPlaygroundBatchStore(dir, strings.Repeat("20", 32))
	require.NoError(t, err)
	_, err = wrong.Load()
	require.Error(t, err)
	wrong.Close()
	store, err = openPlaygroundBatchStore(dir, key)
	require.NoError(t, err)
	defer store.Close()
	raw[len(raw)-1] ^= 1
	require.NoError(t, os.WriteFile(filepath.Join(dir, "state.enc"), raw, 0600))
	_, err = store.Load()
	require.Error(t, err)
}

type batchProxyFixture struct {
	ProxyRepository
	mu      sync.Mutex
	proxies []Proxy
}

func (p *batchProxyFixture) GetByID(_ context.Context, id int64) (*Proxy, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, proxy := range p.proxies {
		if proxy.ID == id {
			copy := proxy
			return &copy, nil
		}
	}
	return nil, ErrProxyNotFound
}
func (p *batchProxyFixture) ListActive(context.Context) ([]Proxy, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Proxy(nil), p.proxies...), nil
}

type batchAdminFixture struct {
	AdminService
	mu        sync.Mutex
	accounts  []Account
	browser   *PlaygroundBrowserService
	creates   int
	lostReply bool
}

func (a *batchAdminFixture) ValidateAccountGroupBindings(context.Context, []int64) error { return nil }
func (a *batchAdminFixture) GetUser(context.Context, int64) (*User, error) {
	return &User{ID: 1, Role: "admin", Status: "active"}, nil
}
func (a *batchAdminFixture) ListAccountsForSchedulerScoreFilter(context.Context, string, string, string, string, int64, string) ([]Account, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Account(nil), a.accounts...), nil
}
func (a *batchAdminFixture) CreateAccount(ctx context.Context, input *CreateAccountInput) (*Account, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.creates++
	token, _ := input.Credentials["playground_login_token"].(string)
	grant, err := a.browser.Cache.ConsumeGrant(ctx, token)
	if err != nil {
		return nil, err
	}
	account := Account{ID: int64(len(a.accounts) + 1), Name: input.Name, Platform: input.Platform, Type: input.Type, ProxyID: input.ProxyID, Extra: input.Extra, Credentials: map[string]any{"playground_profile_id": grant.Profile, "playground_email": grant.Email, "playground_require_proxy": input.Credentials["playground_require_proxy"]}}
	a.accounts = append(a.accounts, account)
	if a.lostReply {
		return nil, errors.New("synthetic lost reply")
	}
	return &account, nil
}

type batchFakeBrowser struct {
	mu           sync.Mutex
	email        string
	manual       bool
	captureEmail string
	closed       atomic.Bool
}

func (b *batchFakeBrowser) LoginPage(_ context.Context, email string) (camoufox.LoginPage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.email = email
	if b.manual {
		return camoufox.LoginPage{Origin: "https://accounts.google.com", Phase: "manual", Reason: "captcha"}, nil
	}
	return camoufox.LoginPage{Origin: aistudio.AIStudioOrigin, Phase: "studio"}, nil
}
func (b *batchFakeBrowser) FillLoginField(context.Context, camoufox.LoginPage, string) error {
	return nil
}
func (b *batchFakeBrowser) ClickLoginTarget(context.Context, camoufox.LoginPage, bool) error {
	return nil
}
func (b *batchFakeBrowser) Capture(context.Context, string) (aistudio.StorageState, string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.manual {
		return aistudio.StorageState{}, "", aistudio.ErrAuthenticationRequired
	}
	if b.captureEmail != "" {
		return playgroundState("synthetic-cookie"), b.captureEmail, nil
	}
	return playgroundState("synthetic-cookie"), b.email, nil
}

func TestPlaygroundBatchManualTakeoverChecksIdentityAndSavesOnce(t *testing.T) {
	s, admin, _ := batchFixture(t)
	var starts atomic.Int32
	human := &batchFakeBrowser{manual: true}
	s.startBrowser = func(context.Context, string, string, bool, string) (batchBrowser, error) {
		if starts.Add(1) == 1 {
			return &batchFakeBrowser{manual: true}, nil
		}
		return human, nil
	}
	id, err := s.Create(context.Background(), 1, PlaygroundBatchRequest{Accounts: "owner@example.test----private-password-marker----", ProxyIDs: []int64{1}, Concurrency: 20})
	require.NoError(t, err)
	require.Eventually(t, func() bool { v := s.View(1, id); return v.Batches[0].Counts["manual"] == 1 && v.Running == 0 }, 5*time.Second, 10*time.Millisecond)
	job := s.View(1, id).Batches[0].Jobs[0]
	require.NoError(t, s.Action(1, id, job.ID, "open_manual"))
	require.Eventually(t, func() bool { return s.View(1, id).Batches[0].Jobs[0].ManualOpen }, 3*time.Second, 10*time.Millisecond)
	_, _, err = s.ManualFrame(context.Background(), 2, id, job.ID)
	require.Error(t, err)
	require.Error(t, s.ManualFinish(context.Background(), 1, id, job.ID)) // CAPTCHA not solved.
	human.mu.Lock()
	human.manual = false
	human.captureEmail = "other@example.test"
	human.mu.Unlock()
	require.ErrorContains(t, s.ManualFinish(context.Background(), 1, id, job.ID), "不一致")
	human.mu.Lock()
	human.captureEmail = "owner@example.test"
	human.mu.Unlock()
	require.NoError(t, s.ManualFinish(context.Background(), 1, id, job.ID))
	require.Eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.runs) == 0 }, time.Second, 10*time.Millisecond)
	view := s.View(1, id).Batches[0].Jobs[0]
	require.Equal(t, "succeeded", view.Status)
	require.False(t, view.HasCredentials)
	require.True(t, human.closed.Load())
	admin.mu.Lock()
	require.Equal(t, 1, admin.creates)
	admin.mu.Unlock()
	_, err = s.store.LoadFrame(job.ID)
	require.Error(t, err)
}

func TestPlaygroundBatchKeepsAttendedChallengeOpenAfterResume(t *testing.T) {
	s, _, _ := batchFixture(t)
	var starts atomic.Int32
	human := &batchFakeBrowser{manual: true}
	s.startBrowser = func(_ context.Context, _ string, _ string, manual bool, resume string) (batchBrowser, error) {
		starts.Add(1)
		if manual {
			require.Contains(t, resume, "https://accounts.google.com/")
			return human, nil
		}
		return &batchFakeBrowser{manual: true}, nil
	}
	id, err := s.Create(context.Background(), 1, PlaygroundBatchRequest{Accounts: "owner@example.test----synthetic----", ProxyIDs: []int64{1}, Concurrency: 10})
	require.NoError(t, err)
	require.Eventually(t, func() bool { v := s.View(1, id); return v.Batches[0].Counts["manual"] == 1 && v.Running == 0 }, 5*time.Second, 10*time.Millisecond)
	job := s.View(1, id).Batches[0].Jobs[0]
	require.NoError(t, s.Action(1, id, job.ID, "open_manual"))
	require.Eventually(t, func() bool { return s.View(1, id).Batches[0].Jobs[0].ManualOpen }, 3*time.Second, 10*time.Millisecond)
	require.NoError(t, s.Action(1, id, job.ID, "continue_auto"))
	require.Eventually(t, func() bool { j := s.View(1, id).Batches[0].Jobs[0]; return j.Status == "manual" && j.ManualOpen }, 3*time.Second, 10*time.Millisecond)
	require.False(t, human.closed.Load())
	require.EqualValues(t, 2, starts.Load())
	require.NoError(t, s.Action(1, id, job.ID, "close_manual"))
	require.Eventually(t, human.closed.Load, time.Second, 10*time.Millisecond)
}
func (b *batchFakeBrowser) Screenshot(context.Context) (string, error) {
	return "synthetic-private-frame", nil
}
func (b *batchFakeBrowser) Input(context.Context, camoufox.Input) error { return nil }
func (b *batchFakeBrowser) LoginURL(context.Context) (string, error) {
	return "https://accounts.google.com/signin/challenge?state=synthetic", nil
}
func (b *batchFakeBrowser) Close() { b.closed.Store(true) }

func batchFixture(t *testing.T) (*PlaygroundBatchService, *batchAdminFixture, *batchProxyFixture) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("DATA_DIR", root)
	t.Setenv("PLAYGROUND_PROFILE_DIR", "")
	redisServer := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	proxies := &batchProxyFixture{proxies: []Proxy{{ID: 1, Protocol: "http", Host: "127.0.0.1", Port: 32123, Status: StatusActive}}}
	browser := NewPlaygroundBrowserService(&aistudio.SessionCache{Client: client}, proxies)
	admin := &batchAdminFixture{browser: browser}
	cfg := &config.Config{}
	cfg.Totp.EncryptionKey = strings.Repeat("31", 32)
	cfg.Totp.EncryptionKeyConfigured = true
	s, err := NewPlaygroundBatchService(browser, admin, cfg)
	require.NoError(t, err)
	s.mu.Lock()
	s.checkProxy = func(context.Context, *Proxy) error { return nil }
	s.capacity = func() camoufox.BrowserCapacity { return camoufox.BrowserCapacity{Configured: 128, Effective: 128} }
	s.startBrowser = func(context.Context, string, string, bool, string) (batchBrowser, error) {
		return &batchFakeBrowser{}, nil
	}
	s.mu.Unlock()
	t.Cleanup(func() { s.Close(); browser.Close() })
	return s, admin, proxies
}

func TestPlaygroundBatchRequiresProxyAndKeepsPublicStateSecretFree(t *testing.T) {
	s, _, _ := batchFixture(t)
	request := PlaygroundBatchRequest{Accounts: "owner@example.test----private-password-marker----JBSWY3DPEHPK3PXP", Concurrency: 20}
	_, err := s.Create(context.Background(), 1, request)
	require.ErrorContains(t, err, "代理")
	request.ProxyIDs = []int64{1}
	id, err := s.Create(context.Background(), 1, request)
	require.NoError(t, err)
	view := s.View(1, id)
	raw, err := json.Marshal(view)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "private-password-marker")
	require.NotContains(t, string(raw), "JBSWY3DPEHPK3PXP")
	require.Empty(t, s.View(2, id).Batches)
	require.Error(t, s.Action(2, id, "", "cancel"))
}

func TestPlaygroundBatchConfiguresTwentyConcurrentTasksWithSeparateProxies(t *testing.T) {
	s, _, proxies := batchFixture(t)
	ids := []int64{}
	lines := []string{}
	proxies.mu.Lock()
	proxies.proxies = nil
	for i := 1; i <= 20; i++ {
		id := int64(i)
		ids = append(ids, id)
		proxies.proxies = append(proxies.proxies, Proxy{ID: id, Protocol: "http", Host: "127.0.0.1", Port: 32000 + i, Status: StatusActive})
		lines = append(lines, fmt.Sprintf("user%d@example.test----synthetic----", i))
	}
	proxies.mu.Unlock()
	release := make(chan struct{})
	var started atomic.Int32
	s.startBrowser = func(ctx context.Context, _ string, _ string, _ bool, _ string) (batchBrowser, error) {
		started.Add(1)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return &batchFakeBrowser{}, nil
		}
	}
	id, err := s.Create(context.Background(), 1, PlaygroundBatchRequest{Accounts: strings.Join(lines, "\n"), ProxyIDs: ids, Concurrency: 20})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return started.Load() == 20 }, 5*time.Second, 10*time.Millisecond)
	s.mu.Lock()
	require.Len(t, s.proxyBusy, 20)
	s.mu.Unlock()
	close(release)
	require.Eventually(t, func() bool { return s.View(1, id).Batches[0].Counts["succeeded"] == 20 }, 10*time.Second, 10*time.Millisecond)
}

func TestPlaygroundBatchManualTaskReleasesBrowserAndQueueContinues(t *testing.T) {
	s, admin, _ := batchFixture(t)
	var count atomic.Int32
	first := &batchFakeBrowser{manual: true}
	s.startBrowser = func(context.Context, string, string, bool, string) (batchBrowser, error) {
		if count.Add(1) == 1 {
			return first, nil
		}
		return &batchFakeBrowser{}, nil
	}
	id, err := s.Create(context.Background(), 1, PlaygroundBatchRequest{Accounts: "one@example.test----p----\ntwo@example.test----p----", ProxyIDs: []int64{1}, Concurrency: 20})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		v := s.View(1, id)
		return len(v.Batches) == 1 && v.Batches[0].Counts["manual"] == 1 && v.Batches[0].Counts["succeeded"] == 1
	}, 5*time.Second, 10*time.Millisecond)
	require.True(t, first.closed.Load())
	admin.mu.Lock()
	require.Equal(t, 1, admin.creates)
	admin.mu.Unlock()
	view := s.View(1, id)
	for _, job := range view.Batches[0].Jobs {
		if job.Status == "succeeded" {
			require.False(t, job.HasCredentials)
		} else {
			frame, live, err := s.ManualFrame(context.Background(), 1, id, job.ID)
			require.NoError(t, err)
			require.False(t, live)
			require.Equal(t, "synthetic-private-frame", frame)
		}
	}
}

func TestPlaygroundBatchLostDatabaseReplyDoesNotDuplicateAccount(t *testing.T) {
	s, admin, _ := batchFixture(t)
	admin.lostReply = true
	id, err := s.Create(context.Background(), 1, PlaygroundBatchRequest{Accounts: "owner@example.test----p----", ProxyIDs: []int64{1}, Concurrency: 10})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return s.View(1, id).Batches[0].Counts["succeeded"] == 1 }, 5*time.Second, 10*time.Millisecond)
	admin.mu.Lock()
	require.Equal(t, 1, admin.creates)
	require.Len(t, admin.accounts, 1)
	require.Equal(t, true, admin.accounts[0].Credentials["playground_require_proxy"])
	require.EqualValues(t, 1, *admin.accounts[0].ProxyID)
	admin.mu.Unlock()
	id2, err := s.Create(context.Background(), 1, PlaygroundBatchRequest{Accounts: "owner@example.test----p----", ProxyIDs: []int64{1}, Concurrency: 10})
	require.NoError(t, err)
	require.Equal(t, 1, s.View(1, id2).Batches[0].Counts["skipped"])
}

func TestPlaygroundBatchCancellationPersistsAndClearsSecrets(t *testing.T) {
	s, _, _ := batchFixture(t)
	entered := make(chan struct{})
	s.startBrowser = func(ctx context.Context, _ string, _ string, _ bool, _ string) (batchBrowser, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	id, err := s.Create(context.Background(), 1, PlaygroundBatchRequest{Accounts: "owner@example.test----private-password-marker----", ProxyIDs: []int64{1}, Concurrency: 10})
	require.NoError(t, err)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not start")
	}
	require.NoError(t, s.Action(1, id, "", "cancel"))
	require.Eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.runs) == 0 }, time.Second, 10*time.Millisecond)
	loaded, err := s.store.Load()
	require.NoError(t, err)
	require.Nil(t, loaded.Batches[0].Jobs[0].Secret)
	require.Equal(t, "cancelled", loaded.Batches[0].Jobs[0].Status)
}

func TestPlaygroundBatchRestoredSubmissionStateAndNoDirectFallback(t *testing.T) {
	s, _, _ := batchFixture(t)
	profile := uuid.NewString()
	job := &playgroundBatchJob{ID: uuid.NewString(), Email: "owner@example.test", Profile: profile, Status: "running", Secret: &batchSecret{Password: "private-password-marker"}, SecretExpires: time.Now().Add(time.Hour), Progress: camoufox.GoogleLoginProgress{Submitted: map[string]time.Time{"password": time.Now().Add(-time.Minute)}}}
	s.mu.Lock()
	s.state.Batches = []*playgroundBatch{{ID: uuid.NewString(), Owner: 1, Paused: true, ProxyIDs: []int64{1}, Jobs: []*playgroundBatchJob{job}}}
	require.NoError(t, s.saveLocked())
	s.mu.Unlock()
	s.Close()
	loaded, err := s.store.Load()
	require.NoError(t, err)
	require.Equal(t, "queued", loaded.Batches[0].Jobs[0].Status)
	require.Contains(t, loaded.Batches[0].Jobs[0].Progress.Submitted, "password")
	repo, account := newPlaygroundTestAccount(t)
	repo.account.Credentials["playground_require_proxy"] = true
	gateway, _ := newPlaygroundTestGateway(t, repo)
	_, _, err = gateway.acquireSession(context.Background(), account)
	require.ErrorContains(t, err, "有效代理")
}

func TestPlaygroundBatchRestartExpiresSecretsBeforeDispatch(t *testing.T) {
	s, admin, _ := batchFixture(t)
	id, jobID := uuid.NewString(), uuid.NewString()
	s.mu.Lock()
	s.state.Batches = []*playgroundBatch{{ID: id, Owner: 1, ProxyIDs: []int64{1}, Jobs: []*playgroundBatchJob{{
		ID: jobID, Email: "owner@example.test", Profile: uuid.NewString(), Status: "running", ProxyID: 1,
		Secret: &batchSecret{Password: "expired-private-marker"}, SecretExpires: time.Now().Add(-time.Minute),
		Progress: camoufox.GoogleLoginProgress{Submitted: map[string]time.Time{"password": time.Now().Add(-time.Hour)}},
	}}}}
	require.NoError(t, s.saveLocked())
	s.mu.Unlock()
	s.Close()
	cfg := &config.Config{}
	cfg.Totp.EncryptionKey, cfg.Totp.EncryptionKeyConfigured = strings.Repeat("31", 32), true
	restarted, err := NewPlaygroundBatchService(s.browser, admin, cfg)
	require.NoError(t, err)
	defer restarted.Close()
	view := restarted.View(1, id)
	require.Equal(t, "failed", view.Batches[0].Jobs[0].Status)
	require.Equal(t, batchReason("expired"), view.Batches[0].Jobs[0].Reason)
	require.False(t, view.Batches[0].Jobs[0].HasCredentials)
	loaded, err := restarted.store.Load()
	require.NoError(t, err)
	require.Nil(t, loaded.Batches[0].Jobs[0].Secret)
	require.Contains(t, loaded.Batches[0].Jobs[0].Progress.Submitted, "password")
}

func TestPlaygroundBatchRevokedOwnerNeverStartsLogin(t *testing.T) {
	s, admin, _ := batchFixture(t)
	s.ownerActive = func(context.Context, int64) bool { return false }
	var starts atomic.Int32
	s.startBrowser = func(context.Context, string, string, bool, string) (batchBrowser, error) {
		starts.Add(1)
		return &batchFakeBrowser{}, nil
	}
	id, err := s.Create(context.Background(), 1, PlaygroundBatchRequest{Accounts: "owner@example.test----synthetic----", ProxyIDs: []int64{1}, Concurrency: 2})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		view := s.View(1, id)
		return view.Running == 0 && view.Batches[0].Jobs[0].Reason == batchReason("owner_inactive")
	}, 3*time.Second, 10*time.Millisecond)
	require.Zero(t, starts.Load(), "a revoked owner cannot launch a credential-bearing browser")
	admin.mu.Lock()
	defer admin.mu.Unlock()
	require.Zero(t, admin.creates)
}
