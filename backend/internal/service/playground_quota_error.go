package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

func playgroundQuotaRPCError(err error) *aistudio.RPCError {
	var rpc *aistudio.RPCError
	if errors.As(err, &rpc) && rpc.Method == "GenerateContent" &&
		(rpc.StatusCode == http.StatusTooManyRequests || (rpc.StatusCode == http.StatusOK && rpc.Code == 8)) {
		return rpc
	}
	return nil
}

// RPC quota errors are returned as Go errors, including HTTP200 stream trailers.
// Persist them here before compatibility handlers turn failures into failover.
func (s *PlaygroundGatewayService) markGenerationQuotaError(ctx context.Context, account *Account, model string, err error) {
	if ctx.Err() != nil || account == nil || account.Platform != PlatformGemini || account.Type != AccountTypePlayground || s.accountRepo == nil || strings.TrimSpace(model) == "" {
		return
	}
	rpc := playgroundQuotaRPCError(err)
	if rpc == nil {
		return
	}
	decision := aistudio.ResolveQuotaCooldown(http.StatusTooManyRequests, []byte(rpc.Message))
	// Playground quotas are tracked by the actual upstream model. An exhausted
	// Flash allocation must not block Argon or permanently disable the account.
	if persistErr := s.accountRepo.SetModelRateLimit(ctx, account.ID, model, decision.ResetAt, "Playground model quota: "+decision.Reason); persistErr != nil {
		logger.FromContext(ctx).Warn("playground.quota_cooldown_update_failed", zap.Int64("account_id", account.ID), zap.String("model", model), zap.Error(persistErr))
		return
	}
	logger.FromContext(ctx).Warn("playground.quota_model_cooldown", zap.Int64("account_id", account.ID), zap.String("model", model), zap.Time("reset_at", decision.ResetAt), zap.Int64("cooldown_seconds", int64(time.Until(decision.ResetAt).Seconds())), zap.String("reason", decision.Reason))
}

func playgroundQuotaFailover(err error) *UpstreamFailoverError {
	rpc := playgroundQuotaRPCError(err)
	if rpc == nil {
		return nil
	}
	body, _ := json.Marshal(map[string]any{"error": map[string]any{"code": http.StatusTooManyRequests, "status": "RESOURCE_EXHAUSTED", "message": rpc.Message}})
	return &UpstreamFailoverError{StatusCode: http.StatusTooManyRequests, ResponseBody: body}
}
