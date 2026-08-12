package handler

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"

	"github.com/Wei-Shaw/sub2api/internal/securityaudit"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestGrokVideoLookupPreblockedSessionBlocksBeforeAccountResolution covers the
// non-generation media endpoints (video status/content): an explicit
// session_id/conversation_id header on a blocked session must return the local
// cyber session block before any audit, billing, account selection, or upstream
// account lookup runs.
func TestGrokVideoLookupPreblockedSessionBlocksBeforeAccountResolution(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name   string
		path   string
		handle func(*OpenAIGatewayHandler, *gin.Context)
		header string
	}{
		{name: "video status session_id", path: "/v1/videos/status/task-1", header: "session_id", handle: func(h *OpenAIGatewayHandler, c *gin.Context) { h.GrokVideoStatus(c) }},
		{name: "video status conversation_id", path: "/v1/videos/status/task-1", header: "conversation_id", handle: func(h *OpenAIGatewayHandler, c *gin.Context) { h.GrokVideoStatus(c) }},
		{name: "video content session_id", path: "/v1/videos/content/task-1", header: "session_id", handle: func(h *OpenAIGatewayHandler, c *gin.Context) { h.GrokVideoContent(c) }},
		{name: "video content conversation_id", path: "/v1/videos/content/task-1", header: "conversation_id", handle: func(h *OpenAIGatewayHandler, c *gin.Context) { h.GrokVideoContent(c) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := &handlerPromptEngine{mode: securityaudit.ModeBlocking, decision: &securityaudit.PromptDecision{
				Kind: securityaudit.DecisionAllow, AllowNextStage: true,
			}}
			blocker := &fakeHandlerCyberSessionBlocker{}
			blocker.blocked.Store(true)
			h := newPreblockedSecurityAuditMediaHandler(engine, blocker)
			c, recorder := newSecurityAuditMediaTestContext(t, service.PlatformGrok, tc.path, "")
			c.Params = gin.Params{{Key: "request_id", Value: "task-1"}}
			c.Request.Header.Set(tc.header, "grok-video-lookup-session")

			tc.handle(h, c)

			require.Equal(t, http.StatusForbidden, recorder.Code)
			require.Contains(t, recorder.Body.String(), cyberSessionBlockedErrorCode)
			evaluated, _, _ := engine.snapshot()
			require.Zero(t, evaluated, "preblocked video lookup must skip the audit coordinator")
			require.Equal(t, int64(1), blocker.checks.Load(), "precheck must consult the session lock exactly once")
			require.Zero(t, blocker.marks.Load(), "a precheck hit must not write a new lock")
		})
	}
}

// TestGrokVideoLookupNoExplicitSessionKeepsOldFlow verifies that without an
// explicit session identifier the local precheck passes and the request falls
// through to the original flow (here: missing video task binding -> 404).
func TestGrokVideoLookupNoExplicitSessionKeepsOldFlow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := &handlerPromptEngine{mode: securityaudit.ModeBlocking, decision: &securityaudit.PromptDecision{
		Kind: securityaudit.DecisionAllow, AllowNextStage: true,
	}}
	blocker := &fakeHandlerCyberSessionBlocker{}
	blocker.blocked.Store(true)
	cfg := &config.Config{RunMode: config.RunModeSimple}
	h := &OpenAIGatewayHandler{
		gatewayService:           &service.OpenAIGatewayService{},
		billingCacheService:      service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil),
		apiKeyService:            &service.APIKeyService{},
		concurrencyHelper:        &ConcurrencyHelper{concurrencyService: &service.ConcurrencyService{}},
		securityAuditCoordinator: securityaudit.NewCoordinator(nil, engine),
		sessionBlocker:           blocker,
	}
	c, recorder := newSecurityAuditMediaTestContext(t, service.PlatformGrok, "/v1/videos/status/task-1", "")
	c.Params = gin.Params{{Key: "request_id", Value: "task-1"}}
	// Concurrency 0 -> unlimited no-op slots, keeping the old-flow fixture free of a cache.
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 7, Concurrency: 0})

	h.GrokVideoStatus(c)

	require.Equal(t, http.StatusNotFound, recorder.Code)
	require.Contains(t, recorder.Body.String(), "Video request not found")
	require.Zero(t, blocker.checks.Load(), "no explicit session key means the local lock must not even be consulted")
	evaluated, _, _ := engine.snapshot()
	require.Zero(t, evaluated, "video lookup has no audit payload and must not invoke the coordinator")
}
