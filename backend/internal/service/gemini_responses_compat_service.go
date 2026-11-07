package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
)

// ForwardAsResponses serves Codex and other Responses clients through Gemini,
// including Playground accounts, using the same upstream path as Chat Completions.
func (s *GeminiMessagesCompatService) ForwardAsResponses(ctx context.Context, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
	start := time.Now()
	invalid := func(err error) (*ForwardResult, error) {
		writeResponsesError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, err
	}
	normalized, _, err := normalizeOpenAIResponsesLegacyIngressWithConfig(s.cfg, body)
	if err != nil {
		return invalid(err)
	}
	adapted, mapping, err := adaptResponsesClientToolsForAnthropic(normalized)
	if err != nil {
		return invalid(err)
	}
	var req apicompat.ResponsesRequest
	if err := json.Unmarshal(adapted, &req); err != nil {
		return invalid(errors.New("Failed to parse request body"))
	}
	if strings.TrimSpace(req.Model) == "" {
		return invalid(errors.New("model is required"))
	}
	claudeReq, err := apicompat.ResponsesToAnthropicRequest(&req)
	if err != nil {
		return invalid(err)
	}
	claudeBody, err := json.Marshal(claudeReq)
	if err != nil {
		return nil, fmt.Errorf("marshal gemini responses request: %w", err)
	}
	return s.forwardClaudeBodyAsOpenAI(ctx, c, account, claudeBody, req.Model, req.Stream, false, start, &mapping)
}

func (s *GeminiMessagesCompatService) handleResponsesNonStreamingResponseFromGemini(
	c *gin.Context, resp *http.Response, model string, isOAuth, upstreamStream bool, mapping apicompat.ResponsesClientToolMapping,
) (*ClaudeUsage, error) {
	var collected map[string]any
	var usage *ClaudeUsage
	var raw []byte
	var err error
	if upstreamStream {
		collected, usage, err = collectGeminiSSE(resp.Body, isOAuth)
		if err == nil && extractGeminiFinishReason(collected) == "" {
			err = errors.New("upstream stream ended without a finish reason")
		}
		if err == nil {
			raw, err = json.Marshal(collected)
		}
	} else {
		raw, err = ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
		if err == nil && isOAuth {
			raw = unwrapIfNeeded(true, raw)
		}
		if err == nil {
			err = json.Unmarshal(raw, &collected)
		}
	}
	if err != nil {
		if !c.Writer.Written() {
			writeResponsesError(c, http.StatusBadGateway, "upstream_error", "Failed to read upstream response")
		}
		return nil, err
	}
	result, usage, err := geminiResponseToResponses(collected, model, raw, usage)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	payload, _, err = apicompat.RestoreResponsesClientToolPayload(payload, mapping)
	if err != nil {
		return nil, err
	}
	responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	c.Writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	c.Data(http.StatusOK, "application/json; charset=utf-8", payload)
	return usage, nil
}

func writeGeminiResponsesStreamError(c *gin.Context, state *apicompat.AnthropicEventToResponsesState, message string) {
	payload, _ := json.Marshal(apicompat.ResponsesStreamEvent{
		Type: "response.failed", SequenceNumber: state.SequenceNumber,
		Response: &apicompat.ResponsesResponse{
			ID: state.ResponseID, Object: "response", CreatedAt: state.Created, Model: state.Model,
			Status: "failed", Output: []apicompat.ResponsesOutput{},
			Error: &apicompat.ResponsesError{Code: "upstream_error", Message: message},
		},
	})
	_, _ = fmt.Fprintf(c.Writer, "event: response.failed\ndata: %s\n\n", payload)
	c.Writer.Flush()
	MarkOpsStreamError(c, "upstream_error", message, http.StatusBadGateway)
}
