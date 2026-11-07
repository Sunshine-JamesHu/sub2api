package aistudio

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
)

func TestGenerateContentLongFunctionNamesMatchHistory(t *testing.T) {
	for _, length := range []int{63, 64, 67, 160} {
		t.Run(strings.Repeat("n", length), func(t *testing.T) {
			name := strings.Repeat("n", length)
			req := &antigravity.GeminiRequest{
				Tools: []antigravity.GeminiToolDeclaration{{FunctionDeclarations: []antigravity.GeminiFunctionDecl{{Name: name}}}},
				Contents: []antigravity.GeminiContent{
					{Role: "model", Parts: []antigravity.GeminiPart{{FunctionCall: &antigravity.GeminiFunctionCall{Name: name, Args: map[string]any{}}}}},
					{Role: "user", Parts: []antigravity.GeminiPart{{FunctionResponse: &antigravity.GeminiFunctionResponse{Name: name, Response: map[string]any{"ok": true}}}}},
				},
			}
			raw, err := EncodeGenerateContentRequest("gemini-4-argon", req, "proof")
			require.NoError(t, err)
			var wire []any
			require.NoError(t, json.Unmarshal(raw, &wire))
			decl := wire[6].([]any)[0].([]any)[1].([]any)[0].([]any)
			alias := decl[0].(string)
			require.LessOrEqual(t, len(alias), 63)
			if length <= 63 {
				require.Equal(t, name, alias)
			} else {
				require.NotEqual(t, name, alias)
			}
			contents := wire[1].([]any)
			call := contents[0].([]any)[0].([]any)[0].([]any)[10].([]any)
			response := contents[1].([]any)[0].([]any)[0].([]any)[11].([]any)
			require.Equal(t, alias, call[0])
			require.Equal(t, alias, response[0])
			require.Equal(t, name, req.Tools[0].FunctionDeclarations[0].Name, "encoding must not mutate the client request")
			again, err := EncodeGenerateContentRequest("gemini-4-argon", req, "proof")
			require.NoError(t, err)
			require.Equal(t, raw, again, "aliases must survive retries and follow-up requests")
		})
	}
}

func TestFunctionNameAliasesSeparateSimilarNamesAndRestoreEvents(t *testing.T) {
	first := strings.Repeat("mcp_", 20) + "first"
	second := strings.Repeat("mcp_", 20) + "second"
	require.NotEqual(t, wireFunctionName(first), wireFunctionName(second))
	for _, historyOnly := range []bool{false, true} {
		req := &antigravity.GeminiRequest{Tools: []antigravity.GeminiToolDeclaration{{FunctionDeclarations: []antigravity.GeminiFunctionDecl{{Name: first}, {Name: second}}}}}
		if historyOnly {
			req.Tools = nil
			req.Contents = []antigravity.GeminiContent{{Parts: []antigravity.GeminiPart{{FunctionCall: &antigravity.GeminiFunctionCall{Name: first}}, {FunctionResponse: &antigravity.GeminiFunctionResponse{Name: second}}}}}
		}
		for _, name := range []string{first, second, "short", "unknown"} {
			original := &antigravity.GeminiFunctionCall{Name: wireFunctionName(name), ID: "call_1", Args: map[string]any{"input": "OK"}}
			wantErr := errors.New("client disconnected")
			emit := RestoreFunctionNames(req, func(event StreamEvent) error {
				require.Equal(t, name, event.FunctionCall.Name)
				require.Equal(t, original.ID, event.FunctionCall.ID)
				require.Equal(t, original.Args, event.FunctionCall.Args)
				require.Equal(t, "signature", event.ThoughtSignature)
				return wantErr
			})
			require.ErrorIs(t, emit(StreamEvent{FunctionCall: original, ThoughtSignature: "signature"}), wantErr)
			require.Equal(t, wireFunctionName(name), original.Name, "restoring must not mutate decoder-owned calls")
		}
	}
}
