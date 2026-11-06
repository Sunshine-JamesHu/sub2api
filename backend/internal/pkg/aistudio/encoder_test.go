package aistudio

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
)

func TestEncodeGenerateContentRequest(t *testing.T) {
	req := &antigravity.GeminiRequest{
		Contents: []antigravity.GeminiContent{
			{
				Role: "user",
				Parts: []antigravity.GeminiPart{
					{Text: "Hello Gemini"},
				},
			},
		},
		GenerationConfig: &antigravity.GeminiGenerationConfig{
			MaxOutputTokens: 1024,
		},
	}

	proof := "!AOtestproof12345"
	raw, err := EncodeGenerateContentRequest("gemini-2.5-flash", req, proof)
	require.NoError(t, err)

	var arr []any
	err = json.Unmarshal(raw, &arr)
	require.NoError(t, err)

	require.GreaterOrEqual(t, len(arr), 11)
	require.Equal(t, "models/gemini-2.5-flash", arr[0])
	require.Equal(t, proof, arr[4]) // Field 5 is index 4!
	require.Equal(t, float64(1), arr[10])
}

func TestExtractPromptForBinding(t *testing.T) {
	req := &antigravity.GeminiRequest{
		Contents: []antigravity.GeminiContent{
			{
				Role: "user",
				Parts: []antigravity.GeminiPart{
					{Text: "Translate this text"},
				},
			},
		},
	}
	prompt := ExtractPromptForBinding(req)
	require.Equal(t, "Translate this text", prompt)
	digest := ComputePayloadDigest(prompt)
	require.NotEmpty(t, digest)
}

func TestThinkingLevelDoesNotSendDefaultBudget(t *testing.T) {
	for _, budget := range []int{0, 1024} {
		req := &antigravity.GeminiRequest{
			Contents:         []antigravity.GeminiContent{{Role: "user", Parts: []antigravity.GeminiPart{{Text: "hello"}}}},
			GenerationConfig: &antigravity.GeminiGenerationConfig{ThinkingConfig: &antigravity.GeminiThinkingConfig{ThinkingLevel: "low", ThinkingBudget: budget}},
		}
		raw, err := EncodeGenerateContentRequest("gemini-3.8-flash", req, "proof")
		require.NoError(t, err)
		var wire []any
		require.NoError(t, json.Unmarshal(raw, &wire))
		thinking := wire[3].([]any)[16].([]any)
		require.Equal(t, []any{float64(1), nil, nil, float64(1)}, thinking)
	}
}
