package aistudio

import (
	"context"
	"time"
)

func permissionStreakKey(profile, model string) string {
	return "playground:{" + profile + "}:permission:" + ComputePayloadDigest(model)
}

// PermissionFailure shares a bounded backoff across instances. Counts survive a
// cooldown, but expire after a day without another failure or on generation success.
func (c *SessionCache) PermissionFailure(ctx context.Context, profile, model string) (time.Duration, int64, error) {
	count, err := c.Client.Eval(ctx, `
local n = math.min(4, (tonumber(redis.call('GET', KEYS[1])) or 0) + 1)
redis.call('SET', KEYS[1], n, 'EX', 86400)
return n`, []string{permissionStreakKey(profile, model)}).Int64()
	if err != nil {
		return time.Minute, 0, err
	}
	delays := [...]time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute}
	return delays[count-1], count, nil
}

// PermissionSuccess returns whether this profile/model had a failure streak.
func (c *SessionCache) PermissionSuccess(ctx context.Context, profile, model string) (bool, error) {
	deleted, err := c.Client.Del(ctx, permissionStreakKey(profile, model)).Result()
	return deleted > 0, err
}
