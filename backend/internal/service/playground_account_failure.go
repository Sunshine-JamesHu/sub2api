package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	"github.com/Wei-Shaw/sub2api/internal/pkg/camoufox"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/logredact"
	"go.uber.org/zap"
)

// No unconditional SetError fallback: an old request must never disable a newly
// authorized profile or overwrite an operator's more recent account edit.
type playgroundConditionalErrorRepository interface {
	SetPlaygroundErrorIfUnchanged(context.Context, int64, map[string]any, *int64, time.Time, string) (bool, error)
}

func playgroundManualActionReason(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ""
	}
	switch {
	case errors.Is(err, errPlaygroundAuthorizationMissing):
		return "authorization_missing"
	case errors.Is(err, camoufox.ErrInteractiveLoginRequired):
		return "login_required"
	case errors.Is(err, camoufox.ErrAccountMismatch):
		return "account_mismatch"
	case errors.Is(err, camoufox.ErrRegionNotSupported):
		return "unsupported_egress"
	}
	var rpc *aistudio.RPCError
	if !errors.As(err, &rpc) || (rpc.Method != "GenerateContent" && rpc.Method != "ListModels") ||
		rpc.Code != 7 || (rpc.StatusCode != http.StatusForbidden && rpc.StatusCode != http.StatusOK) {
		return ""
	}
	// These are the explicit upstream messages recognized by AIStudio2API2.
	// The generic "caller does not have permission" is deliberately excluded,
	// regardless of the number of failures: healthy accounts return it too.
	switch strings.TrimSpace(rpc.Message) {
	case "Account not supported.":
		return "unsupported_account"
	case "Region not supported.":
		return "unsupported_egress"
	default:
		return ""
	}
}

func (s *PlaygroundGatewayService) markManualActionFailure(ctx context.Context, account *Account, err error) {
	if err == nil || ctx.Err() != nil || account == nil || account.Platform != PlatformGemini || account.Type != AccountTypePlayground {
		return
	}
	reason := playgroundManualActionReason(err)
	if reason == "" {
		return
	}
	repo, ok := s.accountRepo.(playgroundConditionalErrorRepository)
	if !ok {
		logger.FromContext(ctx).Warn("playground.account_block_repository_unavailable", zap.Int64("account_id", account.ID), zap.String("reason", reason))
		return
	}
	message := "[playground:" + reason + "] 自动调度已停止，需重新授权、修复账户/代理后人工恢复；不会定时自动解封。原因: " + err.Error()
	message = truncateString(logredact.RedactText(message, "api_key", "authorization"), 2048)
	applied, persistErr := repo.SetPlaygroundErrorIfUnchanged(ctx, account.ID, account.Credentials, account.ProxyID, account.UpdatedAt, message)
	if persistErr != nil {
		logger.FromContext(ctx).Warn("playground.account_block_failed", zap.Int64("account_id", account.ID), zap.String("reason", reason), zap.Error(persistErr))
		return
	}
	logger.FromContext(ctx).Warn("playground.account_manual_action", zap.Int64("account_id", account.ID), zap.String("reason", reason), zap.Bool("applied", applied))
}
