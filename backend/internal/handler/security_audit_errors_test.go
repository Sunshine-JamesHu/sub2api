package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/securityaudit"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func promptGuardDecision(kind securityaudit.DecisionKind) *securityaudit.Decision {
	decision := &securityaudit.Decision{Kind: kind, AllowNextStage: false}
	switch kind {
	case securityaudit.DecisionBlock:
		decision.HTTPStatus = http.StatusForbidden
		decision.ErrorCode = securityaudit.ErrorCodeBlocked
		decision.ClientMessage = "提示词安全审计拒绝了该请求，请调整输入后重试"
	case securityaudit.DecisionInvalid:
		decision.HTTPStatus = http.StatusServiceUnavailable
		decision.ErrorCode = securityaudit.ErrorCodeInvalidResponse
		decision.ClientMessage = "提示词安全审计暂时不可用，请稍后重试"
	default:
		decision.HTTPStatus = http.StatusServiceUnavailable
		decision.ErrorCode = securityaudit.ErrorCodeUnavailable
		decision.ClientMessage = "提示词安全审计暂时不可用，请稍后重试"
	}
	return decision
}

func securityAuditErrorTestContext(t *testing.T) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	ctx := context.WithValue(context.Background(), ctxkey.RequestID, "request-error-golden")
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/test", nil).WithContext(ctx)
	return c, recorder
}

func decodeErrorJSON(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	return payload
}

func requireObject(t *testing.T, value any) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	require.True(t, ok)
	return object
}

func requireArray(t *testing.T, value any) []any {
	t.Helper()
	array, ok := value.([]any)
	require.True(t, ok)
	return array
}

func TestPromptGuardOpenAIAndClaudeErrorEnvelopesGolden(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, kind := range []securityaudit.DecisionKind{securityaudit.DecisionBlock, securityaudit.DecisionUnavailable, securityaudit.DecisionInvalid} {
		decision := promptGuardDecision(kind)
		t.Run("openai_"+string(kind), func(t *testing.T) {
			c, recorder := securityAuditErrorTestContext(t)
			(&OpenAIGatewayHandler{}).openAISecurityAuditError(c, decision)
			require.Equal(t, decision.HTTPStatus, recorder.Code)
			payload := decodeErrorJSON(t, recorder)
			errorObject := requireObject(t, payload["error"])
			require.Equal(t, decision.ErrorCode, errorObject["code"])
			if kind == securityaudit.DecisionBlock {
				require.Equal(t, "permission_error", errorObject["type"])
			} else {
				require.Equal(t, "api_error", errorObject["type"])
			}
			require.NotContains(t, recorder.Body.String(), "raw prompt")
			require.NotContains(t, recorder.Body.String(), "guard-one")
		})

		t.Run("responses_"+string(kind), func(t *testing.T) {
			c, recorder := securityAuditErrorTestContext(t)
			(&GatewayHandler{}).responsesSecurityAuditError(c, decision)
			require.Equal(t, decision.HTTPStatus, recorder.Code)
			errorObject := requireObject(t, decodeErrorJSON(t, recorder)["error"])
			require.Equal(t, decision.ErrorCode, errorObject["code"])
			require.Equal(t, "api_error", errorObject["type"])
		})

		t.Run("claude_"+string(kind), func(t *testing.T) {
			c, recorder := securityAuditErrorTestContext(t)
			(&GatewayHandler{}).anthropicSecurityAuditError(c, decision)
			require.Equal(t, decision.HTTPStatus, recorder.Code)
			payload := decodeErrorJSON(t, recorder)
			require.Equal(t, "error", payload["type"])
			errorObject := requireObject(t, payload["error"])
			require.Equal(t, decision.ErrorCode, errorObject["code"])
			if kind == securityaudit.DecisionBlock {
				require.Equal(t, "permission_error", errorObject["type"])
			} else {
				require.Equal(t, "api_error", errorObject["type"])
			}
		})
	}
}

func TestPromptGuardGeminiErrorEnvelopeGolden(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, kind := range []securityaudit.DecisionKind{securityaudit.DecisionBlock, securityaudit.DecisionUnavailable, securityaudit.DecisionInvalid} {
		decision := promptGuardDecision(kind)
		c, recorder := securityAuditErrorTestContext(t)
		googleSecurityAuditError(c, decision)
		require.Equal(t, decision.HTTPStatus, recorder.Code)
		payload := decodeErrorJSON(t, recorder)
		errorObject := requireObject(t, payload["error"])
		require.Equal(t, float64(decision.HTTPStatus), errorObject["code"], "Gemini code must remain numeric")
		if decision.HTTPStatus == http.StatusForbidden {
			require.Equal(t, "PERMISSION_DENIED", errorObject["status"])
		} else {
			require.Equal(t, "UNAVAILABLE", errorObject["status"])
		}
		details := requireArray(t, errorObject["details"])
		require.Len(t, details, 1)
		errorInfo := requireObject(t, details[0])
		require.Equal(t, "type.googleapis.com/google.rpc.ErrorInfo", errorInfo["@type"])
		require.Equal(t, decision.ErrorCode, errorInfo["reason"])
		require.Equal(t, "sub2api.securityaudit", errorInfo["domain"])
		metadata := requireObject(t, errorInfo["metadata"])
		require.Equal(t, map[string]any{"request_id": "request-error-golden"}, metadata)
	}
}

func TestPromptGuardWebSocketCloseMappingGolden(t *testing.T) {
	require.Equal(t, int64(4403), int64(securityAuditWSCloseStatus(promptGuardDecision(securityaudit.DecisionBlock))))
	require.Equal(t, securityaudit.ErrorCodeBlocked, securityAuditWSCloseReason(promptGuardDecision(securityaudit.DecisionBlock)))
	require.Equal(t, int64(1013), int64(securityAuditWSCloseStatus(promptGuardDecision(securityaudit.DecisionUnavailable))))
	require.Equal(t, securityaudit.ErrorCodeUnavailable, securityAuditWSCloseReason(promptGuardDecision(securityaudit.DecisionUnavailable)))
	require.Equal(t, int64(1013), int64(securityAuditWSCloseStatus(promptGuardDecision(securityaudit.DecisionInvalid))))
	require.Equal(t, securityaudit.ErrorCodeInvalidResponse, securityAuditWSCloseReason(promptGuardDecision(securityaudit.DecisionInvalid)))
}

func TestCyberSessionBlockedDecisionUsesOpenAIAndWebSocketProtocols(t *testing.T) {
	gin.SetMode(gin.TestMode)
	decision := cyberSessionBlockedSecurityAuditDecision()
	c, recorder := securityAuditErrorTestContext(t)
	(&OpenAIGatewayHandler{}).openAISecurityAuditError(c, decision)
	require.Equal(t, http.StatusForbidden, recorder.Code)
	errorObject := requireObject(t, decodeErrorJSON(t, recorder)["error"])
	require.Equal(t, "permission_error", errorObject["type"])
	require.Equal(t, cyberSessionBlockedErrorCode, errorObject["code"])
	require.Equal(t, cyberSessionBlockedClientMsg, errorObject["message"])

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		writeSecurityAuditWSError(r.Context(), conn, decision)
		_ = conn.Close(securityAuditWSCloseStatus(decision), securityAuditWSCloseReason(decision))
	}))
	defer server.Close()

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	client, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	cancelDial()
	require.NoError(t, err)
	defer func() { _ = client.CloseNow() }()

	readCtx, cancelRead := context.WithTimeout(context.Background(), 3*time.Second)
	_, payload, err := client.Read(readCtx)
	cancelRead()
	require.NoError(t, err)
	var frame map[string]any
	require.NoError(t, json.Unmarshal(payload, &frame))
	require.Equal(t, "evt_cyber_session_blocked", frame["event_id"])
	errorFrame := requireObject(t, frame["error"])
	require.Equal(t, cyberSessionBlockedErrorCode, errorFrame["code"])
	require.Equal(t, cyberSessionBlockedClientMsg, errorFrame["message"])
	require.Equal(t, coderws.StatusPolicyViolation, securityAuditWSCloseStatus(decision))
	require.Equal(t, "session blocked by cyber-security policy", securityAuditWSCloseReason(decision))
}

func TestCyberSessionBlockedDecisionWritesCompactSSEFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, recorder := securityAuditErrorTestContext(t)
	service.MarkOpenAICompactClientStream(c)
	stop := service.StartOpenAICompactSSEKeepalive(c, time.Millisecond)
	t.Cleanup(stop)
	require.Eventually(t, func() bool {
		return recorder.Code == http.StatusOK && strings.Contains(recorder.Body.String(), ": keepalive\n\n")
	}, time.Second, time.Millisecond)

	(&OpenAIGatewayHandler{}).openAISecurityAuditError(c, cyberSessionBlockedSecurityAuditDecision())

	body := recorder.Body.String()
	require.Contains(t, body, "event: response.failed\n")
	require.Contains(t, body, `"code":"permission_denied"`)
	require.Contains(t, body, cyberSessionBlockedClientMsg)
}

func TestLegacyModerationErrorKeepsExistingClientPriority(t *testing.T) {
	legacy := &securityaudit.Decision{
		Kind: securityaudit.DecisionBlock, HTTPStatus: http.StatusForbidden,
		ErrorCode: "content_policy_violation", ClientMessage: "legacy exact message",
		Legacy: &securityaudit.LegacyDecision{Blocked: true, StatusCode: http.StatusForbidden, ErrorCode: "content_policy_violation", Message: "legacy exact message"},
		Prompt: &securityaudit.PromptDecision{Kind: securityaudit.DecisionBlock, ErrorCode: securityaudit.ErrorCodeBlocked},
	}
	c, recorder := securityAuditErrorTestContext(t)
	(&GatewayHandler{}).openAISecurityAuditError(c, legacy)
	require.Equal(t, http.StatusForbidden, recorder.Code)
	require.Contains(t, recorder.Body.String(), "legacy exact message")
	require.Contains(t, recorder.Body.String(), "content_policy_violation")
	require.NotContains(t, recorder.Body.String(), securityaudit.ErrorCodeBlocked)
}
