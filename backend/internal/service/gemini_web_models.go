package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

func (s *GeminiMessagesCompatService) forwardGeminiWebModels(ctx context.Context, account *Account, requested string) (*UpstreamHTTPResult, error) {
	if s.playgroundGatewayService == nil {
		return nil, errors.New("Playground service unavailable")
	}
	names, _, err := s.playgroundGatewayService.Models(ctx, account)
	if err != nil {
		return nil, err
	}
	models := make([]map[string]any, 0, len(names))
	for _, name := range names {
		model := map[string]any{"name": "models/" + name, "displayName": name, "supportedGenerationMethods": []string{"generateContent", "streamGenerateContent", "countTokens"}}
		if requested != "" && strings.TrimPrefix(requested, "models/") == name {
			return geminiModelsJSONResult(http.StatusOK, model)
		}
		models = append(models, model)
	}
	if requested != "" {
		return geminiModelsJSONResult(http.StatusNotFound, map[string]any{"error": map[string]any{"code": 404, "status": "NOT_FOUND", "message": "Gemini Web model unavailable for this account"}})
	}
	return geminiModelsJSONResult(http.StatusOK, map[string]any{"models": models})
}
