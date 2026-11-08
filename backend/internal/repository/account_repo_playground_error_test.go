package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestPlaygroundErrorCASPreservesIdentityRevisionAndManualPause(t *testing.T) {
	for _, count := range []int64{0, 1} {
		t.Run(map[int64]string{0: "stale-or-paused", 1: "blocked"}[count], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			stamp := time.Now().UTC()
			proxy := int64(3)
			mock.ExpectExec(`(?s)WITH updated AS.*SET status = \$1, schedulable = FALSE, error_message = \$2.*a.deleted_at IS NULL.*a.platform = \$4 AND a.type = \$5.*a.status = \$6 AND a.schedulable = TRUE.*a.credentials = \$7::jsonb.*a.proxy_id IS NOT DISTINCT FROM \$8.*a.updated_at = \$9.*INSERT INTO scheduler_outbox.*SELECT \$10, updated.id`).
				WithArgs(service.StatusError, "[playground:login_required]", int64(70), service.PlatformGemini, service.AccountTypePlayground, service.StatusActive, `{"playground_profile_id":"profile"}`, proxy, stamp, service.SchedulerOutboxEventAccountChanged).
				WillReturnResult(sqlmock.NewResult(0, count))
			r := newAccountRepositoryWithSQL(nil, db, nil)
			applied, err := r.SetPlaygroundErrorIfUnchanged(context.Background(), 70, map[string]any{"playground_profile_id": "profile"}, &proxy, stamp, "[playground:login_required]")
			require.NoError(t, err)
			require.Equal(t, count > 0, applied)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestPlaygroundErrorCASDoesNotClaimFailedWrite(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	failure := errors.New("database unavailable")
	mock.ExpectExec(`WITH updated AS`).WillReturnError(failure)
	r := newAccountRepositoryWithSQL(nil, db, nil)
	applied, err := r.SetPlaygroundErrorIfUnchanged(context.Background(), 70, nil, nil, time.Now(), "login")
	require.ErrorIs(t, err, failure)
	require.False(t, applied)
	require.NoError(t, mock.ExpectationsWereMet())
}
