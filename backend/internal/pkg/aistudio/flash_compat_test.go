package aistudio

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
)

func TestMakerSuiteFlashThinkingCompatibility(t *testing.T) {
	for _, tt := range []struct {
		model, level string
		want         float64
	}{
		{"gemini-3.8-flash", "minimal", 1},
		{"models/gemini-3.8-flash", "MINIMAL", 1},
		{"gemini-3.8-flash", "low", 1},
		{"gemini-3.8-flash", "medium", 2},
		{"gemini-3.8-flash", "high", 3},
		{"gemini-3-flash-preview", "minimal", 4},
		{"gemini-4-argon", "minimal", 4},
	} {
		t.Run(tt.model+"/"+tt.level, func(t *testing.T) {
			cfg := &antigravity.GeminiThinkingConfig{ThinkingLevel: tt.level, ThinkingBudget: 2048}
			req := &antigravity.GeminiRequest{GenerationConfig: &antigravity.GeminiGenerationConfig{ThinkingConfig: cfg}}
			raw, err := EncodeGenerateContentRequest(tt.model, req, "proof")
			require.NoError(t, err)
			var wire []any
			require.NoError(t, json.Unmarshal(raw, &wire))
			require.Equal(t, []any{float64(1), nil, nil, tt.want}, wire[3].([]any)[16])
			require.Equal(t, tt.level, cfg.ThinkingLevel)
			require.Equal(t, 2048, cfg.ThinkingBudget)
		})
	}
}

func TestSchemaEncryptionAnnotationPreservesConstraintsAndPropertyNames(t *testing.T) {
	for _, encrypted := range []string{"true", "false"} {
		raw := json.RawMessage(`{"type":"object","properties":{"message":{"type":"string","encrypted":` + encrypted + `,"minLength":1},"encrypted":{"type":"boolean"},"items":{"type":"array","items":{"type":"string","encrypted":` + encrypted + `}}},"required":["message","encrypted"]}`)
		schema, err := geminiSchema(raw)
		require.NoError(t, err)
		props := schema["properties"].(map[string]any)
		require.Equal(t, "BOOLEAN", props["encrypted"].(map[string]any)["type"])
		require.Equal(t, int64(1), props["message"].(map[string]any)["minLength"])
		require.NotContains(t, props["message"], "encrypted")
		require.NotContains(t, props["items"].(map[string]any)["items"], "encrypted")
		require.Equal(t, []string{"message", "encrypted"}, schema["required"])
		require.Contains(t, string(raw), `"encrypted":`+encrypted)
	}
	_, err := geminiSchema(json.RawMessage(`{"type":"string","encrypted":"yes"}`))
	require.ErrorContains(t, err, "schema.encrypted")
	_, err = geminiSchema(json.RawMessage(`{"type":"string","unrecognized_constraint":true}`))
	require.ErrorContains(t, err, "unrecognized_constraint")
}
