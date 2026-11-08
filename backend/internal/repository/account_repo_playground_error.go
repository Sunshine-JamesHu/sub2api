package repository

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// SetPlaygroundErrorIfUnchanged never quarantines a profile reauthorized or
// edited after the request acquired its session. The scheduler notification is
// part of the same statement, so a committed block cannot lose its outbox event.
func (r *accountRepository) SetPlaygroundErrorIfUnchanged(ctx context.Context, id int64, credentials map[string]any, proxyID *int64, updatedAt time.Time, message string) (bool, error) {
	if r == nil || r.sql == nil {
		return false, errors.New("account repository SQL executor is not configured")
	}
	raw, err := json.Marshal(normalizeJSONMap(credentials))
	if err != nil {
		return false, err
	}
	result, err := r.sql.ExecContext(ctx, `
		WITH updated AS (
		UPDATE accounts AS a
		SET status = $1, schedulable = FALSE, error_message = $2, updated_at = NOW()
		WHERE a.id = $3 AND a.deleted_at IS NULL
			AND a.platform = $4 AND a.type = $5
			AND a.status = $6 AND a.schedulable = TRUE
			AND a.credentials = $7::jsonb
			AND a.proxy_id IS NOT DISTINCT FROM $8
			AND a.updated_at = $9
		RETURNING a.id
		)
		INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload)
		SELECT $10, updated.id, NULL, NULL FROM updated
	`, service.StatusError, message, id, service.PlatformGemini, service.AccountTypePlayground,
		service.StatusActive, string(raw), proxyID, updatedAt, service.SchedulerOutboxEventAccountChanged)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil || count == 0 {
		return false, err
	}
	r.syncSchedulerAccountSnapshotDetached(ctx, id)
	return true, nil
}
