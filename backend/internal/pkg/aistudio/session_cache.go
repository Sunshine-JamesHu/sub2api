package aistudio

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const SessionCacheTTL = 10 * time.Minute
const sessionLeaseTTL = 30 * time.Second

var ErrSessionLeaseLost = errors.New("Playground session lease lost; retry the request")

// SessionCache deliberately has no in-memory or database fallback. Redis is the
// authority for exported cookies; the browser profile is the recovery source.
type SessionCache struct{ Client *redis.Client }

// BrowserGrant contains identity/configuration only, never cookies. It is issued
// after interactive login and consumed once by account creation or reauthorization.
type BrowserGrant struct {
	Profile string `json:"profile"`
	Email   string `json:"email"`
	Proxy   string `json:"proxy"`
}

func (c *SessionCache) IssueGrant(ctx context.Context, g BrowserGrant) (string, error) {
	token := uuid.NewString()
	raw, err := json.Marshal(g)
	if err != nil {
		return "", err
	}
	err = c.Client.Set(ctx, "playground:login:"+token, raw, 30*time.Minute).Err()
	return token, err
}
func (c *SessionCache) ConsumeGrant(ctx context.Context, token string) (BrowserGrant, error) {
	var g BrowserGrant
	if _, err := uuid.Parse(token); err != nil {
		return g, errors.New("invalid browser login token")
	}
	raw, err := c.Client.GetDel(ctx, "playground:login:"+token).Bytes()
	if err != nil {
		return g, errors.New("browser login expired or already saved; log in again")
	}
	err = json.Unmarshal(raw, &g)
	return g, err
}

type SessionLease struct {
	cache      *SessionCache
	key, owner string
}

func (c *SessionCache) Acquire(ctx context.Context, profile string) (*SessionLease, error) {
	l := &SessionLease{cache: c, key: "playground:{" + profile + "}:lease", owner: uuid.NewString()}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		ok, err := c.Client.SetNX(ctx, l.key, l.owner, sessionLeaseTTL).Result()
		if err != nil {
			return nil, err
		}
		if ok {
			return l, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (l *SessionLease) script(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	return l.cache.Client.Eval(ctx, `if redis.call('GET',KEYS[1]) ~= ARGV[1] then return redis.error_reply('PLAYGROUND_LEASE_LOST') end `+script,
		append([]string{l.key}, keys...), append([]any{l.owner}, args...)...).Result()
}
func (l *SessionLease) Renew(ctx context.Context) error {
	_, err := l.script(ctx, `return redis.call('PEXPIRE',KEYS[1],ARGV[2])`, nil, sessionLeaseTTL.Milliseconds())
	return err
}
func (l *SessionLease) Release(ctx context.Context) {
	_, _ = l.script(ctx, `return redis.call('DEL',KEYS[1])`, nil)
}
func (l *SessionLease) Get(ctx context.Context, key string) (StorageState, bool, error) {
	blocked, err := l.script(ctx, `return redis.call('EXISTS',KEYS[2])`, []string{key + ":login-required"})
	if err != nil {
		return StorageState{}, false, err
	}
	if blocked == int64(1) {
		return StorageState{}, false, ErrAuthenticationRequired
	}
	raw, err := l.script(ctx, `local v=redis.call('GET',KEYS[2]); if v then redis.call('PEXPIRE',KEYS[2],ARGV[2]) end; return v`, []string{key}, SessionCacheTTL.Milliseconds())
	if errors.Is(err, redis.Nil) {
		return StorageState{}, false, nil
	}
	if err != nil {
		return StorageState{}, false, err
	}
	var state StorageState
	value, ok := raw.(string)
	if !ok {
		return state, false, errors.New("invalid Playground cache response")
	}
	if err = json.Unmarshal([]byte(value), &state); err != nil {
		return state, false, err
	}
	if err = state.Validate(); err != nil {
		return state, false, err
	}
	return state, true, nil
}

// A logged-out profile needs interactive reauthorization, not a new browser on
// every incoming request. Successful reauthorization uses a new profile ID.
func (l *SessionLease) RequireLogin(ctx context.Context, key string) error {
	_, err := l.script(ctx, `redis.call('DEL',KEYS[2]); return redis.call('SET',KEYS[3],'1','EX',86400)`, []string{key, key + ":login-required"})
	return err
}
func (l *SessionLease) Put(ctx context.Context, key string, state StorageState) error {
	if err := state.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = l.script(ctx, `return redis.call('SET',KEYS[2],ARGV[2],'PX',ARGV[3])`, []string{key}, string(raw), SessionCacheTTL.Milliseconds())
	return err
}
func (l *SessionLease) Delete(ctx context.Context, key string) error {
	_, err := l.script(ctx, `return redis.call('DEL',KEYS[2])`, []string{key})
	return err
}

// DeleteProfileSessions evicts every proxy/email variant and login-required
// marker for this profile, without releasing its lease.
func (l *SessionLease) DeleteProfileSessions(ctx context.Context) error {
	pattern := strings.TrimSuffix(l.key, ":lease") + ":session:*"
	iter := l.cache.Client.Scan(ctx, 0, pattern, 100).Iterator()
	for iter.Next(ctx) {
		if err := l.Delete(ctx, iter.Val()); err != nil {
			return err
		}
	}
	return iter.Err()
}
