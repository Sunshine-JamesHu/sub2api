package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/securityaudit"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type handlerPromptEngine struct {
	mu sync.Mutex

	mode      securityaudit.Mode
	decision  *securityaudit.PromptDecision
	err       error
	evaluated int
	enqueued  int
	requests  []securityaudit.Request
}

func (e *handlerPromptEngine) EffectiveMode() securityaudit.Mode { return e.mode }
func (e *handlerPromptEngine) Enqueue(_ context.Context, req securityaudit.Request) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.enqueued++
	e.requests = append(e.requests, req.Clone())
	return e.err
}
func (e *handlerPromptEngine) Evaluate(_ context.Context, req securityaudit.Request) (*securityaudit.PromptDecision, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.evaluated++
	e.requests = append(e.requests, req.Clone())
	return e.decision, e.err
}
func (e *handlerPromptEngine) snapshot() (evaluated, enqueued int, requests []securityaudit.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	requests = make([]securityaudit.Request, len(e.requests))
	copy(requests, e.requests)
	return e.evaluated, e.enqueued, requests
}

func securityAuditMediaTestMiddleware(c *gin.Context) {
	groupID := int64(3)
	user := &service.User{ID: 7, Username: "media-user", Email: "media@example.test"}
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID: 9, UserID: 7, User: user, Name: "media-key", GroupID: &groupID,
		Group: &service.Group{ID: groupID, Name: "media-group", Platform: service.PlatformOpenAI, AllowImageGeneration: true},
	})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 7, Concurrency: 2})
	c.Next()
}

func newSecurityAuditMediaTestContext(t *testing.T, platform, path, body string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	groupID := int64(3)
	user := &service.User{ID: 7, Username: "media-user", Email: "media@example.test"}
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID: 9, UserID: 7, User: user, Name: "media-key", GroupID: &groupID,
		Group: &service.Group{ID: groupID, Name: "media-group", Platform: platform, AllowImageGeneration: true},
	})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 7, Concurrency: 2})
	return c, recorder
}

func newPreblockedSecurityAuditMediaHandler(engine *handlerPromptEngine, blocker cyberSessionBlocker) *OpenAIGatewayHandler {
	return &OpenAIGatewayHandler{
		gatewayService:           &service.OpenAIGatewayService{},
		billingCacheService:      &service.BillingCacheService{},
		apiKeyService:            &service.APIKeyService{},
		concurrencyHelper:        &ConcurrencyHelper{concurrencyService: &service.ConcurrencyService{}},
		securityAuditCoordinator: securityaudit.NewCoordinator(nil, engine),
		sessionBlocker:           blocker,
	}
}

func blockingHandlerPromptEngine() *handlerPromptEngine {
	return &handlerPromptEngine{mode: securityaudit.ModeBlocking, decision: &securityaudit.PromptDecision{
		Kind: securityaudit.DecisionBlock, ErrorCode: securityaudit.ErrorCodeBlocked, AllowNextStage: false,
	}}
}

func TestAsyncImagePromptGuardRunsBeforeTaskCreation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &asyncImageMemoryStore{tasks: map[string]*service.ImageTaskRecord{}}
	tasks := service.NewImageTaskServiceWithUploader(store, nil, time.Hour, time.Minute)
	engine := blockingHandlerPromptEngine()
	openAI := &OpenAIGatewayHandler{securityAuditCoordinator: securityaudit.NewCoordinator(nil, engine)}
	h := &AsyncImageHandler{tasks: tasks, openAI: openAI}
	executions := 0
	h.execute = func(string, *gin.Context) { executions++ }

	router := gin.New()
	router.Use(securityAuditMediaTestMiddleware)
	router.POST("/v1/images/generations/async", h.Submit)
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations/async", strings.NewReader(`{"model":"gpt-image-2","prompt":"blocked async prompt"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusForbidden, recorder.Code)
	require.Contains(t, recorder.Body.String(), securityaudit.ErrorCodeBlocked)
	require.Empty(t, store.tasks, "no asynchronous task may exist after a blocking decision")
	require.Zero(t, executions)
	evaluated, _, requests := engine.snapshot()
	require.Equal(t, 1, evaluated)
	require.Len(t, requests, 1)
	require.Contains(t, string(requests[0].Body), "blocked async prompt")
}

func TestAsyncImageSuccessfulPrecheckIsNotRepeatedByDetachedExecution(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &asyncImageMemoryStore{tasks: map[string]*service.ImageTaskRecord{}}
	tasks := service.NewImageTaskServiceWithUploader(store, nil, time.Hour, time.Minute)
	engine := &handlerPromptEngine{mode: securityaudit.ModeBlocking, decision: &securityaudit.PromptDecision{Kind: securityaudit.DecisionAllow, AllowNextStage: true}}
	openAI := &OpenAIGatewayHandler{securityAuditCoordinator: securityaudit.NewCoordinator(nil, engine)}
	h := &AsyncImageHandler{tasks: tasks, openAI: openAI}
	var executionMu sync.Mutex
	repeatedDecision := false
	h.execute = func(_ string, c *gin.Context) {
		apiKey, _ := middleware2.GetAPIKeyFromContext(c)
		subject, _ := middleware2.GetAuthSubjectFromContext(c)
		decision := openAI.checkSecurityAudit(c, nil, apiKey, subject, service.ContentModerationProtocolOpenAIImages, "gpt-image-2", []byte(`{"prompt":"must not rescan"}`))
		executionMu.Lock()
		repeatedDecision = decision != nil
		executionMu.Unlock()
		c.JSON(http.StatusOK, gin.H{"created": 1, "data": []any{}})
	}

	router := gin.New()
	router.Use(securityAuditMediaTestMiddleware)
	router.POST("/v1/images/generations/async", h.Submit)
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations/async", strings.NewReader(`{"model":"gpt-image-2","prompt":"allowed async prompt"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusAccepted, recorder.Code)
	require.Eventually(t, func() bool {
		store.mu.RLock()
		defer store.mu.RUnlock()
		for _, task := range store.tasks {
			if task.Status == service.ImageTaskStatusCompleted {
				return true
			}
		}
		return false
	}, time.Second, 10*time.Millisecond)
	evaluated, _, _ := engine.snapshot()
	require.Equal(t, 1, evaluated)
	executionMu.Lock()
	require.False(t, repeatedDecision)
	executionMu.Unlock()
}

func TestBatchImagePromptGuardRunsBeforePersistenceOrBilling(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := blockingHandlerPromptEngine()
	openAI := &OpenAIGatewayHandler{securityAuditCoordinator: securityaudit.NewCoordinator(nil, engine)}
	h := &BatchImageHandler{openAI: openAI}
	router := gin.New()
	router.Use(securityAuditMediaTestMiddleware)
	router.POST("/v1/images/batches", h.Submit)
	body := map[string]any{
		"model": "gemini-image-test",
		"items": []map[string]any{{
			"custom_id": "one", "prompt": "blocked batch prompt",
			"reference_images": []map[string]any{{"mime_type": "image/png", "data": []byte("BINARY_CANARY")}},
		}},
	}
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "/v1/images/batches", strings.NewReader(string(raw)))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	require.NotPanics(t, func() { router.ServeHTTP(recorder, request) }, "nil service would panic if Submit were reached")
	require.Equal(t, http.StatusForbidden, recorder.Code)
	evaluated, _, requests := engine.snapshot()
	require.Equal(t, 1, evaluated)
	require.Len(t, requests, 1)
	require.Contains(t, string(requests[0].Body), "blocked batch prompt")
	require.NotContains(t, string(requests[0].Body), "BINARY_CANARY")
	require.NotContains(t, string(requests[0].Body), "QklOQVJZX0NBTkFSWQ==")
}

func TestBatchImagePreblockedSessionUsesRawPromptCacheKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := &handlerPromptEngine{mode: securityaudit.ModeBlocking, decision: &securityaudit.PromptDecision{
		Kind: securityaudit.DecisionAllow, AllowNextStage: true,
	}}
	blocker := &fakeHandlerCyberSessionBlocker{}
	blocker.blocked.Store(true)
	openAI := &OpenAIGatewayHandler{
		securityAuditCoordinator: securityaudit.NewCoordinator(nil, engine),
		sessionBlocker:           blocker,
	}
	h := &BatchImageHandler{openAI: openAI}
	router := gin.New()
	router.Use(securityAuditMediaTestMiddleware)
	router.POST("/v1/images/batches", h.Submit)
	request := httptest.NewRequest(http.MethodPost, "/v1/images/batches", strings.NewReader(`{
		"model":"gemini-image-test",
		"prompt_cache_key":"batch-raw-session",
		"items":[{"custom_id":"one","prompt":"audit payload does not retain the session key"}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusForbidden, recorder.Code)
	require.Contains(t, recorder.Body.String(), cyberSessionBlockedErrorCode)
	evaluated, _, _ := engine.snapshot()
	require.Zero(t, evaluated, "preblocked raw prompt_cache_key must skip the audit coordinator")
}

func TestMediaHandlersPreblockedSessionUseRawPromptCacheKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name     string
		platform string
		path     string
		body     string
		handle   func(*OpenAIGatewayHandler, *gin.Context)
	}{
		{
			name:     "images",
			platform: service.PlatformOpenAI,
			path:     "/v1/images/generations",
			body:     `{"model":"gpt-image-2","prompt":"audit payload omits prompt cache key","prompt_cache_key":"image-raw-session"}`,
			handle: func(h *OpenAIGatewayHandler, c *gin.Context) {
				h.Images(c)
			},
		},
		{
			name:     "grok media",
			platform: service.PlatformGrok,
			path:     "/v1/images/generations",
			body:     `{"model":"grok-imagine-image","prompt":"audit payload omits prompt cache key","prompt_cache_key":"grok-media-raw-session"}`,
			handle: func(h *OpenAIGatewayHandler, c *gin.Context) {
				h.GrokImages(c)
			},
		},
		{
			name:     "grok tts",
			platform: service.PlatformGrok,
			path:     "/v1/audio/speech",
			body:     `{"model":"grok-tts","input":"audit payload omits prompt cache key","prompt_cache_key":"grok-tts-raw-session"}`,
			handle: func(h *OpenAIGatewayHandler, c *gin.Context) {
				h.GrokVoice(c, "tts")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := &handlerPromptEngine{mode: securityaudit.ModeBlocking, decision: &securityaudit.PromptDecision{
				Kind: securityaudit.DecisionAllow, AllowNextStage: true,
			}}
			blocker := &fakeHandlerCyberSessionBlocker{}
			blocker.blocked.Store(true)
			h := newPreblockedSecurityAuditMediaHandler(engine, blocker)
			c, recorder := newSecurityAuditMediaTestContext(t, tc.platform, tc.path, tc.body)

			tc.handle(h, c)

			require.Equal(t, http.StatusForbidden, recorder.Code)
			require.Contains(t, recorder.Body.String(), cyberSessionBlockedErrorCode)
			evaluated, _, _ := engine.snapshot()
			require.Zero(t, evaluated, "preblocked raw prompt_cache_key must skip the audit coordinator")
		})
	}
}

func TestAsyncImagePreblockedSessionUsesRawPromptCacheKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &asyncImageMemoryStore{tasks: map[string]*service.ImageTaskRecord{}}
	tasks := service.NewImageTaskServiceWithUploader(store, nil, time.Hour, time.Minute)
	engine := &handlerPromptEngine{mode: securityaudit.ModeBlocking, decision: &securityaudit.PromptDecision{
		Kind: securityaudit.DecisionAllow, AllowNextStage: true,
	}}
	blocker := &fakeHandlerCyberSessionBlocker{}
	blocker.blocked.Store(true)
	openAI := newPreblockedSecurityAuditMediaHandler(engine, blocker)
	h := &AsyncImageHandler{tasks: tasks, openAI: openAI, execute: func(string, *gin.Context) {
		t.Fatal("a preblocked async request must not execute")
	}}
	c, recorder := newSecurityAuditMediaTestContext(t, service.PlatformOpenAI, "/v1/images/generations/async", `{"model":"gpt-image-2","prompt":"audit payload omits prompt cache key","prompt_cache_key":"async-image-raw-session"}`)

	h.Submit(c)

	require.Equal(t, http.StatusForbidden, recorder.Code)
	require.Contains(t, recorder.Body.String(), cyberSessionBlockedErrorCode)
	require.Empty(t, store.tasks, "a preblocked async request must not create a task")
	evaluated, _, _ := engine.snapshot()
	require.Zero(t, evaluated, "preblocked raw prompt_cache_key must skip the audit coordinator")
}

func TestSecurityAuditBlockingFailuresLeaveAllDownstreamCountersAtZero(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, kind := range []securityaudit.DecisionKind{securityaudit.DecisionBlock, securityaudit.DecisionUnavailable, securityaudit.DecisionInvalid} {
		t.Run(string(kind), func(t *testing.T) {
			promptDecision := promptGuardDecision(kind)
			engine := &handlerPromptEngine{mode: securityaudit.ModeBlocking, decision: &securityaudit.PromptDecision{
				Kind: kind, ErrorCode: promptDecision.ErrorCode, AllowNextStage: false,
			}}
			coordinator := securityaudit.NewCoordinator(nil, engine)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-test","messages":[{"role":"user","content":"guard me"}]}`))
			groupID := int64(3)
			apiKey := &service.APIKey{ID: 9, UserID: 7, GroupID: &groupID, Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI}}
			subject := middleware2.AuthSubject{UserID: 7, Concurrency: 2}
			decision := runSecurityAudit(c, nil, coordinator, nil, apiKey, subject, service.ContentModerationProtocolOpenAIChat, "gpt-test", []byte(`{"messages":[{"role":"user","content":"guard me"}]}`), "http")
			require.NotNil(t, decision)
			require.False(t, decision.AllowNextStage)
			require.False(t, recorder.Result().Header.Get("Content-Type") != "", "Guard evaluation itself must not start SSE/HTTP output")

			accountSelections, billingChecks, billingPreconsumes, upstreamDispatches := 0, 0, 0, 0
			if decision.AllowNextStage {
				accountSelections++
				billingChecks++
				billingPreconsumes++
				upstreamDispatches++
			}
			require.Zero(t, accountSelections)
			require.Zero(t, billingChecks)
			require.Zero(t, billingPreconsumes)
			require.Zero(t, upstreamDispatches)
			(&OpenAIGatewayHandler{}).openAISecurityAuditError(c, decision)
			require.Equal(t, promptDecision.HTTPStatus, recorder.Code)
		})
	}
}
