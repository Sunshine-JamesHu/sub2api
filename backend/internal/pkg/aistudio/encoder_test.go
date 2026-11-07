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

func TestMixedToolsDefaultServerSideInvocations(t *testing.T) {
	yes, no := true, false
	for _, tt := range []struct {
		name                    string
		search, code, functions bool
		explicit                *bool
		mode                    string
		want                    *bool
	}{
		{name: "search only", search: true},
		{name: "functions only", functions: true},
		{name: "mixed search", search: true, functions: true, want: &yes},
		{name: "mixed code", code: true, functions: true, want: &yes},
		{name: "explicit true", search: true, functions: true, explicit: &yes, want: &yes},
		{name: "explicit false", search: true, functions: true, explicit: &no, want: &no},
		{name: "tools disabled", search: true, functions: true, mode: "NONE"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := &antigravity.GeminiRequest{ToolConfig: &antigravity.GeminiToolConfig{
				IncludeServerSideToolInvocations: tt.explicit,
				FunctionCallingConfig:            &antigravity.GeminiFunctionCallingConfig{Mode: tt.mode},
			}}
			if tt.search {
				req.Tools = append(req.Tools, antigravity.GeminiToolDeclaration{GoogleSearch: &antigravity.GeminiGoogleSearch{}})
			}
			if tt.code {
				req.Tools = append(req.Tools, antigravity.GeminiToolDeclaration{CodeExecution: &antigravity.GeminiCodeExecution{}})
			}
			if tt.functions {
				req.Tools = append(req.Tools, antigravity.GeminiToolDeclaration{FunctionDeclarations: []antigravity.GeminiFunctionDecl{{Name: "read_file"}}})
			}
			raw, err := EncodeGenerateContentRequest("gemini-3.8-flash", req, "proof")
			require.NoError(t, err)
			var wire []any
			require.NoError(t, json.Unmarshal(raw, &wire))
			if tt.want == nil {
				require.Len(t, wire, 11)
			} else {
				require.Equal(t, *tt.want, wire[13].([]any)[2])
			}
		})
	}
}
