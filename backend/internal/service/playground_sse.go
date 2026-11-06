package service

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// collectPlaygroundSSE keeps every part (including thoughts and tool calls) and
// reads through EOF so RPC trailer and truncation errors cannot become success.
func collectPlaygroundSSE(body io.Reader, observe func([]byte)) (map[string]any, *ClaudeUsage, geminiSSECollectStats, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), 16<<20)
	stats := geminiSSECollectStats{fallback: &geminiSSEFallbackBody{}}
	parts := []any{}
	finish := ""
	usage := &ClaudeUsage{}
	result := map[string]any{}
	for scanner.Scan() {
		if !strings.HasPrefix(scanner.Text(), "data:") {
			continue
		}
		raw := []byte(strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "data:")))
		var chunk map[string]any
		if err := json.Unmarshal(raw, &chunk); err != nil {
			return nil, nil, stats, fmt.Errorf("decode Playground SSE: %w", err)
		}
		stats.dataEvents++
		if observe != nil {
			observe(raw)
		}
		parts = append(parts, mapsToAny(extractGeminiParts(chunk))...)
		if candidates, ok := chunk["candidates"].([]any); ok {
			for _, candidate := range candidates {
				if value, ok := candidate.(map[string]any); ok {
					if value, ok := value["finishReason"].(string); ok && value != "" {
						finish = value
					}
				}
			}
		}
		if metadata, ok := chunk["usageMetadata"]; ok {
			result["usageMetadata"] = metadata
			if next := extractGeminiUsage(raw); next != nil {
				usage = next
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, stats, err
	}
	if finish == "" {
		return nil, nil, stats, errors.New("Playground stream has no finish frame")
	}
	result["candidates"] = []any{map[string]any{"index": 0, "content": map[string]any{"role": "model", "parts": parts}, "finishReason": finish}}
	return result, usage, stats, nil
}

func mapsToAny(parts []map[string]any) []any {
	result := make([]any, len(parts))
	for i, part := range parts {
		result[i] = part
	}
	return result
}
