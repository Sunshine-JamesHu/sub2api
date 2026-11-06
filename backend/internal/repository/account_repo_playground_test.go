package repository

import (
	"context"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestPlaygroundCredentialCASIncludesProxyAndOutbox(t *testing.T) {
	for _, updated := range []int64{0, 1} {
		t.Run(map[int64]string{0: "stale", 1: "updated"}[updated], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			proxyID := int64(45)
			mock.ExpectExec(`(?s)WITH updated AS.*UPDATE accounts AS a.*a.platform = \$3.*a.type = \$4.*a.credentials = \$5::jsonb.*a.proxy_id IS NOT DISTINCT FROM \$6.*INSERT INTO scheduler_outbox.*FROM updated`).
				WithArgs(`{"cookies":"next"}`, int64(123), service.PlatformGemini, service.AccountTypePlayground, `{"cookies":"previous"}`, proxyID, service.SchedulerOutboxEventAccountChanged).
				WillReturnResult(sqlmock.NewResult(0, updated))
			repo := newAccountRepositoryWithSQL(nil, db, nil)
			applied, err := repo.UpdatePlaygroundCredentialsIfUnchanged(context.Background(), 123, map[string]any{"cookies": "previous"}, &proxyID, map[string]any{"cookies": "next"})
			require.NoError(t, err)
			require.Equal(t, updated > 0, applied)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
