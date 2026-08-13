package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/securityaudit"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestCachesSecurityAuditCompletionSkipsWebSocketStages(t *testing.T) {
	require.True(t, cachesSecurityAuditCompletion("http"))
	require.True(t, cachesSecurityAuditCompletion(""))
	require.False(t, cachesSecurityAuditCompletion("first_turn"))
	require.False(t, cachesSecurityAuditCompletion("subsequent_turn"))
	require.True(t, isSecurityAuditWebSocketStage("first_turn"))
	require.True(t, isSecurityAuditWebSocketStage("subsequent_turn"))
	require.False(t, isSecurityAuditWebSocketStage("http"))
}

func TestRunSecurityAuditDoesNotSkipSubsequentWebSocketTurns(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := &turnCountingEngine{mode: securityaudit.ModeAsync}
	coordinator := securityaudit.NewCoordinator(nil, engine)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	subject := middleware2.AuthSubject{UserID: 7, Concurrency: 1}
	first := runSecurityAudit(c, nil, coordinator, nil, nil, subject, "openai_responses", "gpt-test",
		[]byte(`{"type":"response.create","response":{"input":"benign"}}`), "first_turn")
	require.NotNil(t, first)
	require.True(t, first.AllowNextStage)
	require.Equal(t, int64(1), engine.enqueues.Load())
	_, cached := c.Get(securityAuditCompletedContextKey)
	require.False(t, cached, "WebSocket stages must not set the HTTP completion cache")

	// Even if an HTTP path previously cached completion on this Context, WS turns
	// must still audit every response.create payload.
	c.Set(securityAuditCompletedContextKey, true)

	second := runSecurityAudit(c, nil, coordinator, nil, nil, subject, "openai_responses", "gpt-test",
		[]byte(`{"type":"response.create","response":{"input":"malicious follow-up"}}`), "subsequent_turn")
	require.NotNil(t, second)
	require.Equal(t, int64(2), engine.enqueues.Load(), "subsequent WebSocket turns must be audited again")
}

func TestRunSecurityAuditDeduplicatesRepeatedPayloadWithinWebSocketTurn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := &turnCountingEngine{mode: securityaudit.ModeBlocking}
	coordinator := securityaudit.NewCoordinator(nil, engine)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	payload := []byte(`{"type":"response.create","response":{"input":"same turn"}}`)
	c.Set(securityAuditWSTurnContextKey, 2)
	first := runSecurityAudit(c, nil, coordinator, nil, nil, middleware2.AuthSubject{UserID: 7}, "openai_responses", "gpt-test", payload, "subsequent_turn")
	second := runSecurityAudit(c, nil, coordinator, nil, nil, middleware2.AuthSubject{UserID: 7}, "openai_responses", "gpt-test", payload, "subsequent_turn")
	require.NotNil(t, first)
	require.NotNil(t, second)
	require.True(t, first.AllowNextStage)
	require.True(t, second.AllowNextStage)
	require.Equal(t, int64(1), engine.evaluates.Load())

	// The cache holds only one successful same-turn result.
	entry, exists := c.Get(securityAuditWSDedupeContextKey)
	require.True(t, exists)
	require.IsType(t, securityAuditWSDedupeEntry{}, entry)

	c.Set(securityAuditWSTurnContextKey, 3)
	runSecurityAudit(c, nil, coordinator, nil, nil, middleware2.AuthSubject{UserID: 7}, "openai_responses", "gpt-test", payload, "subsequent_turn")
	require.Equal(t, int64(2), engine.evaluates.Load())
}

func TestRunSecurityAuditDoesNotCacheFailedWebSocketDecision(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := &turnCountingEngine{
		mode: securityaudit.ModeBlocking,
		decisions: []*securityaudit.PromptDecision{
			{Kind: securityaudit.DecisionUnavailable, AllowNextStage: false},
			{Kind: securityaudit.DecisionAllow, AllowNextStage: true},
		},
	}
	coordinator := securityaudit.NewCoordinator(nil, engine)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set(securityAuditWSTurnContextKey, 2)
	payload := []byte(`{"type":"response.create","response":{"input":"retry me"}}`)

	first := runSecurityAudit(c, nil, coordinator, nil, nil, middleware2.AuthSubject{UserID: 7}, "openai_responses", "gpt-test", payload, "subsequent_turn")
	_, cachedAfterFailure := c.Get(securityAuditWSDedupeContextKey)
	second := runSecurityAudit(c, nil, coordinator, nil, nil, middleware2.AuthSubject{UserID: 7}, "openai_responses", "gpt-test", payload, "subsequent_turn")

	require.False(t, first.AllowNextStage)
	require.False(t, cachedAfterFailure)
	require.True(t, second.AllowNextStage)
	require.Equal(t, int64(2), engine.evaluates.Load())
}

func TestRunSecurityAuditDoesNotCacheFlaggedWebSocketDecision(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := &turnCountingEngine{
		mode: securityaudit.ModeBlocking,
		decisions: []*securityaudit.PromptDecision{
			{Kind: securityaudit.DecisionFlag, AllowNextStage: true},
			{Kind: securityaudit.DecisionAllow, AllowNextStage: true},
		},
	}
	coordinator := securityaudit.NewCoordinator(nil, engine)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set(securityAuditWSTurnContextKey, 2)
	payload := []byte(`{"type":"response.create","response":{"input":"retry flagged"}}`)

	first := runSecurityAudit(c, nil, coordinator, nil, nil, middleware2.AuthSubject{UserID: 7}, "openai_responses", "gpt-test", payload, "subsequent_turn")
	_, cachedAfterFlag := c.Get(securityAuditWSDedupeContextKey)
	second := runSecurityAudit(c, nil, coordinator, nil, nil, middleware2.AuthSubject{UserID: 7}, "openai_responses", "gpt-test", payload, "subsequent_turn")

	require.Equal(t, securityaudit.DecisionFlag, first.Kind)
	require.True(t, first.AllowNextStage)
	require.False(t, cachedAfterFlag)
	require.Equal(t, securityaudit.DecisionAllow, second.Kind)
	require.Equal(t, int64(2), engine.evaluates.Load())
}

func TestOpenAISecurityAuditPreblockedSessionSkipsCoordinator(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := &turnCountingEngine{mode: securityaudit.ModeBlocking}
	blocker := &fakeHandlerCyberSessionBlocker{}
	blocker.blocked.Store(true)
	h := &OpenAIGatewayHandler{
		securityAuditCoordinator: securityaudit.NewCoordinator(nil, engine),
		sessionBlocker:           blocker,
	}
	c := newOpenAISecurityAuditContext(t, `{"model":"gpt-test","input":"blocked session"}`)
	payload := []byte(`{"prompt_cache_key":"audit-lock-1","input":"blocked session"}`)

	decision := h.checkSecurityAudit(c, nil, &service.APIKey{ID: 9}, middleware2.AuthSubject{UserID: 7}, service.ContentModerationProtocolOpenAIResponses, "gpt-test", payload)

	require.NotNil(t, decision)
	require.Equal(t, securityaudit.DecisionBlock, decision.Kind)
	require.Equal(t, http.StatusForbidden, decision.HTTPStatus)
	require.Equal(t, cyberSessionBlockedErrorCode, decision.ErrorCode)
	require.Equal(t, cyberSessionBlockedClientMsg, decision.ClientMessage)
	require.Zero(t, engine.evaluates.Load(), "preblocked session must not reach the coordinator")
	require.Equal(t, int64(1), blocker.checks.Load())
	require.Zero(t, blocker.marks.Load())
}

func TestOpenAISecurityAuditWSFirstTurnChecksPromptCacheKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := &turnCountingEngine{mode: securityaudit.ModeBlocking}
	blocker := &fakeHandlerCyberSessionBlocker{}
	blocker.blocked.Store(true)
	h := &OpenAIGatewayHandler{securityAuditCoordinator: securityaudit.NewCoordinator(nil, engine), sessionBlocker: blocker}
	c := newOpenAISecurityAuditContext(t, `{}`)
	c.Set(securityAuditWSTurnContextKey, 1)
	payload := []byte(`{"type":"response.create","model":"gpt-test","prompt_cache_key":"ws-first-turn-lock","input":"blocked"}`)

	decision := h.checkSecurityAuditStage(c, nil, &service.APIKey{ID: 9}, middleware2.AuthSubject{}, service.ContentModerationProtocolOpenAIResponses, "gpt-test", payload, "first_turn")

	require.True(t, isCyberSessionBlockedSecurityAuditDecision(decision))
	require.Zero(t, engine.evaluates.Load(), "a blocked first WS payload must not reach the coordinator")
}

func TestOpenAISecurityAuditPrecheckBypassesCompletedAndWSDedupeCaches(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Run("http completion cache", func(t *testing.T) {
		engine := &turnCountingEngine{mode: securityaudit.ModeBlocking}
		blocker := &fakeHandlerCyberSessionBlocker{}
		blocker.blocked.Store(true)
		h := &OpenAIGatewayHandler{securityAuditCoordinator: securityaudit.NewCoordinator(nil, engine), sessionBlocker: blocker}
		c := newOpenAISecurityAuditContext(t, `{}`)
		c.Request.Header.Set("session_id", "audit-cache-lock")
		c.Set(securityAuditCompletedContextKey, true)

		decision := h.checkSecurityAudit(c, nil, &service.APIKey{ID: 9}, middleware2.AuthSubject{}, service.ContentModerationProtocolOpenAIChat, "gpt-test", []byte(`{"messages":[{"role":"user","content":"retry"}]}`))
		require.True(t, isCyberSessionBlockedSecurityAuditDecision(decision))
		require.Zero(t, engine.evaluates.Load())
	})

	t.Run("websocket turn dedupe", func(t *testing.T) {
		engine := &turnCountingEngine{mode: securityaudit.ModeBlocking}
		blocker := &fakeHandlerCyberSessionBlocker{}
		h := &OpenAIGatewayHandler{securityAuditCoordinator: securityaudit.NewCoordinator(nil, engine), sessionBlocker: blocker}
		c := newOpenAISecurityAuditContext(t, `{}`)
		c.Set(securityAuditWSTurnContextKey, 2)
		payload := []byte(`{"type":"response.create","prompt_cache_key":"audit-ws-lock","input":"same turn"}`)

		first := h.checkSecurityAuditStage(c, nil, &service.APIKey{ID: 9}, middleware2.AuthSubject{}, service.ContentModerationProtocolOpenAIResponses, "gpt-test", payload, "subsequent_turn")
		require.True(t, first.AllowNextStage)
		require.Equal(t, int64(1), engine.evaluates.Load())

		blocker.blocked.Store(true)
		second := h.checkSecurityAuditStage(c, nil, &service.APIKey{ID: 9}, middleware2.AuthSubject{}, service.ContentModerationProtocolOpenAIResponses, "gpt-test", payload, "subsequent_turn")
		require.True(t, isCyberSessionBlockedSecurityAuditDecision(second))
		require.Equal(t, int64(1), engine.evaluates.Load(), "precheck must run before the successful same-turn cache")
	})
}

func TestOpenAISecurityAuditOnlyRealBlocksMarkSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name      string
		kind      securityaudit.DecisionKind
		wantMarks int64
	}{
		{name: "allow", kind: securityaudit.DecisionAllow},
		{name: "flag", kind: securityaudit.DecisionFlag},
		{name: "unavailable", kind: securityaudit.DecisionUnavailable},
		{name: "invalid", kind: securityaudit.DecisionInvalid},
		{name: "block", kind: securityaudit.DecisionBlock},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := &turnCountingEngine{mode: securityaudit.ModeBlocking, decisions: []*securityaudit.PromptDecision{{
				Kind: tc.kind, AllowNextStage: tc.kind == securityaudit.DecisionAllow || tc.kind == securityaudit.DecisionFlag,
			}}}
			blocker := &fakeHandlerCyberSessionBlocker{}
			h := &OpenAIGatewayHandler{securityAuditCoordinator: securityaudit.NewCoordinator(nil, engine), sessionBlocker: blocker}
			c := newOpenAISecurityAuditContext(t, `{}`)
			c.Request.Header.Set("session_id", "audit-mark-"+tc.name)

			decision := h.checkSecurityAudit(c, nil, &service.APIKey{ID: 9}, middleware2.AuthSubject{}, service.ContentModerationProtocolOpenAIChat, "gpt-test", []byte(`{"messages":[{"role":"user","content":"test"}]}`))
			require.NotNil(t, decision)
			require.Equal(t, tc.wantMarks, blocker.marks.Load())
		})
	}
}

func TestOpenAISecurityAuditLegacyBlockMarksSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	blocker := &fakeHandlerCyberSessionBlocker{}
	h := &OpenAIGatewayHandler{
		securityAuditCoordinator: securityaudit.NewCoordinator(staticSecurityAuditLegacyEngine{decision: &securityaudit.LegacyDecision{
			Blocked: true, StatusCode: http.StatusForbidden, ErrorCode: "content_policy_violation", Message: "legacy block",
		}}, nil),
		sessionBlocker: blocker,
	}
	c := newOpenAISecurityAuditContext(t, `{}`)
	c.Request.Header.Set("conversation_id", "legacy-audit-lock")

	decision := h.checkSecurityAudit(c, nil, &service.APIKey{ID: 9}, middleware2.AuthSubject{}, service.ContentModerationProtocolOpenAIChat, "gpt-test", []byte(`{"messages":[{"role":"user","content":"legacy"}]}`))
	require.NotNil(t, decision)
	require.Equal(t, securityaudit.DecisionBlock, decision.Kind)
	require.NotNil(t, decision.Legacy)
	require.Zero(t, blocker.marks.Load(), "session locking defaults to off until enabled in content moderation settings")
}

func TestOpenAISecurityAuditUsesOriginalSessionBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := &turnCountingEngine{mode: securityaudit.ModeBlocking}
	blocker := &fakeHandlerCyberSessionBlocker{}
	blocker.blocked.Store(true)
	h := &OpenAIGatewayHandler{securityAuditCoordinator: securityaudit.NewCoordinator(nil, engine), sessionBlocker: blocker}
	c := newOpenAISecurityAuditContext(t, `{}`)

	decision := h.checkSecurityAuditWithSessionBody(c, nil, &service.APIKey{ID: 9}, middleware2.AuthSubject{}, service.ContentModerationProtocolOpenAIImages, "gpt-image-2", []byte(`{"prompt":"audit body excludes session key"}`), []byte(`{"prompt_cache_key":"raw-image-session"}`))
	require.True(t, isCyberSessionBlockedSecurityAuditDecision(decision))
	require.Zero(t, engine.evaluates.Load())
}

func TestOpenAISecurityAuditEmptyBodyKeepsExistingAuditFlow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := &turnCountingEngine{mode: securityaudit.ModeBlocking}
	blocker := &fakeHandlerCyberSessionBlocker{}
	h := &OpenAIGatewayHandler{securityAuditCoordinator: securityaudit.NewCoordinator(nil, engine), sessionBlocker: blocker}
	c := newOpenAISecurityAuditContext(t, "")

	decision := h.checkSecurityAuditWithSessionBody(c, nil, &service.APIKey{ID: 9}, middleware2.AuthSubject{}, service.ContentModerationProtocolOpenAIImages, "gpt-image-2", nil, []byte("{\"prompt_cache_key\":\"empty-audit-body\"}"))

	require.NotNil(t, decision)
	require.True(t, decision.AllowNextStage)
	require.Equal(t, int64(1), engine.evaluates.Load(), "the original helper audited an empty body")
	require.Equal(t, int64(1), blocker.checks.Load())
}

func newOpenAISecurityAuditContext(t *testing.T, body string) *gin.Context {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	return c
}

type fakeHandlerCyberSessionBlocker struct {
	blocked atomic.Bool
	checks  atomic.Int64
	marks   atomic.Int64
}

func (b *fakeHandlerCyberSessionBlocker) IsCyberSessionBlocked(context.Context, string) bool {
	b.checks.Add(1)
	return b.blocked.Load()
}

func (b *fakeHandlerCyberSessionBlocker) MarkCyberSessionBlocked(context.Context, string) {
	b.marks.Add(1)
}

type staticSecurityAuditLegacyEngine struct {
	decision *securityaudit.LegacyDecision
}

func (e staticSecurityAuditLegacyEngine) Check(context.Context, securityaudit.Request) (*securityaudit.LegacyDecision, error) {
	return e.decision, nil
}

func TestRunSecurityAuditLogsWebSocketChecksAndCacheHits(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := &turnCountingEngine{mode: securityaudit.ModeBlocking}
	coordinator := securityaudit.NewCoordinator(nil, engine)
	core, logs := observer.New(zap.InfoLevel)
	reqLog := zap.New(core)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set(securityAuditWSTurnContextKey, 2)
	payload := []byte(`{"type":"response.create","response":{"input":"same turn"}}`)

	runSecurityAudit(c, reqLog, coordinator, nil, nil, middleware2.AuthSubject{UserID: 7}, "openai_responses", "gpt-test", payload, "subsequent_turn")
	runSecurityAudit(c, reqLog, coordinator, nil, nil, middleware2.AuthSubject{UserID: 7}, "openai_responses", "gpt-test", payload, "subsequent_turn")

	startLogs := logs.FilterMessage("security_audit.gateway_check_start").All()
	require.Len(t, startLogs, 1)
	require.Equal(t, false, startLogs[0].ContextMap()["cached"])

	doneLogs := logs.FilterMessage("security_audit.gateway_check_done").All()
	require.Len(t, doneLogs, 2)
	require.Equal(t, false, doneLogs[0].ContextMap()["cached"])
	require.Equal(t, true, doneLogs[1].ContextMap()["cached"])
	require.Equal(t, "allow", doneLogs[1].ContextMap()["decision"])
	require.Equal(t, "subsequent_turn", doneLogs[1].ContextMap()["stage"])
	require.Equal(t, int64(1), engine.evaluates.Load())
}

type turnCountingEngine struct {
	mode      securityaudit.Mode
	enqueues  atomic.Int64
	evaluates atomic.Int64
	decisions []*securityaudit.PromptDecision
}

func (e *turnCountingEngine) EffectiveMode() securityaudit.Mode { return e.mode }
func (e *turnCountingEngine) Enqueue(context.Context, securityaudit.Request) error {
	e.enqueues.Add(1)
	return nil
}
func (e *turnCountingEngine) Evaluate(context.Context, securityaudit.Request) (*securityaudit.PromptDecision, error) {
	call := e.evaluates.Add(1)
	if int(call) <= len(e.decisions) {
		return e.decisions[call-1], nil
	}
	return &securityaudit.PromptDecision{Kind: securityaudit.DecisionAllow, AllowNextStage: true}, nil
}
