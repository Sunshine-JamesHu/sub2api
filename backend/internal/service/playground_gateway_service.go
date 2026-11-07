package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/aistudio"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/logredact"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"sync"
)

// PlaygroundGatewayService forwards requests to Google MakerSuiteService using web cookies and WAA BotGuard proofs.
type PlaygroundGatewayService struct {
	accountRepo     AccountRepository
	waaManager      *aistudio.WAAManager
	generateContent func(context.Context, *aistudio.Session, string, []byte) (*http.Response, error)
	browser         *PlaygroundBrowserService
	activeSessions  sync.Map
}

func NewPlaygroundGatewayService(accountRepo AccountRepository) *PlaygroundGatewayService {
	return &PlaygroundGatewayService{
		accountRepo: accountRepo,
		waaManager:  aistudio.DefaultWAAManager(),
	}
}

func decodePlaygroundRequest(body []byte) (*antigravity.GeminiRequest, error) {
	var request antigravity.GeminiRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, fmt.Errorf("parse Gemini request: %w", err)
	}
	if len(request.Contents) == 0 {
		return nil, errors.New("Playground request has no contents")
	}
	return &request, nil
}

// DoGemini shares the exact generation path between account tests and all API formats.
func (s *PlaygroundGatewayService) DoGemini(ctx context.Context, account *Account, model string, body []byte) (*http.Response, error) {
	if err := aistudio.CheckRuntime(); err != nil {
		return nil, err
	}
	request, err := decodePlaygroundRequest(body)
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	session, release, err := s.acquireSession(runCtx, account)
	if err != nil {
		cancel()
		return nil, err
	}
	runCtx = s.sessionContext(session)
	releaseAll := func() { cancel(); release() }
	attempt := 0
	generate := s.rawGenerate
	if s.generateContent != nil {
		generate = s.generateContent
	}
	var upstream *http.Response
	for {
		upstream, err = generate(runCtx, session, model, body)
		if err == nil && upstream.StatusCode != http.StatusOK {
			var raw []byte
			raw, err = io.ReadAll(io.LimitReader(upstream.Body, 1<<20))
			upstream.Body.Close()
			if err == nil {
				err = aistudio.CheckRPCResponse("GenerateContent", upstream.StatusCode, raw)
			}
		}
		if errors.Is(err, aistudio.ErrAuthenticationRequired) && attempt == 0 {
			attempt++
			err = s.refreshSession(session)
			if err == nil {
				continue
			}
		}
		break
	}
	if err != nil {
		s.markGenerationPermissionError(runCtx, account, model, err)
		releaseAll()
		return nil, err
	}
	if upstream.StatusCode != http.StatusOK {
		upstream.Body = &playgroundBody{ReadCloser: upstream.Body, done: releaseAll}
		return upstream, nil
	}
	reader, writer := io.Pipe()
	requestID := upstream.Header.Get("x-goog-request-id")
	go func() {
		defer releaseAll()
		emitted := false
		consume := func(event aistudio.StreamEvent) error {
			parts := []any{}
			if event.Text != "" {
				part := map[string]any{"text": event.Text}
				if event.Thought {
					part["thought"] = true
				}
				if event.ThoughtSignature != "" {
					part["thoughtSignature"] = event.ThoughtSignature
				}
				parts = append(parts, part)
			}
			if event.InlineData != nil {
				parts = append(parts, map[string]any{"inlineData": event.InlineData})
			}
			if event.FunctionCall != nil {
				part := map[string]any{"functionCall": event.FunctionCall}
				if event.ThoughtSignature != "" {
					part["thoughtSignature"] = event.ThoughtSignature
				}
				parts = append(parts, part)
			}
			if len(parts) == 0 && event.ThoughtSignature != "" {
				parts = append(parts, map[string]any{"thoughtSignature": event.ThoughtSignature})
			}
			chunk := map[string]any{}
			if len(parts) > 0 || event.FinishReason != "" {
				candidate := map[string]any{"index": 0}
				if len(parts) > 0 {
					candidate["content"] = map[string]any{"role": "model", "parts": parts}
				}
				if event.FinishReason != "" {
					candidate["finishReason"] = event.FinishReason
				}
				chunk["candidates"] = []any{candidate}
			}
			if event.Usage != nil {
				chunk["usageMetadata"] = map[string]any{"promptTokenCount": event.Usage.PromptTokenCount, "candidatesTokenCount": event.Usage.CandidatesTokenCount, "totalTokenCount": event.Usage.TotalTokenCount, "cachedContentTokenCount": event.Usage.CachedContentTokenCount, "thoughtsTokenCount": event.Usage.ThoughtsTokenCount, "toolUsePromptTokenCount": event.Usage.ToolUsePromptTokenCount}
			}
			if len(chunk) == 0 {
				return nil
			}
			encoded, err := json.Marshal(chunk)
			if err != nil {
				return err
			}
			if _, err = fmt.Fprintf(writer, "data: %s\n\n", encoded); err != nil {
				return err
			}
			emitted = true
			return nil
		}
		consume = aistudio.RestoreFunctionNames(request, consume)
		for {
			decodeErr := aistudio.DecodeStream(upstream.Body, consume)
			_ = upstream.Body.Close()
			// Stream trailers carry an RPC code without the HTTP/method context.
			// Attach it here before classifying or persisting the failure.
			var trailerErr *aistudio.RPCError
			if errors.As(decodeErr, &trailerErr) && trailerErr.Method == "" {
				annotated := *trailerErr
				annotated.Method = "GenerateContent"
				annotated.StatusCode = upstream.StatusCode
				decodeErr = &annotated
			}
			if errors.Is(decodeErr, aistudio.ErrAuthenticationRequired) && !emitted && attempt == 0 {
				attempt++
				decodeErr = s.refreshSession(session)
				if decodeErr == nil {
					upstream, decodeErr = generate(runCtx, session, model, body)
					if decodeErr == nil && upstream.StatusCode == http.StatusOK {
						continue
					}
					if decodeErr == nil {
						raw, readErr := io.ReadAll(io.LimitReader(upstream.Body, 1<<20))
						upstream.Body.Close()
						if readErr != nil {
							decodeErr = readErr
						} else {
							decodeErr = aistudio.CheckRPCResponse("GenerateContent", upstream.StatusCode, raw)
						}
					}
				}
			}
			s.markGenerationPermissionError(runCtx, account, model, decodeErr)
			_ = writer.CloseWithError(decodeErr)
			return
		}
	}()
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: &playgroundBody{ReadCloser: reader, done: cancel}}
	if requestID != "" {
		response.Header.Set("x-goog-request-id", requestID)
		response.Header.Set("x-request-id", requestID)
	}
	return response, nil
}

// A single RPC 7 can be transient or model-specific; it does not establish that
// the account is unusable. Briefly avoid only the actual upstream model while
// preserving the original failure for diagnostics and allowing automatic retry.
func (s *PlaygroundGatewayService) markGenerationPermissionError(ctx context.Context, account *Account, model string, err error) {
	if err == nil || ctx.Err() != nil || account == nil || account.Platform != PlatformGemini || account.Type != AccountTypePlayground || s.accountRepo == nil || strings.TrimSpace(model) == "" {
		return
	}
	var rpcErr *aistudio.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Method != "GenerateContent" || rpcErr.Code != 7 || strings.Contains(strings.ToLower(rpcErr.Message), "region not supported") ||
		(rpcErr.StatusCode != http.StatusForbidden && rpcErr.StatusCode != http.StatusOK) {
		return
	}
	message := fmt.Sprintf("Playground model temporarily unavailable after generation permission denied (model %q): %s", model, rpcErr.Error())
	message = truncateString(logredact.RedactText(message, "api_key", "authorization"), 2048)
	resetAt := time.Now().Add(time.Minute)
	if persistErr := s.accountRepo.SetModelRateLimit(ctx, account.ID, model, resetAt, message); persistErr != nil {
		logger.FromContext(ctx).Warn("playground.permission_cooldown_update_failed", zap.Int64("account_id", account.ID), zap.String("model", model), zap.Error(persistErr))
		return
	}
	logger.FromContext(ctx).Warn("playground.permission_model_cooldown", zap.Int64("account_id", account.ID), zap.String("model", model), zap.Int("upstream_status", rpcErr.StatusCode), zap.Int("rpc_code", rpcErr.Code), zap.Time("reset_at", resetAt), zap.String("reason", message))
}

type playgroundBody struct {
	io.ReadCloser
	done func()
}

func (b *playgroundBody) Close() error { b.done(); return b.ReadCloser.Close() }

// ForwardNative serves Gemini native API requests through AI Studio Playground.
func (s *PlaygroundGatewayService) ForwardNative(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	originalModel string,
	action string,
	stream bool,
	body []byte,
) (*ForwardResult, error) {
	mappedModel := originalModel
	if account.Type == AccountTypePlayground {
		mappedModel = account.GetMappedModel(originalModel)
	}

	startTime := time.Now()
	resp, err := s.DoGemini(ctx, account, mappedModel, body)
	if err != nil {
		return nil, &UpstreamFailoverError{
			StatusCode:   http.StatusServiceUnavailable,
			ResponseBody: []byte(err.Error()),
		}
	}
	defer resp.Body.Close()

	requestID := resp.Header.Get("x-request-id")
	if requestID != "" {
		c.Header("x-request-id", requestID)
	}

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		s.handlePlaygroundError(ctx, account, mappedModel, resp.StatusCode, respBody)
		return nil, &UpstreamFailoverError{
			StatusCode:   resp.StatusCode,
			ResponseBody: respBody,
		}
	}

	if stream {
		return s.streamResponseToClient(c, resp.Body, originalModel, mappedModel, requestID, startTime)
	}
	return s.nonStreamResponseToClient(c, resp.Body, originalModel, mappedModel, requestID, startTime)
}

func (s *PlaygroundGatewayService) handlePlaygroundError(ctx context.Context, account *Account, modelName string, statusCode int, body []byte) {
	decision := aistudio.ResolveQuotaCooldown(statusCode, body)
	if decision.ModelOnly {
		setAccountModelRateLimitSnapshot(account, modelName, decision.ResetAt, decision.Reason, time.Now())
		logger.LegacyPrintf("service.playground_gateway", "[Playground RateLimit] Account %d model %s cooled down until %v (reason=%s)",
			account.ID, modelName, decision.ResetAt, decision.Reason)
	} else {
		_ = s.accountRepo.SetRateLimited(ctx, account.ID, decision.ResetAt)
		logger.LegacyPrintf("service.playground_gateway", "[Playground RateLimit] Account %d rate limited until %v (reason=%s)",
			account.ID, decision.ResetAt, decision.Reason)
	}
}

func (s *PlaygroundGatewayService) streamResponseToClient(
	c *gin.Context,
	body io.Reader,
	originalModel, mappedModel, requestID string,
	startTime time.Time,
) (*ForwardResult, error) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)

	var usage ClaudeUsage
	var firstTokenMs *int
	firstTokenSeen := false
	clientDisconnect := false

	// body is already returning Gemini SSE data: { ... }\n\n
	scanner := io.Reader(body)
	buf := make([]byte, 4096)
	for {
		n, err := scanner.Read(buf)
		if n > 0 {
			if !firstTokenSeen {
				firstTokenSeen = true
				ms := int(time.Since(startTime).Milliseconds())
				firstTokenMs = &ms
			}
			if _, wErr := c.Writer.Write(buf[:n]); wErr != nil {
				clientDisconnect = true
				break
			}
			c.Writer.Flush()
		}
		if err != nil {
			if err != io.EOF {
				return nil, fmt.Errorf("read Playground stream: %w", err)
			}
			break
		}
	}

	return &ForwardResult{
		RequestID:        requestID,
		Usage:            usage,
		Model:            originalModel,
		UpstreamModel:    mappedModel,
		Stream:           true,
		Duration:         time.Since(startTime),
		FirstTokenMs:     firstTokenMs,
		ClientDisconnect: clientDisconnect,
	}, nil
}

func (s *PlaygroundGatewayService) nonStreamResponseToClient(
	c *gin.Context,
	body io.Reader,
	originalModel, mappedModel, requestID string,
	startTime time.Time,
) (*ForwardResult, error) {
	// For non-streaming, collect all SSE data events and send as JSON

	var parts []any
	var finishReason string
	var usageMetadata map[string]any
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("read Playground response: %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var chunk struct {
			Candidates []struct {
				Content struct {
					Parts []any `json:"parts"`
				} `json:"content"`
				FinishReason string `json:"finishReason"`
			} `json:"candidates"`
			UsageMetadata map[string]any `json:"usageMetadata"`
		}
		if err = json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &chunk); err != nil {
			return nil, err
		}
		for _, candidate := range chunk.Candidates {
			parts = append(parts, candidate.Content.Parts...)
			if candidate.FinishReason != "" {
				finishReason = candidate.FinishReason
			}
		}
		if chunk.UsageMetadata != nil {
			usageMetadata = chunk.UsageMetadata
		}
	}
	lastCandidates := []any{map[string]any{"index": 0, "content": map[string]any{"role": "model", "parts": parts}, "finishReason": finishReason}}

	response := map[string]any{
		"candidates": lastCandidates,
	}
	if usageMetadata != nil {
		response["usageMetadata"] = usageMetadata
	}

	c.JSON(http.StatusOK, response)

	var usage ClaudeUsage
	if usageMetadata != nil {
		if pt, ok := usageMetadata["promptTokenCount"].(float64); ok {
			usage.InputTokens = int(pt)
		}
		if ct, ok := usageMetadata["candidatesTokenCount"].(float64); ok {
			usage.OutputTokens = int(ct)
		}
	}

	return &ForwardResult{
		RequestID:     requestID,
		Usage:         usage,
		Model:         originalModel,
		UpstreamModel: mappedModel,
		Stream:        false,
		Duration:      time.Since(startTime),
	}, nil
}

// TestConnectionWithPrompt tests connection and WAA inference for Playground accounts.
func (s *PlaygroundGatewayService) TestConnectionWithPrompt(ctx context.Context, account *Account, model, prompt string) (*TestConnectionResult, error) {
	if strings.TrimSpace(prompt) == "" {
		prompt = "Hi, reply 'OK' if you can read this."
	}
	if strings.TrimSpace(model) == "" {
		model = "gemini-2.5-flash"
	}
	body, err := json.Marshal(antigravity.GeminiRequest{Contents: []antigravity.GeminiContent{{Role: "user", Parts: []antigravity.GeminiPart{{Text: prompt}}}}})
	if err != nil {
		return nil, err
	}
	response, err := s.DoGemini(ctx, account, model, body)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GenerateContent HTTP %d", response.StatusCode)
	}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 16<<20)
	var text strings.Builder
	finishReason := ""
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var chunk antigravity.GeminiResponse
		if err = json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &chunk); err != nil {
			return nil, err
		}
		for _, candidate := range chunk.Candidates {
			if candidate.Content != nil {
				for _, part := range candidate.Content.Parts {
					if !part.Thought {
						text.WriteString(part.Text)
					}
				}
			}
			if candidate.FinishReason != "" {
				finishReason = candidate.FinishReason
			}
		}
	}
	if err = scanner.Err(); err != nil {
		return nil, err
	}
	if (finishReason != "STOP" && finishReason != "MAX_TOKENS") || strings.TrimSpace(text.String()) == "" {
		return nil, fmt.Errorf("connection test did not receive valid final text and successful completion (finish=%q)", finishReason)
	}
	return &TestConnectionResult{Text: text.String(), MappedModel: model}, nil
}
