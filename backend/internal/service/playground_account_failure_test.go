package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	"github.com/Wei-Shaw/sub2api/internal/pkg/camoufox"
	"github.com/stretchr/testify/require"
)

func (r *playgroundTestRepository) SetPlaygroundErrorIfUnchanged(ctx context.Context, id int64, credentials map[string]any, proxyID *int64, updatedAt time.Time, message string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if id != r.account.ID || playgroundCredentialVersion(credentials) != playgroundCredentialVersion(r.account.Credentials) ||
		!r.account.UpdatedAt.Equal(updatedAt) || r.account.Status != StatusActive || !r.account.Schedulable ||
		(proxyID == nil) != (r.account.ProxyID == nil) || (proxyID != nil && *proxyID != *r.account.ProxyID) {
		return false, nil
	}
	r.account.Status, r.account.Schedulable, r.account.ErrorMessage = StatusError, false, message
	r.errorsRecorded++
	return true, nil
}

func TestPlaygroundManualActionClassification(t *testing.T) {
	rpc := func(method string, status, code int, msg string) error {
		return &aistudio.RPCError{Method: method, StatusCode: status, Code: code, Message: msg}
	}
	for _, tc := range []struct {
		name   string
		err    error
		reason string
	}{
		{"browser login", camoufox.ErrInteractiveLoginRequired, "login_required"},
		{"missing authorization", errPlaygroundAuthorizationMissing, "authorization_missing"},
		{"wrapped login", fmt.Errorf("recover: %w", camoufox.ErrInteractiveLoginRequired), "login_required"},
		{"identity", camoufox.ErrAccountMismatch, "account_mismatch"},
		{"trusted region page", camoufox.ErrRegionNotSupported, "unsupported_egress"},
		{"unsupported account", rpc("GenerateContent", 403, 7, "Account not supported."), "unsupported_account"},
		{"unsupported account trailer", rpc("GenerateContent", 200, 7, "Account not supported."), "unsupported_account"},
		{"region", rpc("ListModels", 403, 7, "Region not supported."), "unsupported_egress"},
		{"generic permission", rpc("GenerateContent", 403, 7, "The caller does not have permission"), ""},
		{"permission trailer", rpc("GenerateContent", 200, 7, "The caller does not have permission"), ""},
		{"unconfirmed auth", aistudio.ErrAuthenticationRequired, ""},
		{"401 before recovery", rpc("GenerateContent", 401, 16, "expired"), ""},
		{"optional Drive", rpc("GenerateAccessToken", 401, 16, "OAuth error: unauthorized_client."), ""},
		{"other bootstrap", rpc("ListPrompts", 403, 7, "Account not supported."), ""},
		{"quota", rpc("GenerateContent", 429, 8, "quota exhausted"), ""},
		{"invalid input", rpc("GenerateContent", 400, 3, "Account not supported."), ""},
		{"model not found", rpc("GenerateContent", 404, 5, "not found"), ""},
		{"server failure", rpc("GenerateContent", 503, 7, "Account not supported."), ""},
		{"unknown account text", rpc("GenerateContent", 403, 7, "Account not supported. Please retry later."), ""},
		{"untyped login text", errors.New(camoufox.ErrInteractiveLoginRequired.Error()), ""},
		{"profile volume", errors.New("Camoufox [profile-missing]: check shared volume"), ""},
		{"timeout", context.DeadlineExceeded, ""},
		{"cancelled confirmation", errors.Join(camoufox.ErrInteractiveLoginRequired, context.Canceled), ""},
		{"expired confirmation", errors.Join(camoufox.ErrInteractiveLoginRequired, context.DeadlineExceeded), ""},
		{"network", errors.New("proxy TLS handshake failed"), ""},
		{"success", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.reason, playgroundManualActionReason(tc.err))
			repo, account := newPlaygroundTestAccount(t)
			repo.account.Status, repo.account.Schedulable = StatusActive, true
			gateway := NewPlaygroundGatewayService(repo)
			gateway.markManualActionFailure(context.Background(), account, tc.err)
			if tc.reason == "" {
				require.Zero(t, repo.errorsRecorded)
				require.True(t, repo.account.Schedulable)
			} else {
				require.Equal(t, 1, repo.errorsRecorded)
				require.False(t, repo.account.Schedulable)
				require.Equal(t, StatusError, repo.account.Status)
				require.Contains(t, repo.account.ErrorMessage, "[playground:"+tc.reason+"]")
			}
		})
	}
}

func TestPlaygroundManualActionProtectsNewerAccountState(t *testing.T) {
	for _, change := range []string{"reauthorized", "proxy", "edited", "paused", "already error", "cancelled", "other platform", "other type"} {
		t.Run(change, func(t *testing.T) {
			repo, account := newPlaygroundTestAccount(t)
			repo.account.Status, repo.account.Schedulable = StatusActive, true
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch change {
			case "reauthorized":
				repo.account.Credentials["playground_profile_id"] = "new-profile"
			case "proxy":
				id := int64(45)
				repo.account.ProxyID = &id
			case "edited":
				repo.account.UpdatedAt = time.Now()
			case "paused":
				repo.account.Schedulable = false
			case "already error":
				repo.account.Status = StatusError
			case "cancelled":
				cancel()
			case "other platform":
				account.Platform = PlatformOpenAI
			case "other type":
				account.Type = AccountTypeAPIKey
			}
			NewPlaygroundGatewayService(repo).markManualActionFailure(ctx, account, camoufox.ErrInteractiveLoginRequired)
			require.Zero(t, repo.errorsRecorded)
		})
	}
}

func TestPlaygroundGenericPermissionNeverPermanentlyBlocksByCount(t *testing.T) {
	repo, account := newPlaygroundTestAccount(t)
	repo.account.Status, repo.account.Schedulable = StatusActive, true
	gateway := NewPlaygroundGatewayService(repo)
	for i := 0; i < 150; i++ {
		gateway.markManualActionFailure(context.Background(), account, &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusForbidden, Code: 7, Message: "The caller does not have permission"})
	}
	require.Zero(t, repo.errorsRecorded)
	require.True(t, repo.account.Schedulable)
}

type playgroundUnconditionalErrorRepository struct {
	AccountRepository
	writes int
}

func (r *playgroundUnconditionalErrorRepository) SetError(context.Context, int64, string) error {
	r.writes++
	return nil
}

func TestPlaygroundManualActionNeverFallsBackToUnconditionalWrite(t *testing.T) {
	repo := &playgroundUnconditionalErrorRepository{}
	_, account := newPlaygroundTestAccount(t)
	NewPlaygroundGatewayService(repo).markManualActionFailure(context.Background(), account, camoufox.ErrInteractiveLoginRequired)
	require.Zero(t, repo.writes)
}

func TestPlaygroundManualActionCannotBeUndoneByModelSuccess(t *testing.T) {
	repo, account := newPlaygroundTestAccount(t)
	repo.account.Status, repo.account.Schedulable = StatusActive, true
	gateway, _ := newPlaygroundTestGateway(t, repo)
	ctx := context.Background()
	_, _, err := gateway.browser.Cache.PermissionFailure(ctx, account.GetCredential("playground_profile_id"), "flash")
	require.NoError(t, err)
	gateway.markManualActionFailure(ctx, account, camoufox.ErrInteractiveLoginRequired)
	// A late successful in-flight generation can clear only the model cooldown.
	// It cannot restore account-level scheduling or erase the operator-action error.
	gateway.markGenerationSuccess(ctx, account, "flash")
	require.Equal(t, StatusError, repo.account.Status)
	require.False(t, repo.account.Schedulable)
	require.Contains(t, repo.account.ErrorMessage, "login_required")
}
